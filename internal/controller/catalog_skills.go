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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// CatalogSkillsHandler implements net/http.Handler. It serves the
// runtime skill-discovery catalog the v1.6.0 architecture introduces.
//
// Two routes are recognized (both GET, both return JSON):
//
//	GET /catalog/skills
//	     Query params: query (substring match against name/description),
//	                   tier (e.g. rh-official, anthropic-official),
//	                   limit (default 100).
//	     Response: {items: [{name, displayName, description, version,
//	                         tier, sourceRepo, sourceRevision,
//	                         contentSha256}]}
//	     The metadata-only listing the binders plugin + MCP
//	     `skill_discover` tool consume.
//
//	GET /catalog/skills/<name>
//	     Response: full skill metadata + SKILL.md body resolved from
//	              spec.source (inline | configMapRef).
//	     The MCP `skill_load` tool consumes this — the agent has
//	     decided which skill it wants and pulls the full body into
//	     context.
//
// Tier comes from the standard label `agentoffice.ai/skill-tier`
// (applied by the importer pipeline as an OCI annotation; for
// imported skills the importer creates a corresponding Skill CR
// with that label set). Locally-authored Skill CRs without the
// label get tier "customer-authored" by default.
//
// Source-repo / source-revision come from labels with the same
// `agentoffice.ai/source-repo` / `agentoffice.ai/source-revision`
// keys the importer writes.
//
// Access control: today the endpoint serves every Skill in the
// agent-office namespace to any caller (in-cluster service-mesh
// gates network access). When `spec.permitted.skills` filtering
// is added to AgentWorkstation (deferred per the v1.6.0 plan),
// this handler grows an `?agent=<aw>` filter that enforces the
// allowlist.
//
// All routes return JSON. Errors:
//
//	400 — malformed path / bad query
//	404 — skill not found (by-name route)
//	500 — backing K8s API or ConfigMap resolve error
type CatalogSkillsHandler struct {
	client    client.Client
	namespace string // agent-office; could be made configurable later
}

// NewCatalogSkillsHandler returns a handler ready to register against
// a mux. The namespace defaults to "agent-office" and matches the
// namespace the BindersHandler reads from.
func NewCatalogSkillsHandler(c client.Client) *CatalogSkillsHandler {
	return &CatalogSkillsHandler{
		client:    c,
		namespace: "agent-office",
	}
}

// skillCatalogEntry is the metadata-only shape returned by the
// list endpoint. Trimmed for token-efficiency — the full SKILL.md
// body is fetched separately via the by-name endpoint.
type skillCatalogEntry struct {
	Name           string   `json:"name"`
	DisplayName    string   `json:"displayName,omitempty"`
	Description    string   `json:"description,omitempty"`
	Version        string   `json:"version,omitempty"`
	Tier           string   `json:"tier,omitempty"`
	SourceRepo     string   `json:"sourceRepo,omitempty"`
	SourceRevision string   `json:"sourceRevision,omitempty"`
	ContentSHA256  string   `json:"contentSha256,omitempty"`
	Tool           string   `json:"tool,omitempty"`
	Requires       []string `json:"requires,omitempty"`
	// Dependencies mirrors spec.dependencies enriched with live
	// availability, so a composer can preselect what exists and
	// name exactly what is missing instead of guessing.
	Dependencies []catalogDependency `json:"dependencies,omitempty"`
}

// catalogDependency is a SkillDependency enriched at serve time.
type catalogDependency struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Optional bool   `json:"optional,omitempty"`
	// Available reports whether the depended-on resource exists in
	// the catalog namespace right now (mcpServer -> an
	// MCPServerRegistration; knowledgeBase -> a KnowledgeBase CR).
	Available bool `json:"available"`
	// GatewayURL is set for available mcpServer deps: the shared
	// MCP gateway endpoint an AgentWorkstation entry should point
	// at. Per-registration credentials are injected gateway-side
	// on the tool-call path, so ONE gateway entry serves every
	// registration — composers should dedupe on this URL.
	GatewayURL string `json:"gatewayUrl,omitempty"`
}

// skillCatalogDetail is the full shape returned by the by-name
// endpoint — adds the resolved SKILL.md body and the invocation
// prompt template (if any).
type skillCatalogDetail struct {
	skillCatalogEntry
	SkillMd        string `json:"skillMd"`
	PromptTemplate string `json:"promptTemplate,omitempty"`
}

func (h *CatalogSkillsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")

	// Patterns recognized:
	//   /catalog/skills            → 2 parts (GET)
	//   /catalog/packs             → 2 parts (GET, unified typed search)
	//   /catalog/recommend         → 2 parts (POST, one-step composer brain)
	//   /catalog/skills/<name>     → 3 parts (GET)
	if len(parts) == 2 && parts[0] == "catalog" && parts[1] == "recommend" {
		h.recommend(w, r)
		return
	}
	if len(parts) == 2 && parts[0] == "catalog" && parts[1] == "install" {
		h.install(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeCatalogJSONError(w, http.StatusMethodNotAllowed,
			fmt.Errorf("only GET is supported"))
		return
	}
	switch len(parts) {
	case 2:
		if parts[0] != "catalog" || (parts[1] != "skills" && parts[1] != "packs") {
			writeCatalogJSONError(w, http.StatusBadRequest,
				fmt.Errorf("unknown path %q", r.URL.Path))
			return
		}
		if parts[1] == "packs" {
			h.listPacks(w, r)
			return
		}
		h.listSkills(w, r)
	case 3:
		if parts[0] != "catalog" || parts[1] != "skills" {
			writeCatalogJSONError(w, http.StatusBadRequest,
				fmt.Errorf("unknown path %q", r.URL.Path))
			return
		}
		h.getSkill(w, r, parts[2])
	default:
		writeCatalogJSONError(w, http.StatusBadRequest,
			fmt.Errorf("unknown path %q", r.URL.Path))
	}
}

// listSkills serves GET /catalog/skills with optional query/tier
// filters. Reads from the controller-runtime cache (informer-backed),
// so it's cheap to call frequently.
func (h *CatalogSkillsHandler) listSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	tierFilter := strings.TrimSpace(r.URL.Query().Get("tier"))

	var skills agentofficev1alpha1.SkillList
	if err := h.client.List(ctx, &skills, client.InNamespace(h.namespace)); err != nil {
		writeCatalogJSONError(w, http.StatusInternalServerError,
			fmt.Errorf("listing skills: %w", err))
		return
	}

	items := make([]skillCatalogEntry, 0, len(skills.Items))
	for i := range skills.Items {
		s := &skills.Items[i]
		entry := h.toCatalogEntry(ctx, s)

		if tierFilter != "" && entry.Tier != tierFilter {
			continue
		}
		if q != "" {
			haystack := strings.ToLower(entry.Name + " " +
				entry.DisplayName + " " + entry.Description)
			if !strings.Contains(haystack, q) {
				continue
			}
		}

		items = append(items, entry)
	}

	// Deterministic order — name asc. Makes the response cache-friendly
	// (same content every time → same ETag/hash) and easier to diff
	// across calls.
	sort.Slice(items, func(i, j int) bool {
		return items[i].Name < items[j].Name
	})

	writeCatalogJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"count": len(items),
	})
}

// getSkill serves GET /catalog/skills/<name>, returning the full
// SKILL.md body alongside the metadata.
func (h *CatalogSkillsHandler) getSkill(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()

	var s agentofficev1alpha1.Skill
	if err := h.client.Get(ctx, types.NamespacedName{
		Namespace: h.namespace,
		Name:      name,
	}, &s); err != nil {
		// Distinguish "not found" from other errors so the MCP
		// `skill_load` tool can surface a clean message.
		if client.IgnoreNotFound(err) == nil {
			writeCatalogJSONError(w, http.StatusNotFound,
				fmt.Errorf("skill %q not found", name))
			return
		}
		writeCatalogJSONError(w, http.StatusInternalServerError,
			fmt.Errorf("getting skill %q: %w", name, err))
		return
	}

	body, err := h.resolveSkillMd(ctx, &s)
	if err != nil {
		writeCatalogJSONError(w, http.StatusInternalServerError,
			fmt.Errorf("resolving skill body: %w", err))
		return
	}

	entry := h.toCatalogEntry(ctx, &s)
	// ContentSHA256 is only meaningful once the body is resolved;
	// compute fresh here so it matches the body we're returning.
	sum := sha256.Sum256([]byte(body))
	entry.ContentSHA256 = hex.EncodeToString(sum[:])

	writeCatalogJSON(w, http.StatusOK, skillCatalogDetail{
		skillCatalogEntry: entry,
		SkillMd:           body,
		PromptTemplate:    s.Spec.Invocation.PromptTemplate,
	})
}

// toCatalogEntry projects a Skill CR onto the trimmed catalog
// metadata shape. Reads tier/source provenance from the standard
// labels the importer pipeline writes.
func (h *CatalogSkillsHandler) toCatalogEntry(ctx context.Context, s *agentofficev1alpha1.Skill) skillCatalogEntry {
	tier := s.Labels["agentoffice.ai/skill-tier"]
	if tier == "" {
		// Skills authored locally (no importer involvement) default
		// to customer-authored. Lets binders plugin filter "show me
		// just the ones I wrote" vs "everything from upstream".
		tier = "customer-authored"
	}

	displayName := s.Spec.DisplayName
	if displayName == "" {
		displayName = s.Name
	}

	return skillCatalogEntry{
		Name:           s.Name,
		DisplayName:    displayName,
		Description:    s.Spec.Description,
		Version:        s.Spec.Version,
		Tier:           tier,
		SourceRepo:     s.Labels["agentoffice.ai/source-repo"],
		SourceRevision: s.Labels["agentoffice.ai/source-revision"],
		ContentSHA256:  s.Status.ContentSHA256,
		Tool:           s.Spec.Invocation.Tool,
		Requires:       s.Spec.Requires,
		Dependencies:   h.enrichDependencies(ctx, s.Spec.Dependencies),
	}
}

// enrichDependencies resolves each declared dependency against live
// cluster state. Failures to look up are reported as unavailable
// rather than failing the catalog request — the catalog must stay
// readable even if the kuadrant CRD is absent on a cluster.
// (RBAC marker for the mcp.kuadrant.io read lives with the Skill
// controller's markers in skill_controller.go.)
func (h *CatalogSkillsHandler) enrichDependencies(ctx context.Context, deps []agentofficev1alpha1.SkillDependency) []catalogDependency {
	if len(deps) == 0 {
		return nil
	}
	out := make([]catalogDependency, 0, len(deps))
	for _, d := range deps {
		cd := catalogDependency{Kind: d.Kind, Name: d.Name, Optional: d.Optional}
		switch d.Kind {
		case "mcpServer":
			reg := &unstructured.Unstructured{}
			reg.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "mcp.kuadrant.io", Version: "v1alpha1", Kind: "MCPServerRegistration",
			})
			if err := h.client.Get(ctx, types.NamespacedName{
				Namespace: h.namespace, Name: d.Name,
			}, reg); err == nil {
				cd.Available = true
				// One shared gateway endpoint serves every registration;
				// per-registration credentials are injected gateway-side
				// on the tool-call path (Kuadrant model — the same URL
				// the binder's presets use).
				cd.GatewayURL = sharedGatewayURL(h.namespace)
			}
		case "knowledgeBase":
			var kb agentofficev1alpha1.KnowledgeBase
			if err := h.client.Get(ctx, types.NamespacedName{
				Namespace: h.namespace, Name: d.Name,
			}, &kb); err == nil {
				cd.Available = true
			}
		}
		out = append(out, cd)
	}
	return out
}

// resolveSkillMd dereferences spec.source — either inline body or
// ConfigMap data[key]. Mirrors the resolution logic the Skill
// reconciler does on every reconcile; we re-resolve here so the
// catalog never serves stale content (informer cache could lag
// behind a fresh ConfigMap update by a few seconds).
//
// Future: when an Artifact/Skill CR can reference an OCI image
// for its body (spec.source.oci added later in v1.6.x), this
// function gains a third branch that pulls the OCI manifest +
// extracts the SKILL.md layer.
func (h *CatalogSkillsHandler) resolveSkillMd(ctx context.Context, s *agentofficev1alpha1.Skill) (string, error) {
	if s.Spec.Source.Inline != "" {
		return s.Spec.Source.Inline, nil
	}
	if s.Spec.Source.ConfigMapRef != nil {
		var cm corev1.ConfigMap
		if err := h.client.Get(ctx, types.NamespacedName{
			Namespace: h.namespace,
			Name:      s.Spec.Source.ConfigMapRef.Name,
		}, &cm); err != nil {
			return "", fmt.Errorf("getting configmap %q: %w",
				s.Spec.Source.ConfigMapRef.Name, err)
		}
		body, ok := cm.Data[s.Spec.Source.ConfigMapRef.Key]
		if !ok {
			return "", fmt.Errorf("configmap %q has no key %q",
				s.Spec.Source.ConfigMapRef.Name,
				s.Spec.Source.ConfigMapRef.Key)
		}
		return body, nil
	}
	return "", fmt.Errorf("skill %q has neither inline nor configMapRef source", s.Name)
}

// writeCatalogJSON is a tiny helper local to this file. Named with the
// "Catalog" prefix to avoid colliding with backstage_catalog.go's
// writeJSON (different signature — that one always writes 200; this
// one needs to write 404/500 as well).
func writeCatalogJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeCatalogJSONError(w http.ResponseWriter, status int, err error) {
	writeCatalogJSON(w, status, map[string]string{"error": err.Error()})
}
