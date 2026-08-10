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
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// TestListPacks pins the unified search surface the composer trusts:
// all three types in one response with deterministic order, the tool
// recipe coming from the registration's client-recipe annotation (and
// the bare-gateway default when absent), skill dependencies enriched,
// and ?query= narrowing across types.
func TestListPacks(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentofficev1alpha1.AddToScheme(scheme)
	regGVK := schema.GroupVersionKind{
		Group: "mcp.kuadrant.io", Version: "v1alpha1", Kind: "MCPServerRegistration",
	}
	scheme.AddKnownTypeWithName(regGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		regGVK.GroupVersion().WithKind("MCPServerRegistrationList"),
		&unstructured.UnstructuredList{},
	)

	github := &unstructured.Unstructured{}
	github.SetGroupVersionKind(regGVK)
	github.SetNamespace("agent-office")
	github.SetName("github")
	github.SetAnnotations(map[string]string{
		"agentoffice.ai/client-recipe": `{"url":"http://mcp-gateway-data-science-gateway-class.agent-office.svc.cluster.local/mcp","type":"http","authHeaderValue":"Bearer ${GITHUB_PERSONAL_ACCESS_TOKEN}","envFromSecret":"github-mcp-installation-token"}`,
		"agentoffice.ai/display-name":  "GitHub (governed)",
	})

	bare := &unstructured.Unstructured{}
	bare.SetGroupVersionKind(regGVK)
	bare.SetNamespace("agent-office")
	bare.SetName("ops-metrics")

	skill := &agentofficev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agent-office", Name: "weekly-ops-report"},
		Spec: agentofficev1alpha1.SkillSpec{
			DisplayName: "Weekly Ops Report",
			Version:     "0.1.0",
			Source:      agentofficev1alpha1.SkillSource{Inline: "body"},
			Dependencies: []agentofficev1alpha1.SkillDependency{
				{Kind: "mcpServer", Name: "ops-metrics"},
			},
		},
	}
	kb := &agentofficev1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agent-office", Name: "platform-capabilities"},
		Spec: agentofficev1alpha1.KnowledgeBaseSpec{
			DisplayName: "Agent Platform Capabilities",
		},
	}

	h := NewCatalogSkillsHandler(
		fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(github, bare, skill, kb).Build(),
	)

	get := func(url string) map[string]any {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		if rec.Code != 200 {
			t.Fatalf("%s -> %d %s", url, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		return out
	}

	all := get("/catalog/packs")
	items := all["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("want 4 packs (1 skill, 2 tools, 1 kb), got %d: %s", len(items), mustJSON(items))
	}
	// deterministic order: skill, tool(github), tool(ops-metrics), kb
	types := []string{}
	for _, it := range items {
		types = append(types, it.(map[string]any)["type"].(string))
	}
	if types[0] != "skill" || types[1] != "tool" || types[3] != "kb" {
		t.Fatalf("type order wrong: %v", types)
	}

	// skill deps enriched: ops-metrics registration EXISTS here -> available
	sk := items[0].(map[string]any)
	dep := sk["dependencies"].([]any)[0].(map[string]any)
	if dep["available"] != true {
		t.Errorf("skill dep should be available (registration present): %v", dep)
	}

	// github recipe from annotation, verbatim
	var gh map[string]any
	for _, it := range items {
		m := it.(map[string]any)
		if m["type"] == "tool" && m["name"] == "github" {
			gh = m
		}
	}
	rec := gh["recipe"].(map[string]any)
	if rec["envFromSecret"] != "github-mcp-installation-token" ||
		rec["authHeaderValue"] != "Bearer ${GITHUB_PERSONAL_ACCESS_TOKEN}" {
		t.Errorf("github recipe not honored: %v", rec)
	}
	if gh["displayName"] != "GitHub (governed)" {
		t.Errorf("display-name annotation not honored: %v", gh["displayName"])
	}

	// bare registration gets the default shared-gateway recipe
	for _, it := range items {
		m := it.(map[string]any)
		if m["type"] == "tool" && m["name"] == "ops-metrics" {
			r := m["recipe"].(map[string]any)
			if r["url"] != sharedGatewayURL("agent-office") || r["type"] != "http" {
				t.Errorf("default recipe wrong: %v", r)
			}
		}
	}

	// search narrows across types
	q := get("/catalog/packs?query=github")
	if int(q["count"].(float64)) != 1 {
		t.Errorf("query=github want 1, got %v", q["count"])
	}
	tf := get("/catalog/packs?type=kb")
	if int(tf["count"].(float64)) != 1 {
		t.Errorf("type=kb want 1, got %v", tf["count"])
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
