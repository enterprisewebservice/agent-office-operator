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
	"net/http"
	"sort"

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
