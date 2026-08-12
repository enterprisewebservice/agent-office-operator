package controller

import (
	"strings"
	"testing"
)

// The two real gateways on the cluster: their descriptions are the
// signal, and their crews are the confirmation.
func teamFixture() []gatewayCandidate {
	return []gatewayCandidate{
		{Name: "newsroom-gateway", Ready: true,
			Description: "Shared OpenClaw gateway for the nl2sql.ai journalist crew (anchor, wire, bench, scout).",
			Members:     []string{"nl2sql-anchor (editor)", "nl2sql-wire (reporter)"}},
		{Name: "research-gateway", Ready: true,
			Description: "Shared OpenClaw gateway for the Karpathy auto-research crew.",
			Members:     nil},
	}
}

func TestTeamFallbackRoutesByDescription(t *testing.T) {
	c := teamFixture()
	if got := pickTeamFallback("a reporter for the nl2sql journalist desk", c); got == nil || got.Gateway != "newsroom-gateway" {
		t.Errorf("journalist job should join the newsroom, got %+v", got)
	}
	if got := pickTeamFallback("an auto-research agent running Karpathy training loops", c); got == nil || got.Gateway != "research-gateway" {
		t.Errorf("research job should join research-gateway, got %+v", got)
	}
}

// No match must still yield a usable placement — the template needs a
// gateway or Create fails — but it must be a NEW team, not the nearest
// unrelated one. This originally asserted "always pick a Ready
// gateway", which is precisely the behaviour that crammed unrelated
// crews onto one runtime.
func TestNoMatchProposesANewTeamRatherThanNothing(t *testing.T) {
	got := pickTeamFallback("zzzqqq unmatchable xyzzy", teamFixture())
	if got == nil || got.Gateway == "" {
		t.Fatal("must still yield a gateway so Create has something to use")
	}
	if got.Existing {
		t.Errorf("must not join an unrelated existing crew: %+v", got)
	}
	if got.Ready {
		t.Error("a team that does not exist yet cannot be Ready")
	}
}

func TestNoGatewaysYieldsNoTeam(t *testing.T) {
	if got := pickTeamFallback("anything", nil); got != nil {
		t.Errorf("empty cluster must yield no team, got %+v", got)
	}
}

// An existing name resolves to that crew; an unfamiliar name is a
// PROPOSAL to create one. Rejecting unknown names (the original
// behaviour) silently removed "start a new team" from the answer space.
func TestResolveTeamDistinguishesExistingFromProposed(t *testing.T) {
	c := teamFixture()
	prop := resolveTeam("does-not-exist", "because", c)
	if prop == nil || prop.Existing {
		t.Errorf("unknown gateway should become a new-team proposal, got %+v", prop)
	}
	got := resolveTeam("newsroom-gateway", "journalist crew", c)
	if got == nil || got.Gateway != "newsroom-gateway" || got.Reason != "journalist crew" {
		t.Errorf("valid gateway should resolve with its reason, got %+v", got)
	}
	if len(got.Members) == 0 {
		t.Error("resolved team should carry the existing crew for review")
	}
}

// A job unrelated to every existing crew must start a NEW team, not get
// crammed onto whichever gateway happens to be Ready — unrelated crews
// sharing a runtime and blast radius is the thing teams exist to avoid.
func TestUnrelatedJobProposesANewTeam(t *testing.T) {
	got := pickTeamFallback("sculpt terrain and lay footpaths in an Unreal park", teamFixture())
	if got == nil {
		t.Fatal("expected a proposed team")
	}
	if got.Existing {
		t.Errorf("unrelated job must NOT join an existing crew, got %+v", got)
	}
	if got.Gateway == "" || !strings.HasSuffix(got.Gateway, "-gateway") {
		t.Errorf("proposed gateway name malformed: %q", got.Gateway)
	}
	if got.Ready {
		t.Error("a team that does not exist yet cannot be Ready")
	}
}

// A related job must still JOIN rather than proliferate gateways.
func TestRelatedJobStillJoinsExisting(t *testing.T) {
	got := pickTeamFallback("another reporter for the nl2sql journalist desk", teamFixture())
	if got == nil || !got.Existing || got.Gateway != "newsroom-gateway" {
		t.Errorf("related job should join the existing crew, got %+v", got)
	}
}

// The model naming a gateway that does not exist is a PROPOSAL now, not
// a hallucination to discard.
func TestUnknownGatewayFromModelBecomesANewTeam(t *testing.T) {
	got := resolveTeam("unreal-worlds-gateway", "3D world building is its own discipline", teamFixture())
	if got == nil {
		t.Fatal("unknown name should become a new-team proposal")
	}
	if got.Existing {
		t.Error("should be marked as NOT existing")
	}
	if got.Gateway != "unreal-worlds-gateway" {
		t.Errorf("name mangled: %q", got.Gateway)
	}
	// A model that appends -gateway twice must not produce
	// foo-gateway-gateway.
	if g := resolveTeam("foo-gateway", "x", teamFixture()); g == nil || g.Gateway != "foo-gateway" {
		t.Errorf("double suffix: %+v", g)
	}
}
