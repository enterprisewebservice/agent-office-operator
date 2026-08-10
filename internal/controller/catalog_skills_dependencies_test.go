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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// TestEnrichDependencies covers the three states a composer must be
// able to trust: an mcpServer dep whose registration exists (available
// + gateway URL), one whose registration is absent (unavailable, no
// URL), and a knowledgeBase dep resolved against KnowledgeBase CRs.
// The kuadrant type is registered as unstructured, exactly as the
// handler queries it — the operator does not import kuadrant's API.
func TestEnrichDependencies(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	if err := agentofficev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("agentoffice scheme: %v", err)
	}
	regGVK := schema.GroupVersionKind{
		Group: "mcp.kuadrant.io", Version: "v1alpha1", Kind: "MCPServerRegistration",
	}
	scheme.AddKnownTypeWithName(regGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		regGVK.GroupVersion().WithKind("MCPServerRegistrationList"),
		&unstructured.UnstructuredList{},
	)

	reg := &unstructured.Unstructured{}
	reg.SetGroupVersionKind(regGVK)
	reg.SetNamespace("agent-office")
	reg.SetName("ops-metrics")

	kb := &agentofficev1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agent-office", Name: "morgan-wiki"},
	}

	h := NewCatalogSkillsHandler(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(reg, kb).Build(),
	)

	got := h.enrichDependencies(context.Background(), []agentofficev1alpha1.SkillDependency{
		{Kind: "mcpServer", Name: "ops-metrics"},
		{Kind: "mcpServer", Name: "not-deployed"},
		{Kind: "knowledgeBase", Name: "morgan-wiki", Optional: true},
		{Kind: "knowledgeBase", Name: "missing-wiki"},
	})

	if len(got) != 4 {
		t.Fatalf("want 4 enriched deps, got %d", len(got))
	}
	wantURL := "http://mcp-gateway-data-science-gateway-class.agent-office.svc.cluster.local/mcp"
	if !got[0].Available || got[0].GatewayURL != wantURL {
		t.Errorf("ops-metrics: want available with gateway URL %q, got %+v", wantURL, got[0])
	}
	if got[1].Available || got[1].GatewayURL != "" {
		t.Errorf("not-deployed: want unavailable with no URL, got %+v", got[1])
	}
	if !got[2].Available || !got[2].Optional {
		t.Errorf("morgan-wiki: want available+optional, got %+v", got[2])
	}
	if got[3].Available {
		t.Errorf("missing-wiki: want unavailable, got %+v", got[3])
	}

	// Empty declaration stays empty — the catalog must not invent a
	// dependencies key for skills that declare none.
	if h.enrichDependencies(context.Background(), nil) != nil {
		t.Errorf("nil deps must enrich to nil")
	}
}
