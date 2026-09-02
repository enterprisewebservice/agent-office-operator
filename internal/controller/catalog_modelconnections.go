/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// GET /catalog/model-connections — the hiring UI's brain menu.
//
// Returns every ModelConnection's NON-SECRET metadata, including its
// access rules, so the AgentGenesis field can filter the radio list
// against the signed-in user's group memberships
// (user.entity.spec.memberOf — the same identity surface the genesis
// template already routes git providers on). The filtering is a menu
// courtesy, not the security boundary: connection credentials live
// only in the admin namespace and are projected exclusively by the
// operator, so a hand-written CR referencing an unadvertised
// connection gains a provider block but never Secret access.
//
// Secret references are deliberately absent from the wire shape.

type modelConnectionEntry struct {
	Name        string                                    `json:"name"`
	DisplayName string                                    `json:"displayName"`
	Description string                                    `json:"description,omitempty"`
	Kind        string                                    `json:"kind"`
	Provider    string                                    `json:"provider,omitempty"`
	Models      []modelConnectionModel                    `json:"models,omitempty"`
	KeyStrategy string                                    `json:"keyStrategy,omitempty"`
	Access      *agentofficev1alpha1.ModelConnectionAccess `json:"access,omitempty"`
	// Default marks the connection the hiring form pre-selects for a
	// user who can see it (annotation agentoffice.ai/default-brain=true,
	// v1.7.65). Without it the form fell through to the platform Codex
	// subscription for anyone who never touched the Brain section.
	Default bool `json:"default,omitempty"`
}

type modelConnectionModel struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type modelConnectionList struct {
	Items []modelConnectionEntry `json:"items"`
	Count int                    `json:"count"`
}

func (h *CatalogSkillsHandler) listModelConnections(w http.ResponseWriter, r *http.Request) {
	var conns agentofficev1alpha1.ModelConnectionList
	if err := h.client.List(r.Context(), &conns); err != nil {
		writeCatalogJSONError(w, http.StatusInternalServerError, err)
		return
	}

	items := make([]modelConnectionEntry, 0, len(conns.Items))
	for _, c := range conns.Items {
		e := modelConnectionEntry{
			Default: c.Annotations["agentoffice.ai/default-brain"] == "true",
			Name:        c.Name,
			DisplayName: c.Spec.DisplayName,
			Description: c.Spec.Description,
			Kind:        string(c.Spec.Kind),
			Provider:    string(c.Spec.Provider),
			KeyStrategy: c.Spec.KeyStrategy,
			Access:      c.Spec.Access,
		}
		for _, m := range c.Spec.Models {
			e.Models = append(e.Models, modelConnectionModel{ID: m.ID, Name: m.Name})
		}
		items = append(items, e)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelConnectionList{Items: items, Count: len(items)})
}

// POST /catalog/model-connections/probe — ask an OpenAI-compatible
// endpoint what models it serves, so the admin UI can offer
// checkboxes instead of free-typed model ids. Body:
//
//	{ "baseUrl": "http://…/v1",
//	  "apiKey": "sk-…",                        // optional, OR
//	  "secretRef": {"name","namespace","key"} } // optional
//
// The key never returns to the caller; the response is only
// {models: [{id}]} or {error}. Admin-supplied URLs only — the same
// trust as publishing the connection itself.
type probeRequest struct {
	BaseURL   string `json:"baseUrl"`
	APIKey    string `json:"apiKey,omitempty"`
	SecretRef *struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace,omitempty"`
		Key       string `json:"key,omitempty"`
	} `json:"secretRef,omitempty"`
}

func (h *CatalogSkillsHandler) probeModelEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeCatalogJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("only POST is supported"))
		return
	}
	var req probeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeCatalogJSONError(w, http.StatusBadRequest, fmt.Errorf("bad body: %w", err))
		return
	}
	base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		writeCatalogJSONError(w, http.StatusBadRequest, fmt.Errorf("baseUrl must be http(s)"))
		return
	}

	key := req.APIKey
	if key == "" && req.SecretRef != nil && req.SecretRef.Name != "" {
		ns := req.SecretRef.Namespace
		if ns == "" {
			ns = "agent-office"
		}
		k := req.SecretRef.Key
		if k == "" {
			k = "api-key"
		}
		var sec corev1.Secret
		if err := h.client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: req.SecretRef.Name}, &sec); err == nil {
			key = string(sec.Data[k])
		}
	}

	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, base+"/models", nil)
	if err != nil {
		writeCatalogJSONError(w, http.StatusBadRequest, err)
		return
	}
	if key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeCatalogJSONError(w, http.StatusBadGateway, fmt.Errorf("endpoint unreachable: %w", err))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		writeCatalogJSONError(w, http.StatusBadGateway,
			fmt.Errorf("endpoint answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)]))))
		return
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeCatalogJSONError(w, http.StatusBadGateway, fmt.Errorf("endpoint returned non-OpenAI models payload"))
		return
	}
	out := struct {
		Models []modelConnectionModel `json:"models"`
	}{Models: make([]modelConnectionModel, 0, len(parsed.Data))}
	for _, m := range parsed.Data {
		if m.ID != "" {
			out.Models = append(out.Models, modelConnectionModel{ID: m.ID})
		}
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].ID < out.Models[j].ID })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
