/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// /catalog/packs — ONE searchable index over everything an agent can be
// composed from: skills (Skill CRs), tools (MCPServerRegistrations),
// and knowledge bases (KnowledgeBase CRs).
//
// This is the mindifact-shaped view of the catalog: every entry is a
// typed "pack" with name/version/description/requires, and the composer
// UI is a single search box over it instead of per-kind tabs. The field
// vocabulary deliberately matches the pack manifest convention
// (name, version, description, requires, provides) so a catalog entry
// is structurally a pack — publishable to any registry that speaks the
// same shape — without any registry branding living in the platform.
//
//	GET /catalog/packs?query=<substring>&type=skill,tool,kb
//	  → {items: [catalogPack], count}
//
// Search is a case-insensitive substring match over name, displayName
// and description — the same "one input filters typed rows" model the
// registry site uses. No pagination: the whole catalog is a few dozen
// entries and the composer filters client-side as the user types; the
// server-side query exists for the MCP discovery tools.
//
// Tools carry a client `recipe` — the exact mcpServers entry an
// AgentWorkstation needs to consume the registration. The recipe is
// data, not code: it comes from the registration's
// `agentoffice.ai/client-recipe` annotation (set in gitops next to the
// registration itself), falling back to a bare shared-gateway entry.
// This is what lets the composer drop its hardcoded per-tool presets.
type catalogPack struct {
	Type        string `json:"type"` // skill | tool | kb
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	Tier        string `json:"tier,omitempty"`
	// Requires — skills only: OpenClaw runtime capabilities.
	Requires []string `json:"requires,omitempty"`
	// Dependencies — skills only: platform resources, live-enriched.
	Dependencies []catalogDependency `json:"dependencies,omitempty"`
	// Recipe — tools only: the mcpServers entry that consumes it.
	Recipe *toolRecipe `json:"recipe,omitempty"`
	// GatewayRef — knowledge bases only.
	GatewayRef string `json:"gatewayRef,omitempty"`

	// ---- registry provenance (federated artifacts only) -------------
	// Installed distinguishes what exists on THIS cluster from what a
	// registry merely publishes. Local packs are always true.
	Installed bool `json:"installed"`
	// Registry/Namespace/ArtifactKind are the artifact's coordinates —
	// <registry> <namespace>/<name>:<version>, kind meta-pack|pack|skill.
	Registry     string `json:"registry,omitempty"`
	Namespace    string `json:"namespace,omitempty"`
	ArtifactKind string `json:"artifactKind,omitempty"`
	// Member — which pack a federated skill belongs to.
	Member string `json:"member,omitempty"`
	// Members — the child packs of a meta-pack, straight from the
	// registry manifest. The registry has always published this; the
	// operator used to read it only to decorate the description, so a
	// client could see that parkforge-brain HAD five member packs but
	// never which. The composer needs the actual tree to show what
	// selecting a parent pulls in.
	Members []string `json:"members,omitempty"`
	// Skills — the leaf skills a pack or meta-pack ships, by short
	// name. Advisory: the authoritative parent→child edge is Member on
	// each skill row.
	Skills []string `json:"skills,omitempty"`
	// Manifest/ContentURL/OCI are where an installer fetches from.
	Manifest   string `json:"manifest,omitempty"`
	ContentURL string `json:"contentUrl,omitempty"`
	OCI        string `json:"oci,omitempty"`
}

// toolRecipe mirrors the AgentWorkstation spec.tools.mcpServers entry
// shape the binder writes (url/type/authHeaderValue/envFromSecret).
type toolRecipe struct {
	URL             string `json:"url"`
	Type            string `json:"type"`
	AuthHeaderValue string `json:"authHeaderValue,omitempty"`
	EnvFromSecret   string `json:"envFromSecret,omitempty"`
}

// sharedGatewayURL is the single MCP gateway endpoint every
// registration is served behind; per-registration credentials are
// injected gateway-side on the tool-call path.
func sharedGatewayURL(ns string) string {
	return fmt.Sprintf(
		"http://mcp-gateway-data-science-gateway-class.%s.svc.cluster.local/mcp", ns)
}

func (h *CatalogSkillsHandler) listPacks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	typeFilter := map[string]bool{}
	if tf := strings.TrimSpace(r.URL.Query().Get("type")); tf != "" {
		for _, t := range strings.Split(tf, ",") {
			typeFilter[strings.TrimSpace(t)] = true
		}
	}
	items, err := h.gatherPacks(ctx, typeFilter)
	if err != nil {
		writeCatalogJSONError(w, http.StatusInternalServerError, err)
		return
	}

	if q != "" {
		filtered := items[:0]
		for _, p := range items {
			hay := strings.ToLower(p.Name + " " + p.DisplayName + " " + p.Description)
			if strings.Contains(hay, q) {
				filtered = append(filtered, p)
			}
		}
		items = filtered
	}

	writeCatalogJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"count": len(items),
	})
}

// gatherPacks assembles the full typed catalog (already sorted
// deterministically). typeFilter empty ⇒ all types.
func (h *CatalogSkillsHandler) gatherPacks(ctx context.Context, typeFilter map[string]bool) ([]catalogPack, error) {
	wantType := func(t string) bool { return len(typeFilter) == 0 || typeFilter[t] }
	items := make([]catalogPack, 0, 32)

	if wantType("skill") {
		var skills agentofficev1alpha1.SkillList
		if err := h.client.List(ctx, &skills, client.InNamespace(h.namespace)); err != nil {
			return nil, fmt.Errorf("listing skills: %w", err)
		}
		for i := range skills.Items {
			sk := &skills.Items[i]
			e := h.toCatalogEntry(ctx, sk)
			p := catalogPack{
				Type: "skill", Name: e.Name, DisplayName: e.DisplayName,
				Description: e.Description, Version: e.Version, Tier: e.Tier,
				Requires: e.Requires, Dependencies: e.Dependencies,
				Installed: true, ArtifactKind: "skill",
			}
			// Read back the provenance installSkill wrote. Without this a
			// skill DROPS OUT OF ITS PACK the moment it is installed: the
			// CR keeps the label, the catalog forgot to look, and the
			// parent showed zero children while plainly owning one.
			if sk.Labels != nil {
				p.Member = sk.Labels["agentoffice.ai/pack"]
			}
			if sk.Annotations != nil {
				p.Registry = sk.Annotations["agentoffice.ai/registry"]
			}
			items = append(items, p)
		}
	}

	if wantType("tool") {
		regs := &unstructured.UnstructuredList{}
		regs.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "mcp.kuadrant.io", Version: "v1alpha1",
			Kind: "MCPServerRegistrationList",
		})
		// A cluster without the kuadrant CRD still has a catalog —
		// tools are simply absent, same degradation as enrichment.
		if err := h.client.List(ctx, regs, client.InNamespace(h.namespace)); err == nil {
			for i := range regs.Items {
				reg := &regs.Items[i]
				ann := reg.GetAnnotations()
				recipe := &toolRecipe{URL: sharedGatewayURL(h.namespace), Type: "http"}
				if raw := ann["agentoffice.ai/client-recipe"]; raw != "" {
					var parsed toolRecipe
					if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed.URL != "" {
						recipe = &parsed
					}
				}
				displayName := ann["agentoffice.ai/display-name"]
				if displayName == "" {
					displayName = reg.GetName()
				}
				desc := ann["agentoffice.ai/description"]
				if desc == "" {
					desc = fmt.Sprintf(
						"Governed tools registered behind the MCP gateway (%s). Credentials are injected gateway-side per call.",
						reg.GetName())
				}
				items = append(items, catalogPack{
					Type: "tool", Name: reg.GetName(),
					DisplayName: displayName, Description: desc, Recipe: recipe,
					Installed: true,
				})
			}
		}
	}

	if wantType("kb") {
		var kbs agentofficev1alpha1.KnowledgeBaseList
		if err := h.client.List(ctx, &kbs, client.InNamespace(h.namespace)); err == nil {
			for i := range kbs.Items {
				kb := &kbs.Items[i]
				items = append(items, catalogPack{
					Type: "kb", Name: kb.Name,
					DisplayName: kb.Spec.DisplayName,
					Description: kb.Spec.Description,
					GatewayRef:  kb.Spec.GatewayRef.Name,
					Installed:   true,
				})
			}
		}
	}

	// Federated registry artifacts — searchable BEFORE installation,
	// which is the whole point of a registry. A name already present
	// locally wins: the installed copy is the truth for this cluster.
	if wantType("skill") {
		local := map[string]bool{}
		for _, p := range items {
			local[p.Name] = true
		}
		for _, rp := range registryPacks(ctx) {
			if !local[rp.Name] {
				items = append(items, rp)
			}
		}
	}

	// Deterministic: skills, tools, kbs; name asc within type.
	order := map[string]int{"skill": 0, "tool": 1, "kb": 2}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Installed != items[j].Installed {
			return items[i].Installed // installed first
		}
		if order[items[i].Type] != order[items[j].Type] {
			return order[items[i].Type] < order[items[j].Type]
		}
		return items[i].Name < items[j].Name
	})
	return items, nil
}
