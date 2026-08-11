package controller

import "testing"

func toolCatalog() []catalogPack {
	return []catalogPack{
		{Name: "supply-chain-ops", ArtifactKind: "pack", Registry: "mindifact.ai", Namespace: "agent-office",
			Members: nil},
		{Name: "weekly-ops-report", ArtifactKind: "skill", Member: "supply-chain-ops",
			Registry: "mindifact.ai", Namespace: "agent-office", ContentURL: "https://x/SKILL.md"},
		{Name: "ops-metrics", ArtifactKind: "tool", Member: "supply-chain-ops",
			Registry: "mindifact.ai", Namespace: "agent-office",
			Tool: &registryTool{Mode: "reference", RouteRef: "ops-metrics-mcp-server-route", Prefix: "metrics_"}},
		// A tool row with no definition must never be treated as installable.
		{Name: "broken-tool", ArtifactKind: "tool", Member: "supply-chain-ops", Registry: "mindifact.ai"},
	}
}

func TestResolveInstallSetIncludesTools(t *testing.T) {
	all := toolCatalog()
	got := resolveInstallSet(all, "supply-chain-ops")
	names := map[string]string{}
	for _, p := range got {
		names[p.Name] = p.ArtifactKind
	}
	if len(got) != 2 {
		t.Fatalf("pack should resolve to its skill AND its tool, got %d: %v", len(got), names)
	}
	if names["weekly-ops-report"] != "skill" {
		t.Errorf("missing the skill: %v", names)
	}
	if names["ops-metrics"] != "tool" {
		t.Errorf("missing the tool — this is the whole point of the chain: %v", names)
	}
	if _, bad := names["broken-tool"]; bad {
		t.Errorf("a tool with no definition must not be installable: %v", names)
	}
	// A tool asked for directly still resolves.
	if g := resolveInstallSet(all, "ops-metrics"); len(g) != 1 || g[0].Name != "ops-metrics" {
		t.Errorf("direct tool install: got %v", g)
	}
}

func TestInstallableGuards(t *testing.T) {
	if installable(catalogPack{ArtifactKind: "tool"}) {
		t.Error("tool without a definition must not be installable")
	}
	if installable(catalogPack{ArtifactKind: "skill"}) {
		t.Error("skill without content must not be installable")
	}
	if installable(catalogPack{ArtifactKind: "pack"}) {
		t.Error("a pack row installs nothing itself")
	}
	if !installable(catalogPack{ArtifactKind: "tool", Tool: &registryTool{}}) {
		t.Error("tool with a definition should be installable")
	}
}

func TestToolProvenanceCarriesRecipe(t *testing.T) {
	p := catalogPack{
		Name: "ops-metrics", Registry: "mindifact.ai", Namespace: "agent-office", Version: "0.1.0",
		DisplayName: "Ops Metrics", Description: "governed metrics",
		Recipe: &toolRecipe{URL: "https://gw/mcp", Type: "http"},
	}
	ann := toolProvenance(p)
	// gatherPacks reads this annotation back to rebuild the client recipe,
	// so an installed tool must look identical to a gitops-authored one.
	if ann["agentoffice.ai/client-recipe"] == "" {
		t.Error("client-recipe annotation missing; composer could not wire the tool")
	}
	if ann["agentoffice.ai/pack-ref"] != "agent-office/ops-metrics:0.1.0" {
		t.Errorf("pack-ref wrong: %q", ann["agentoffice.ai/pack-ref"])
	}
}
