package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

func nsWith(name string, ann map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: ann}}
}

func connWith(name string, groups, users []string) *agentofficev1alpha1.ModelConnection {
	c := &agentofficev1alpha1.ModelConnection{ObjectMeta: metav1.ObjectMeta{Name: name}}
	c.Spec.DisplayName = name
	c.Spec.Kind = agentofficev1alpha1.ModelConnectionKindEndpoint
	c.Spec.Models = []agentofficev1alpha1.ModelCatalogEntry{{ID: "m1"}}
	if groups != nil || users != nil {
		c.Spec.Access = &agentofficev1alpha1.ModelConnectionAccess{Groups: groups, Users: users}
	}
	return c
}

func TestTenantOf(t *testing.T) {
	if _, ok := tenantOf(nsWith("agent-office", nil)); ok {
		t.Fatal("a namespace without the annotations is a platform namespace")
	}
	p, ok := tenantOf(nsWith("test2-agent-workspace", map[string]string{
		agentofficev1alpha1.TenantUserAnnotation:   " test2 ",
		agentofficev1alpha1.TenantGroupsAnnotation: "attendees, redhat-workshop-users,,",
	}))
	if !ok || p.User != "test2" || len(p.Groups) != 2 || p.Groups[0] != "attendees" {
		t.Fatalf("unexpected principal %+v ok=%v", p, ok)
	}
	if _, ok := tenantOf(nsWith("x", map[string]string{agentofficev1alpha1.TenantGroupsAnnotation: ""})); !ok {
		t.Fatal("an empty groups annotation still marks a tenant (offered nothing)")
	}
}

func TestConnectionOfferedTo(t *testing.T) {
	attendee := tenantPrincipal{User: "test2", Groups: []string{"attendees"}}
	cases := []struct {
		name string
		conn *agentofficev1alpha1.ModelConnection
		p    tenantPrincipal
		want bool
		by   string
	}{
		{"no access list offers nobody", connWith("c", nil, nil), attendee, false, ""},
		{"empty access list offers nobody", connWith("c", []string{}, []string{}), attendee, false, ""},
		{"group match", connWith("c", []string{"redhat-workshop-users", "attendees"}, nil), attendee, true, "group:attendees"},
		{"group match is case-insensitive", connWith("c", []string{"Attendees"}, nil), attendee, true, "group:Attendees"},
		{"group ref matches on its last segment", connWith("c", []string{"attendees"}, nil), tenantPrincipal{Groups: []string{"group:default/attendees"}}, true, "group:attendees"},
		{"user match wins over groups", connWith("c", []string{"attendees"}, []string{"Test2"}), attendee, true, "user:Test2"},
		{"other groups only", connWith("c", []string{"model-admins"}, []string{"deanpeterson"}), attendee, false, ""},
		{"principal with nothing", connWith("c", []string{"attendees"}, nil), tenantPrincipal{}, false, ""},
	}
	for _, c := range cases {
		got, by := connectionOfferedTo(c.conn, c.p)
		if got != c.want || by != c.by {
			t.Errorf("%s: got (%v, %q) want (%v, %q)", c.name, got, by, c.want, c.by)
		}
	}
}

func TestOfferReconcilerProjectsAndWithdraws(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = agentofficev1alpha1.AddToScheme(scheme)

	seat := nsWith("test2-agent-workspace", map[string]string{
		agentofficev1alpha1.TenantUserAnnotation:   "test2",
		agentofficev1alpha1.TenantGroupsAnnotation: "attendees",
	})
	platform := nsWith("agent-office", nil)
	pub := connWith("claude-work", []string{"attendees", "model-admins"}, nil)
	private := connWith("dean-codex-subscription", []string{"model-admins"}, []string{"deanpeterson"})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(seat, platform, pub, private).
		WithStatusSubresource(&agentofficev1alpha1.ModelConnectionOffer{}).Build()
	r := &ModelConnectionOfferReconciler{Client: cl, Scheme: scheme}
	ctx := context.Background()
	for _, n := range []string{"claude-work", "dean-codex-subscription"} {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: n}}); err != nil {
			t.Fatalf("reconcile %s: %v", n, err)
		}
	}
	var offers agentofficev1alpha1.ModelConnectionOfferList
	if err := cl.List(ctx, &offers); err != nil {
		t.Fatal(err)
	}
	if len(offers.Items) != 1 || offers.Items[0].Namespace != "test2-agent-workspace" || offers.Items[0].Name != "claude-work" {
		t.Fatalf("expected exactly the public connection offered to the seat, got %+v", offers.Items)
	}
	o := offers.Items[0]
	if o.Spec.OfferedTo != "group:attendees" || o.Spec.ConnectionRef != "claude-work" || len(o.Spec.Models) != 1 || len(o.OwnerReferences) != 1 {
		t.Fatalf("offer spec/owner not projected: %+v", o)
	}

	// Narrow the access list: the offer is withdrawn.
	pub.Spec.Access.Groups = []string{"model-admins"}
	if err := cl.Update(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "claude-work"}}); err != nil {
		t.Fatal(err)
	}
	if err := cl.List(ctx, &offers); err != nil {
		t.Fatal(err)
	}
	if len(offers.Items) != 0 {
		t.Fatalf("offer should be withdrawn after the access list narrowed, got %+v", offers.Items)
	}

	// Widen it again by user: projected with the user match recorded.
	pub.Spec.Access.Users = []string{"test2"}
	if err := cl.Update(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "claude-work"}}); err != nil {
		t.Fatal(err)
	}
	var got agentofficev1alpha1.ModelConnectionOffer
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "test2-agent-workspace", Name: "claude-work"}, &got); err != nil {
		t.Fatalf("offer not re-projected: %v", err)
	}
	if got.Spec.OfferedTo != "user:test2" {
		t.Fatalf("expected the user match recorded, got %q", got.Spec.OfferedTo)
	}
}
