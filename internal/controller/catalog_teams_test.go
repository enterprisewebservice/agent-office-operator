package controller

import "testing"

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

// No match must still land somewhere real rather than emitting nothing,
// otherwise the template has no gateway and Create fails.
func TestTeamFallbackAlwaysPicksAReadyTeam(t *testing.T) {
	got := pickTeamFallback("zzzqqq unmatchable xyzzy", teamFixture())
	if got == nil || got.Gateway == "" {
		t.Fatal("must still choose a Ready gateway when nothing matches")
	}
	if !got.Ready {
		t.Errorf("chose a non-Ready gateway: %+v", got)
	}
}

func TestNoGatewaysYieldsNoTeam(t *testing.T) {
	if got := pickTeamFallback("anything", nil); got != nil {
		t.Errorf("empty cluster must yield no team, got %+v", got)
	}
}

// A model naming a gateway that does not exist would fail at create
// time, so it must be rejected rather than passed through.
func TestResolveTeamRejectsUnknownGateway(t *testing.T) {
	c := teamFixture()
	if got := resolveTeam("does-not-exist", "because", c); got != nil {
		t.Errorf("unknown gateway must be rejected, got %+v", got)
	}
	got := resolveTeam("newsroom-gateway", "journalist crew", c)
	if got == nil || got.Gateway != "newsroom-gateway" || got.Reason != "journalist crew" {
		t.Errorf("valid gateway should resolve with its reason, got %+v", got)
	}
	if len(got.Members) == 0 {
		t.Error("resolved team should carry the existing crew for review")
	}
}
