/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// Backstage Binders proxy endpoints.
//
// v1.5.3: read-only HTTP endpoints consumed by the
// agentworkstation-binders Backstage frontend plugin
// (rhdh-plugins/agentworkstation-binders in the agent-office gitops
// repo). The plugin renders a drag-drop "Bindings" card on each
// AgentWorkstation entity page in RHDH that lets users attach
// KnowledgeBases, MemoryModules, and Skills to an agent and Save
// the change as a PR against the gitops repo.
//
// Why these live on the operator: the plugin needs to LIST the
// available binding-source CRs (KBs, MemoryModules, Skills,
// SkillBindings) and GET the current AW spec — all in the user's
// browser. Direct K8s API access from the browser would require
// per-user kubeconfig wiring; serving the same data from the
// operator (which already has informers cached) is faster, doesn't
// leak K8s credentials into the browser, and reuses the existing
// agent-office-backstage-catalog Service that the codex-reauth-ui
// and Karpathy template's EntityPicker already proxy through.
//
// Endpoint shapes (all GET, all return application/json):
//
//   /binders/namespaces/<ns>/knowledgebases
//     {items: [{name, displayName, description, gatewayRef}]}
//
//   /binders/namespaces/<ns>/memorymodules
//     {items: [{name, displayName, kind, filename, version}]}
//
//   /binders/namespaces/<ns>/skills
//     {items: [{name, displayName, tool, version}]}
//
//   /binders/namespaces/<ns>/skillbindings
//     {items: [{name, skillRef:{name,version}, appliedTo}]}
//
//   /binders/namespaces/<ns>/agentworkstations/<aw>
//     {metadata:{name,namespace,annotations}, spec:{...}}
//
//   /binders/namespaces/<ns>/agentworkstations/<aw>/gitops-source
//     {repoUrl, repoOwner, repoName, defaultBranch, filePath}
//
// Authentication: none today (read-only metadata, in-cluster only,
// pulled by RHDH's proxy plugin from a trusted Service). If the AW
// spec ever carries sensitive content (it shouldn't — credentials
// live in Secrets, not in AW spec), gate behind a Bearer token here
// and add the secret on the RHDH proxy entry.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// BindersHandler serves the binder-proxy endpoints. Routed under
// /binders/... on the same HTTP server as BackstageCatalogHandler.
type BindersHandler struct {
	Client client.Client
}

// NewBindersHandler constructs the handler with a controller-runtime
// client (already has informers cached — list/get calls are cheap).
func NewBindersHandler(c client.Client) *BindersHandler {
	return &BindersHandler{Client: c}
}

// ServeHTTP routes requests to the matching handler method based on
// path. Patterns recognized:
//
//	GET /binders/namespaces/<ns>/knowledgebases
//	GET /binders/namespaces/<ns>/memorymodules
//	GET /binders/namespaces/<ns>/skills
//	GET /binders/namespaces/<ns>/skillbindings
//	GET /binders/namespaces/<ns>/agentworkstations/<aw>
//	GET /binders/namespaces/<ns>/agentworkstations/<aw>/gitops-source
func (h *BindersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// Expected: ["binders", "namespaces", "<ns>", "<resource>", ...]
	if len(parts) < 4 || parts[0] != "binders" || parts[1] != "namespaces" {
		http.NotFound(w, r)
		return
	}
	ns := parts[2]
	resource := parts[3]

	switch {
	case resource == "knowledgebases" && len(parts) == 4:
		h.listKnowledgeBases(w, r, ns)
	case resource == "memorymodules" && len(parts) == 4:
		h.listMemoryModules(w, r, ns)
	case resource == "skills" && len(parts) == 4:
		h.listSkills(w, r, ns)
	case resource == "skillbindings" && len(parts) == 4:
		h.listSkillBindings(w, r, ns)
	case resource == "agentworkstations" && len(parts) == 5:
		h.getAgentWorkstation(w, r, ns, parts[4])
	case resource == "agentworkstations" && len(parts) == 6 && parts[5] == "gitops-source":
		h.getAgentWorkstationGitopsSource(w, r, ns, parts[4])
	default:
		http.NotFound(w, r)
	}
}

// --------------------------- list endpoints ----------------------------

type kbItem struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	GatewayRef  string `json:"gatewayRef,omitempty"`
}

func (h *BindersHandler) listKnowledgeBases(w http.ResponseWriter, r *http.Request, ns string) {
	var list agentofficev1alpha1.KnowledgeBaseList
	if err := h.Client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeBinderJSONErr(w, err)
		return
	}
	items := make([]kbItem, 0, len(list.Items))
	for _, kb := range list.Items {
		items = append(items, kbItem{
			Name:        kb.Name,
			DisplayName: kb.Spec.DisplayName,
			Description: strings.TrimSpace(kb.Spec.Description),
			GatewayRef:  kb.Spec.GatewayRef.Name,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	writeBinderJSON(w, map[string]interface{}{"items": items})
}

type mmItem struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

func (h *BindersHandler) listMemoryModules(w http.ResponseWriter, r *http.Request, ns string) {
	var list agentofficev1alpha1.MemoryModuleList
	if err := h.Client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeBinderJSONErr(w, err)
		return
	}
	items := make([]mmItem, 0, len(list.Items))
	for _, mm := range list.Items {
		// MemoryModuleSpec has no DisplayName field — synthesize from
		// {Filename} when present, else {Name}. Kind is a typed string
		// (MemoryModuleKind); cast for the JSON shape.
		display := mm.Spec.Filename
		if display == "" {
			display = mm.Name
		}
		items = append(items, mmItem{
			Name:        mm.Name,
			DisplayName: display,
			Kind:        string(mm.Spec.Kind),
			Filename:    mm.Spec.Filename,
			Version:     mm.Spec.Version,
			Description: strings.TrimSpace(mm.Spec.Description),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	writeBinderJSON(w, map[string]interface{}{"items": items})
}

type skillItem struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Version     string `json:"version,omitempty"`
}

func (h *BindersHandler) listSkills(w http.ResponseWriter, r *http.Request, ns string) {
	var list agentofficev1alpha1.SkillList
	if err := h.Client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeBinderJSONErr(w, err)
		return
	}
	items := make([]skillItem, 0, len(list.Items))
	for _, s := range list.Items {
		// Tool lives under Spec.Invocation.Tool (not Spec.Tool — see
		// SkillInvocation type godoc).
		items = append(items, skillItem{
			Name:        s.Name,
			DisplayName: s.Spec.DisplayName,
			Tool:        s.Spec.Invocation.Tool,
			Version:     s.Spec.Version,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	writeBinderJSON(w, map[string]interface{}{"items": items})
}

type sbSkillRef struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type sbItem struct {
	Name      string     `json:"name"`
	SkillRef  sbSkillRef `json:"skillRef"`
	AppliedTo []string   `json:"appliedTo"`
}

func (h *BindersHandler) listSkillBindings(w http.ResponseWriter, r *http.Request, ns string) {
	var list agentofficev1alpha1.SkillBindingList
	if err := h.Client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeBinderJSONErr(w, err)
		return
	}
	items := make([]sbItem, 0, len(list.Items))
	for _, sb := range list.Items {
		applied := []string{}
		if sb.Status.AppliedTo != nil {
			applied = append(applied, sb.Status.AppliedTo...)
			sort.Strings(applied)
		}
		items = append(items, sbItem{
			Name: sb.Name,
			SkillRef: sbSkillRef{
				Name:    sb.Spec.SkillRef.Name,
				Version: sb.Spec.SkillRef.Version,
			},
			AppliedTo: applied,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	writeBinderJSON(w, map[string]interface{}{"items": items})
}

// --------------------------- get endpoints ----------------------------

// getAgentWorkstation returns the AW spec in a shape the plugin can
// consume without depending on the full CRD type. We return only the
// fields the binder UI cares about: spec.runtime, spec.tools,
// spec.memory, spec.knowledgeBaseRefs, plus metadata.annotations
// (so the gitops-source annotations are available client-side too if
// the plugin wants to render them).
func (h *BindersHandler) getAgentWorkstation(w http.ResponseWriter, r *http.Request, ns, name string) {
	var aw agentofficev1alpha1.AgentWorkstation
	if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &aw); err != nil {
		writeBinderJSONErr(w, err)
		return
	}
	writeBinderJSON(w, map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":        aw.Name,
			"namespace":   aw.Namespace,
			"annotations": aw.Annotations,
		},
		"spec": aw.Spec,
	})
}

// getAgentWorkstationGitopsSource returns where the AW manifest
// lives in git so the binder plugin's Save knows which file to
// patch. Reads from these annotations on the AW CR:
//
//	agentoffice.ai/gitops-source-repo:   enterprisewebservice/agent-office
//	agentoffice.ai/gitops-source-path:   agents/pm-agent.yaml
//	agentoffice.ai/gitops-source-branch: main  (optional; default "main")
//
// If either of the first two annotations is missing, returns 404 with
// a JSON body explaining what's needed.
func (h *BindersHandler) getAgentWorkstationGitopsSource(w http.ResponseWriter, r *http.Request, ns, name string) {
	var aw agentofficev1alpha1.AgentWorkstation
	if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &aw); err != nil {
		writeBinderJSONErr(w, err)
		return
	}
	ann := aw.Annotations
	repo := ann["agentoffice.ai/gitops-source-repo"]
	path := ann["agentoffice.ai/gitops-source-path"]
	branch := ann["agentoffice.ai/gitops-source-branch"]
	if branch == "" {
		branch = "main"
	}
	if repo == "" || path == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "gitops-source not configured",
			"hint": "Set agentoffice.ai/gitops-source-repo (owner/repo) and " +
				"agentoffice.ai/gitops-source-path (path within the repo) " +
				"annotations on the AgentWorkstation, or use the agent-office " +
				"Software Template which sets them automatically.",
		})
		return
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": fmt.Sprintf("invalid repo %q — expected owner/repo", repo),
		})
		return
	}
	writeBinderJSON(w, map[string]interface{}{
		"repoUrl":       fmt.Sprintf("https://github.com/%s", repo),
		"repoOwner":     parts[0],
		"repoName":      parts[1],
		"defaultBranch": branch,
		"filePath":      path,
	})
}

// --------------------------- helpers ----------------------------
//
// Prefixed with "Binder" to avoid colliding with the writeJSON helper
// in backstage_catalog.go (same package, both serve JSON).

func writeBinderJSON(w http.ResponseWriter, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func writeBinderJSONErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
