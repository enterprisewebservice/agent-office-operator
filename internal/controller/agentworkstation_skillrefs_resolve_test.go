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

func labelledSkill(ns, name, skill, agent, version string) *agentofficev1alpha1.Skill {
	s := inlineSkill(ns, name, version, "# "+name)
	s.Labels = map[string]string{skillNameLabel: skill, skillAgentLabel: agent}
	return s
}

// TestListRefSkillsPerAgent pins the v1.7.82 contract: a hire's own Skill
// resources are named <agent>-<skill> and found through the skill-name
// label (the agent's own first, then a version match, then the first by
// name); the resolved entry carries the skill's logical name, which is
// the workspace folder; a plain-named Skill still resolves by name.
func TestListRefSkillsPerAgent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentofficev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		labelledSkill("seat", "assistant-golden", "golden", "assistant", "1.0.2"),
		labelledSkill("seat", "dev-golden", "golden", "dev", "1.0.6"),
		inlineSkill("seat", "triage", "1.1.1", "# hand-applied"),
	).Build()
	r := &AgentWorkstationReconciler{Client: cl, Scheme: scheme}
	resolve := func(agent, version string) (ResolvedSkill, []string, bool) {
		aw := &agentofficev1alpha1.AgentWorkstation{
			ObjectMeta: metav1.ObjectMeta{Name: agent, Namespace: "seat"},
			Spec: agentofficev1alpha1.AgentWorkstationSpec{SkillRefs: []agentofficev1alpha1.SkillRef{
				{Name: "golden", Version: version}, {Name: "triage", Version: "1.1.1"},
			}},
		}
		resolved, unresolved := r.listRefSkills(context.Background(), aw)
		for _, rs := range resolved {
			if rs.Skill.Name == "golden" {
				return rs, unresolved, true
			}
		}
		return ResolvedSkill{}, unresolved, false
	}
	// Each agent gets its own resource at its own pin: no shared resource, no fight.
	if rs, un, ok := resolve("assistant", "1.0.2"); !ok || rs.Version != "1.0.2" || len(un) != 0 {
		t.Errorf("assistant: want own golden@1.0.2, got ok=%v version=%q unresolved=%v", ok, rs.Version, un)
	}
	if rs, un, ok := resolve("dev", "1.0.6"); !ok || rs.Version != "1.0.6" || len(un) != 0 {
		t.Errorf("dev: want own golden@1.0.6, got ok=%v version=%q unresolved=%v", ok, rs.Version, un)
	}
	// An agent with no resource of its own borrows one that matches its pin.
	if rs, _, ok := resolve("other", "1.0.6"); !ok || rs.Version != "1.0.6" {
		t.Errorf("other@1.0.6: want the 1.0.6 resource, got ok=%v version=%q", ok, rs.Version)
	}
	// No pin: the first resource by name.
	if rs, _, ok := resolve("other", ""); !ok || rs.Version != "1.0.2" {
		t.Errorf("other unpinned: want assistant-golden (first by name), got ok=%v version=%q", ok, rs.Version)
	}
	// A pin nothing carries is reported, not silently swapped.
	if _, un, ok := resolve("other", "3.0.0"); ok || !strings.Contains(strings.Join(un, " "), "golden (pinned 3.0.0, catalog has 1.0.2)") {
		t.Errorf("other@3.0.0: want an unresolved report, got ok=%v unresolved=%v", ok, un)
	}
	// The logical name is what seeding and status use, never the resource name.
	rs, _, _ := resolve("dev", "1.0.6")
	if rs.Skill.Name != "golden" {
		t.Errorf("resolved name = %q, want the logical skill name", rs.Skill.Name)
	}
	// The hand-applied, plain-named Skill still resolves by name.
	aw := &agentofficev1alpha1.AgentWorkstation{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "seat"},
		Spec:       agentofficev1alpha1.AgentWorkstationSpec{SkillRefs: []agentofficev1alpha1.SkillRef{{Name: "triage", Version: "1.1.1"}}},
	}
	if resolved, un := r.listRefSkills(context.Background(), aw); len(resolved) != 1 || len(un) != 0 {
		t.Errorf("triage by name: resolved=%d unresolved=%v", len(resolved), un)
	}
}
