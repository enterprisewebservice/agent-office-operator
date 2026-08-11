package controller

import "testing"

func TestDropContained(t *testing.T) {
	all := []catalogPack{
		{Name: "parkforge-brain", ArtifactKind: "meta-pack",
			Members: []string{"unreal-scripting", "parkforge-terrain"}},
		{Name: "unreal-scripting", ArtifactKind: "pack"},
		{Name: "parkforge-terrain", ArtifactKind: "pack"},
		{Name: "parkforge-unreal-blueprint-scripting", ArtifactKind: "skill", Member: "unreal-scripting"},
		{Name: "parkforge-terrain-strokes", ArtifactKind: "skill", Member: "parkforge-terrain"},
		{Name: "weekly-ops-report", ArtifactKind: "skill"},
	}
	pick := func(names ...string) []recommendPack {
		var out []recommendPack
		for _, n := range names {
			for _, p := range all {
				if p.Name == n {
					out = append(out, recommendPack{catalogPack: p})
				}
			}
		}
		return out
	}
	names := func(in []recommendPack) []string {
		var o []string
		for _, p := range in {
			o = append(o, p.Name)
		}
		return o
	}
	eq := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	// The exact case Dean hit.
	if g := names(dropContained(pick("parkforge-unreal-blueprint-scripting", "unreal-scripting"), all)); !eq(g, "unreal-scripting") {
		t.Errorf("pack+its skill: got %v", g)
	}
	// Meta-pack swallows a pack AND a grandchild skill.
	if g := names(dropContained(pick("parkforge-brain", "unreal-scripting", "parkforge-terrain-strokes"), all)); !eq(g, "parkforge-brain") {
		t.Errorf("meta-pack: got %v", g)
	}
	// Unrelated picks survive untouched.
	if g := names(dropContained(pick("weekly-ops-report", "parkforge-terrain-strokes"), all)); !eq(g, "weekly-ops-report", "parkforge-terrain-strokes") {
		t.Errorf("unrelated: got %v", g)
	}
	// Siblings are not containment.
	if g := names(dropContained(pick("unreal-scripting", "parkforge-terrain"), all)); !eq(g, "unreal-scripting", "parkforge-terrain") {
		t.Errorf("siblings: got %v", g)
	}
}
