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
	"io"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// POST /catalog/install — materialize a registry artifact on this cluster.
//
//	{"name": "parkforge-terrain-strokes"}            # a skill
//	{"name": "parkforge-terrain"}                    # a pack: all its skills
//	{"name": "parkforge-brain"}                      # a meta-pack: everything
//	→ {"installed": [...], "skipped": [...], "count": n}
//
// This is `mindifact install` translated to a cluster. The CLI unpacks a
// pack into a workspace and overlays it — brain/{contracts,traps,verbs},
// per-pack manifests, and skills/<name>/ into .claude/skills/<name>/.
// The last of those is the part that means anything to an agent here:
// on OpenShift a skill is discovered from a Skill CR, not a file in
// someone's home directory. So installing a mindifact = creating the
// Skill CRs it ships, with the pack's provenance recorded on them.
//
// The rest of a pack's compartments (verbs, traps, factory, evals) are
// workstation artifacts — Python that drives a local editor. They are
// deliberately NOT installed: an agent on this cluster cannot execute
// them, and materializing them would create the illusion that it can.
// The skill body still documents them, and spec.dependencies records
// what the skill needs, so the composer says "not deployed" instead of
// pretending.
//
// Idempotent: installing twice updates in place. Install is also how a
// federated pack stops being federated — once the CR exists,
// /catalog/packs serves the local copy (installed:true) and the remote
// row disappears, because the cluster's copy is the truth for the
// cluster.
type installRequest struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

func (h *CatalogSkillsHandler) install(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeCatalogJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("install is POST"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		writeCatalogJSONError(w, http.StatusBadRequest, err)
		return
	}
	var req installRequest
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeCatalogJSONError(w, http.StatusBadRequest,
			fmt.Errorf(`body must be {"name": "<artifact>"}`))
		return
	}

	remote := registryPacks(r.Context())
	if len(remote) == 0 {
		writeCatalogJSONError(w, http.StatusServiceUnavailable,
			fmt.Errorf("no registry configured or reachable (AGENT_REGISTRY_URLS)"))
		return
	}

	target := resolveInstallSet(remote, req.Name)
	if len(target) == 0 {
		writeCatalogJSONError(w, http.StatusNotFound,
			fmt.Errorf("no installable artifact found for %q", req.Name))
		return
	}

	installed, skipped := []string{}, []string{}
	for _, p := range target {
		var err error
		if p.ArtifactKind == "tool" {
			err = h.installTool(r.Context(), p)
		} else {
			err = h.installSkill(r.Context(), p)
		}
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", p.Name, err))
			continue
		}
		installed = append(installed, p.Name)
	}
	// Force the next /catalog/packs to re-read: the installed copies must
	// immediately outrank their federated rows.
	remoteRegistry.mu.Lock()
	remoteRegistry.fetched = time.Time{}
	remoteRegistry.mu.Unlock()

	writeCatalogJSON(w, http.StatusOK, map[string]any{
		"installed": installed, "skipped": skipped, "count": len(installed),
	})
}

// resolveInstallSet expands a requested artifact into the skills to
// create: a skill installs itself; a pack installs its members; a
// meta-pack installs every skill belonging to any of its members.
func resolveInstallSet(remote []catalogPack, name string) []catalogPack {
	var self *catalogPack
	for i := range remote {
		if remote[i].Name == name {
			self = &remote[i]
			break
		}
	}
	if self == nil {
		return nil
	}
	switch self.ArtifactKind {
	case "", "skill", "tool":
		if !installable(*self) {
			return nil
		}
		return []catalogPack{*self}
	case "pack":
		var out []catalogPack
		for _, p := range remote {
			if p.Member == name && installable(p) {
				out = append(out, p)
			}
		}
		return out
	case "meta-pack":
		// Scope to the meta-pack's OWN family. This used to take every
		// skill sharing the registry, which on a registry hosting more
		// than one family is badly wrong: picking parkforge-brain
		// (5 packs, 6 skills, namespace meshforge) also installed all 13
		// unrelated agent-office skills — openshift-docs, weekly-ops-report
		// and friends — because they happened to be published to the same
		// host. A registry with one family in it hid the bug.
		//
		// The manifest names its member packs, so use them; the namespace
		// is the fallback boundary when an older index omits `members`.
		wanted := map[string]bool{}
		for _, m := range self.Members {
			wanted[m] = true
		}
		var out []catalogPack
		for _, p := range remote {
			if !installable(p) || p.Registry != self.Registry {
				continue
			}
			if len(wanted) > 0 {
				if wanted[p.Member] {
					out = append(out, p)
				}
				continue
			}
			if p.Namespace == self.Namespace {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// installable reports whether an artifact carries what its kind needs
// to be materialized: a skill needs a body to fetch, a tool needs a
// definition to register. A pack row lists members and installs nothing
// itself.
func installable(p catalogPack) bool {
	switch p.ArtifactKind {
	case "tool":
		return p.Tool != nil
	case "", "skill":
		return p.ContentURL != ""
	}
	return false
}

// installSkill fetches the artifact's body and upserts a Skill CR.
func (h *CatalogSkillsHandler) installSkill(ctx context.Context, p catalogPack) error {
	body, err := fetchText(ctx, p.ContentURL)
	if err != nil {
		return fmt.Errorf("fetch content: %w", err)
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("registry served empty content")
	}

	display := p.DisplayName
	if display == "" {
		display = p.Name
	}
	desired := &agentofficev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: h.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "agent-office",
				"agentoffice.ai/skill-tier":    "mindifact",
			},
			Annotations: map[string]string{
				"agentoffice.ai/registry":    p.Registry,
				"agentoffice.ai/pack-ref":    fmt.Sprintf("%s/%s:%s", p.Namespace, p.Name, p.Version),
				"agentoffice.ai/manifest":    p.Manifest,
				"agentoffice.ai/content-url": p.ContentURL,
			},
		},
		Spec: agentofficev1alpha1.SkillSpec{
			DisplayName: display,
			Description: p.Description,
			Version:     p.Version,
			// The registry's `requires` becomes the CR's declared
			// dependencies, so the composer keeps telling the truth about
			// what the skill needs after it is installed.
			Dependencies: toSkillDeps(p.Dependencies),
			Source:       agentofficev1alpha1.SkillSource{Inline: body},
		},
	}
	if p.Member != "" {
		desired.Labels["agentoffice.ai/pack"] = p.Member
	}

	var existing agentofficev1alpha1.Skill
	err = h.client.Get(ctx, types.NamespacedName{Namespace: h.namespace, Name: p.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return h.client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Idempotent update — reinstalling the same version is a no-op write.
	existing.Labels = mergeLabels(existing.Labels, desired.Labels)
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	for k, v := range desired.Annotations {
		existing.Annotations[k] = v
	}
	existing.Spec = desired.Spec
	return h.client.Update(ctx, &existing)
}

func toSkillDeps(in []catalogDependency) []agentofficev1alpha1.SkillDependency {
	if len(in) == 0 {
		return nil
	}
	out := make([]agentofficev1alpha1.SkillDependency, 0, len(in))
	for _, d := range in {
		out = append(out, agentofficev1alpha1.SkillDependency{
			Kind: d.Kind, Name: d.Name, Optional: d.Optional,
		})
	}
	return out
}

func fetchText(ctx context.Context, url string) (string, error) {
	if url == "" {
		return "", fmt.Errorf("artifact has no content URL")
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	return string(b), err
}

// installTool materializes a federated tool artifact as an
// MCPServerRegistration — the missing half of "install a mindifact".
//
// Until now a bundle could only NAME a tool it needed
// (requires: {kind: mcpServer, name: unreal}) and the operator could
// only presence-check that name. Nothing could put the tool there, so
// every parkforge skill declared a dependency on `unreal` that no
// install could ever satisfy. A pack that ships its own tools is the
// difference between a bundle and a shopping list.
//
// Two modes, and the distinction is load-bearing:
//
//   - reference: the workload already runs here — gitops deployed it, or
//     an agent built it, as forge-dev builds ops-metrics mid-demo. This
//     registers it with the gateway. The HTTPRoute must exist; a
//     registration pointing at a missing route is accepted by the API
//     and silently never serves, so this checks first and reports
//     instead of creating a lie.
//   - bundled: the artifact ships the workload too. Deliberately
//     refused rather than half-installed — creating a registration for
//     a Deployment nobody created is the same failure with more steps.
func (h *CatalogSkillsHandler) installTool(ctx context.Context, p catalogPack) error {
	t := p.Tool
	if t == nil {
		return fmt.Errorf("tool artifact carries no definition")
	}
	if t.Mode == "bundled" {
		return fmt.Errorf("mode %q ships a workload, which this operator cannot install yet; "+
			"deploy it via gitops and republish as mode:reference", t.Mode)
	}
	route := t.RouteRef
	if route == "" {
		return fmt.Errorf("mode reference needs routeRef (the HTTPRoute serving this MCP server)")
	}

	// The route is the whole contract of a reference tool. Without it the
	// registration is inert and nothing says so.
	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute",
	})
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: h.namespace, Name: route}, hr); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("HTTPRoute %q not on this cluster — the MCP server it fronts is not deployed", route)
		}
		return fmt.Errorf("checking HTTPRoute %q: %w", route, err)
	}

	spec := map[string]any{
		"targetRef": map[string]any{
			"group": "gateway.networking.k8s.io",
			"kind":  "HTTPRoute",
			"name":  route,
		},
		"state": "Enabled",
	}
	if t.Path != "" {
		spec["path"] = t.Path
	}
	if t.Prefix != "" {
		spec["prefix"] = t.Prefix
	}
	if t.Hint != "" {
		spec["hint"] = t.Hint
	}
	if len(t.Category) > 0 {
		spec["category"] = toAnySlice(t.Category)
	}
	if t.CredentialRef != nil && t.CredentialRef.Name != "" {
		spec["credentialRef"] = map[string]any{
			"name": t.CredentialRef.Name,
			"key":  t.CredentialRef.Key,
		}
	}

	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "mcp.kuadrant.io", Version: "v1alpha1", Kind: "MCPServerRegistration",
	})
	desired.SetName(p.Name)
	desired.SetNamespace(h.namespace)
	desired.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": "agent-office",
		"app.kubernetes.io/component":  "registration",
		"agentoffice.ai/skill-tier":    "mindifact",
	})
	desired.SetAnnotations(toolProvenance(p))
	desired.Object["spec"] = spec

	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := h.client.Get(ctx, types.NamespacedName{Namespace: h.namespace, Name: p.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return h.client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Idempotent: reinstalling the same version rewrites spec + provenance
	// and leaves anything the gateway added to status alone.
	existing.Object["spec"] = spec
	ann := existing.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	for k, v := range toolProvenance(p) {
		ann[k] = v
	}
	existing.SetAnnotations(ann)
	existing.SetLabels(mergeLabels(existing.GetLabels(), desired.GetLabels()))
	return h.client.Update(ctx, &existing)
}

// toolProvenance records where the tool came from and, crucially, the
// client recipe — gatherPacks reads that annotation back to build the
// mcpServers entry the composer wires, so an installed tool is
// indistinguishable from a gitops-authored one.
func toolProvenance(p catalogPack) map[string]string {
	out := map[string]string{
		"agentoffice.ai/registry": p.Registry,
		"agentoffice.ai/pack-ref": fmt.Sprintf("%s/%s:%s", p.Namespace, p.Name, p.Version),
	}
	if p.DisplayName != "" {
		out["agentoffice.ai/display-name"] = p.DisplayName
	}
	if p.Description != "" {
		out["agentoffice.ai/description"] = p.Description
	}
	if p.Recipe != nil && p.Recipe.URL != "" {
		if b, err := json.Marshal(p.Recipe); err == nil {
			out["agentoffice.ai/client-recipe"] = string(b)
		}
	}
	return out
}

func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
