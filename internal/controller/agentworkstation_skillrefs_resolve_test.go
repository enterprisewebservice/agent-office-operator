package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

func inlineSkill(ns, name, version, body string) *agentofficev1alpha1.Skill {
	return &agentofficev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentofficev1alpha1.SkillSpec{
			DisplayName: name, Version: version,
			Source: agentofficev1alpha1.SkillSource{Inline: body},
		},
	}
}

// TestListRefSkills pins the v1.7.68 contract: the agent's namespace
// wins over the platform catalog, a version pin must match, and a name
// that resolves nowhere is REPORTED, never silently dropped.
func TestListRefSkills(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentofficev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		inlineSkill("seat", "local-only", "1.0.0", "# local"),
		inlineSkill("seat", "shadowed", "2.0.0", "# seat copy"),
		inlineSkill(platformCatalogNamespace, "shadowed", "1.0.0", "# catalog copy"),
		inlineSkill(platformCatalogNamespace, "openshift-docs", "1.0.0", "# baked"),
		inlineSkill(platformCatalogNamespace, "pinned", "1.1.1", "# pinned"),
	).Build()
	r := &AgentWorkstationReconciler{Client: cl, Scheme: scheme}
	aw := &agentofficev1alpha1.AgentWorkstation{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "seat"},
		Spec: agentofficev1alpha1.AgentWorkstationSpec{SkillRefs: []agentofficev1alpha1.SkillRef{
			{Name: "openshift-docs"}, {Name: "local-only"}, {Name: "shadowed"},
			{Name: "pinned", Version: "9.9.9"}, {Name: "ghost"}, {Name: "local-only"}, // dup ignored
		}},
	}
	resolved, unresolved := r.listRefSkills(context.Background(), aw)
	got := map[string]ResolvedSkill{}
	for _, rs := range resolved {
		got[rs.Skill.Name] = rs
	}
	if len(got) != 3 {
		t.Fatalf("resolved %d skills, want 3 (openshift-docs, local-only, shadowed): %v", len(got), keysOf(got))
	}
	if got["shadowed"].Skill.Namespace != "seat" || got["shadowed"].Version != "2.0.0" {
		t.Errorf("shadowed resolved from %s@%s, want the seat copy 2.0.0", got["shadowed"].Skill.Namespace, got["shadowed"].Version)
	}
	if got["openshift-docs"].Skill.Namespace != platformCatalogNamespace {
		t.Errorf("openshift-docs should resolve from the platform catalog")
	}
	joined := strings.Join(unresolved, " | ")
	if !strings.Contains(joined, "ghost (no such Skill") || !strings.Contains(joined, "pinned (pinned 9.9.9, catalog has 1.1.1)") {
		t.Errorf("unresolved refs not reported as expected: %q", joined)
	}
	if len(unresolved) != 2 {
		t.Errorf("unresolved = %d, want 2: %q", len(unresolved), joined)
	}
}

func keysOf(m map[string]ResolvedSkill) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
