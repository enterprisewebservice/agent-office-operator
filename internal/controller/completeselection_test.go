package controller

import "testing"

// Mirrors the live parkforge graph: every member pack requires another,
// and only the meta-pack is self-contained.
func parkforgeCatalog() []catalogPack {
	req := func(ns ...string) []catalogRequirement {
		var o []catalogRequirement
		for _, n := range ns {
			o = append(o, catalogRequirement{Name: n, Range: "[1.6,2)", Satisfied: false})
		}
		return o
	}
	return []catalogPack{
		{Name: "parkforge-brain", ArtifactKind: "meta-pack",
			Members: []string{"parkforge-core", "parkforge-terrain", "parkforge-characters", "parkforge-cinematics", "unreal-scripting"}},
		{Name: "parkforge-core", ArtifactKind: "pack"},
		{Name: "unreal-scripting", ArtifactKind: "pack"},
		{Name: "parkforge-terrain", ArtifactKind: "pack", PackRequires: req("parkforge-core")},
		{Name: "parkforge-characters", ArtifactKind: "pack", PackRequires: req("parkforge-core", "unreal-scripting")},
		{Name: "parkforge-cinematics", ArtifactKind: "pack", PackRequires: req("parkforge-core", "unreal-scripting", "parkforge-characters")},
		{Name: "parkforge-terrain-strokes", ArtifactKind: "skill", Member: "parkforge-terrain"},
		{Name: "parkforge-unreal-blueprint-scripting", ArtifactKind: "skill", Member: "unreal-scripting"},
		{Name: "weekly-ops-report", ArtifactKind: "skill"},
	}
}

func resolve(t *testing.T, names ...string) []string {
	t.Helper()
	all := parkforgeCatalog()
	var chosen []recommendPack
	for _, n := range names {
		for _, p := range all {
			if p.Name == n {
				chosen = append(chosen, recommendPack{catalogPack: p})
			}
		}
	}
	got := dropContained(completeSelection(chosen, all), all)
	var out []string
	for _, p := range got {
		out = append(out, p.Name)
	}
	return out
}

func eqSet(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

func TestCompleteSelection(t *testing.T) {
	// The case Dean hit: a terrain skill alone leaves parkforge-core
	// missing, so it must promote to the self-contained meta-pack.
	if g := resolve(t, "parkforge-terrain-strokes"); !eqSet(g, "parkforge-brain") {
		t.Errorf("terrain skill: got %v, want [parkforge-brain]", g)
	}
	// A member pack with an unmet requirement promotes too.
	if g := resolve(t, "parkforge-terrain"); !eqSet(g, "parkforge-brain") {
		t.Errorf("terrain pack: got %v, want [parkforge-brain]", g)
	}
	// unreal-scripting is a ROOT — no requires — so it must NOT balloon.
	if g := resolve(t, "unreal-scripting"); !eqSet(g, "unreal-scripting") {
		t.Errorf("root pack: got %v, want [unreal-scripting]", g)
	}
	if g := resolve(t, "parkforge-unreal-blueprint-scripting"); !eqSet(g, "parkforge-unreal-blueprint-scripting") {
		t.Errorf("root's skill: got %v, want itself", g)
	}
	// Unrelated picks are never touched.
	if g := resolve(t, "weekly-ops-report"); !eqSet(g, "weekly-ops-report") {
		t.Errorf("unrelated: got %v", g)
	}
	// Deep chain: cinematics needs three, still one self-contained answer.
	if g := resolve(t, "parkforge-cinematics"); !eqSet(g, "parkforge-brain") {
		t.Errorf("cinematics: got %v, want [parkforge-brain]", g)
	}
	// Mixed: parkforge work completes, the unrelated skill survives.
	if g := resolve(t, "parkforge-terrain", "weekly-ops-report"); !eqSet(g, "parkforge-brain", "weekly-ops-report") {
		t.Errorf("mixed: got %v", g)
	}
}
