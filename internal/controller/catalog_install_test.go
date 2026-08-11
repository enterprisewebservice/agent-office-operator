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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// TestInstallFromRegistry: installing a federated artifact creates a real
// Skill CR carrying the body, the version, the declared dependencies and
// the pack provenance — and doing it twice updates rather than duplicates.
func TestInstallFromRegistry(t *testing.T) {
	const body = "---\nname: terrain-strokes\n---\nEdit the ground."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".md") {
			fmt.Fprint(w, body)
			return
		}
		fmt.Fprint(w, `{"registry":"mindifact.ai","apiVersion":"v1","artifacts":[
		 {"namespace":"meshforge","name":"parkforge-terrain","version":"1.6.0","kind":"pack",
		  "description":"terrain pack","manifest":"/v1/a.json"},
		 {"namespace":"meshforge","name":"parkforge-terrain-strokes","version":"1.6.0","kind":"skill",
		  "description":"Edit the ground in the Unreal park","member":"parkforge-terrain",
		  "manifest":"/v1/b.json","content":"/v1/x/1.6.0/skills/terrain-strokes.md",
		  "requires":[{"kind":"mcpServer","name":"unreal"}]}]}`)
	}))
	defer srv.Close()
	t.Setenv("AGENT_REGISTRY_URLS", srv.URL+"/v1/index.json")
	remoteRegistry.fetched = time.Time{}
	remoteRegistry.packs = nil

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentofficev1alpha1.AddToScheme(scheme)
	h := NewCatalogSkillsHandler(fake.NewClientBuilder().WithScheme(scheme).Build())

	post := func(name string) map[string]any {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/catalog/install",
			strings.NewReader(fmt.Sprintf(`{"name":%q}`, name)))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("install %s -> %d %s", name, rec.Code, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}

	// installing the PACK installs its member skills
	out := post("parkforge-terrain")
	if int(out["count"].(float64)) != 1 {
		t.Fatalf("pack install should create its member skill: %v", out)
	}

	var sk agentofficev1alpha1.Skill
	if err := h.client.Get(context.Background(),
		client_ObjectKey("agent-office", "parkforge-terrain-strokes"), &sk); err != nil {
		t.Fatalf("Skill CR not created: %v", err)
	}
	if sk.Spec.Source.Inline != body {
		t.Errorf("body not installed: %q", sk.Spec.Source.Inline)
	}
	if sk.Spec.Version != "1.6.0" {
		t.Errorf("version lost: %q", sk.Spec.Version)
	}
	if len(sk.Spec.Dependencies) != 1 || sk.Spec.Dependencies[0].Name != "unreal" {
		t.Errorf("dependencies not carried: %+v", sk.Spec.Dependencies)
	}
	if sk.Annotations["agentoffice.ai/registry"] != "mindifact.ai" ||
		sk.Labels["agentoffice.ai/pack"] != "parkforge-terrain" {
		t.Errorf("provenance lost: %v %v", sk.Annotations, sk.Labels)
	}

	// idempotent: same install again updates, does not duplicate or fail
	out = post("parkforge-terrain-strokes")
	if int(out["count"].(float64)) != 1 {
		t.Errorf("reinstall should succeed: %v", out)
	}
	var list agentofficev1alpha1.SkillList
	_ = h.client.List(context.Background(), &list)
	if len(list.Items) != 1 {
		t.Errorf("reinstall duplicated the CR: %d items", len(list.Items))
	}

	// unknown artifact is a clean 404, not a panic
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/catalog/install",
		strings.NewReader(`{"name":"nope"}`)))
	if rec.Code != 404 {
		t.Errorf("unknown artifact -> %d", rec.Code)
	}
}

func client_ObjectKey(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}
