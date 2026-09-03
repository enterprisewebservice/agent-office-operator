package controller

import (
	"context"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

func endpointConn(name string, groups []string) (*agentofficev1alpha1.ModelConnection, *corev1.Secret) {
	c := &agentofficev1alpha1.ModelConnection{ObjectMeta: metav1.ObjectMeta{Name: name}}
	c.Spec.DisplayName = name
	c.Spec.Kind = agentofficev1alpha1.ModelConnectionKindEndpoint
	c.Spec.BaseURL = "http://" + name + ".svc:4000/v1"
	c.Spec.Models = []agentofficev1alpha1.ModelCatalogEntry{{ID: name + "-model"}}
	c.Spec.APIKeySecretRef = &agentofficev1alpha1.ConnectionSecretRef{Name: name + "-key", Namespace: "agent-office", Key: "api-key"}
	if groups != nil {
		c.Spec.Access = &agentofficev1alpha1.ModelConnectionAccess{Groups: groups}
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-key", Namespace: "agent-office"}, Data: map[string][]byte{"api-key": []byte("k-" + name)}}
	return c, sec
}

func gatewayPair(ns, gwName, awName, connectionRef string) (*agentofficev1alpha1.AgentGateway, *agentofficev1alpha1.AgentWorkstation) {
	gw := &agentofficev1alpha1.AgentGateway{ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: ns}}
	aw := &agentofficev1alpha1.AgentWorkstation{ObjectMeta: metav1.ObjectMeta{Name: awName, Namespace: ns}}
	aw.Spec.Runtime = &agentofficev1alpha1.RuntimeSpec{Shared: &agentofficev1alpha1.SharedRuntime{GatewayRef: gwName}}
	aw.Spec.Model = agentofficev1alpha1.ModelSpec{ConnectionRef: connectionRef}
	return gw, aw
}

func sortedSecretKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// A brain swap must be additive: the connection whose key is already
// projected stays rendered so the gateway's persisted config never
// references a missing ${MODELCONN_*_API_KEY}.
func TestCollectModelConnectionsCarriesProjectedConnections(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = agentofficev1alpha1.AddToScheme(scheme)

	ns := nsWith("seat-agent-workspace", map[string]string{agentofficev1alpha1.TenantGroupsAnnotation: "attendees"})
	claude, claudeKey := endpointConn("claude-work", []string{"attendees"})
	lite, liteKey := endpointConn("litemaas", []string{"attendees"})
	gw, aw := gatewayPair(ns.Name, "seat-gateway", "seat-assistant", "litemaas") // swapped away from claude-work
	projected := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: modelConnectionsSecretName(gw.Name), Namespace: ns.Name},
		Data: map[string][]byte{connEnvVarName("claude-work"): []byte("old")}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, claude, claudeKey, lite, liteKey, gw, aw, projected).Build()
	r := &AgentGatewayReconciler{Client: cl, Scheme: scheme}
	providers, data, err := r.collectModelConnections(context.Background(), gw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := providers["litemaas"]; !ok {
		t.Fatalf("the referenced connection must render, got %v", providers)
	}
	if _, ok := providers["claude-work"]; !ok {
		t.Fatalf("the previously projected connection must stay rendered, got %v", providers)
	}
	want := []string{connEnvVarName("claude-work"), connEnvVarName("litemaas")}
	if got := sortedSecretKeys(data); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("derived secret keys: got %v want %v", got, want)
	}
	if string(data[connEnvVarName("claude-work")]) != "k-claude-work" {
		t.Fatal("the carried key must be re-read from the admin Secret, not copied stale")
	}
}

// Revocation still prunes: a carried connection that is no longer offered
// to the workspace is dropped, block and key.
func TestCollectModelConnectionsDropsUnofferedCarriedConnection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = agentofficev1alpha1.AddToScheme(scheme)

	ns := nsWith("seat-agent-workspace", map[string]string{agentofficev1alpha1.TenantGroupsAnnotation: "attendees"})
	private, privateKey := endpointConn("model-desk", []string{"model-admins"})
	lite, liteKey := endpointConn("litemaas", []string{"attendees"})
	gw, aw := gatewayPair(ns.Name, "seat-gateway", "seat-assistant", "litemaas")
	projected := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: modelConnectionsSecretName(gw.Name), Namespace: ns.Name},
		Data: map[string][]byte{connEnvVarName("model-desk"): []byte("old")}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, private, privateKey, lite, liteKey, gw, aw, projected).Build()
	r := &AgentGatewayReconciler{Client: cl, Scheme: scheme}
	providers, data, err := r.collectModelConnections(context.Background(), gw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := providers["model-desk"]; ok {
		t.Fatal("a connection no longer offered to the workspace must not stay rendered")
	}
	if _, ok := data[connEnvVarName("model-desk")]; ok {
		t.Fatal("a connection no longer offered to the workspace must lose its key")
	}
	if _, ok := data[connEnvVarName("litemaas")]; !ok {
		t.Fatal("the offered, referenced connection must keep its key")
	}
}
