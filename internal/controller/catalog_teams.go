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
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// Which TEAM a new agent joins is a recommendation, not a form field.
//
// A gateway is the team: agents on one share a runtime, a browser node,
// a namespace and a blast radius. Asking someone to pick from a dropdown
// pushes a platform decision onto a person who described a job in a
// sentence — the same reason the pack list became a recommendation
// instead of a multi-select. The description that names "the nl2sql.ai
// journalist crew" is exactly the signal needed to route a reporter to
// it, and it is already written on the gateway.
//
// Selection is reported and NOT overridable in the composer. A wrong
// answer is a prompt or scoring bug to fix here, where it improves for
// everyone, rather than a click every user repeats forever.

// recommendTeam is the chosen gateway, with enough context for the
// composer to show what joining it means.
type recommendTeam struct {
	Gateway  string   `json:"gateway"`
	Team     string   `json:"team,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Members  []string `json:"members,omitempty"`
	Ready    bool     `json:"ready"`
	Existing bool     `json:"existing"`
}

// gatewayCandidate is one team the recommender may choose from.
type gatewayCandidate struct {
	Name        string
	Description string
	Team        string
	Ready       bool
	Members     []string
}

// gatherGateways lists the teams a new agent could join, each with the
// crew already on it — "who works here" is a stronger signal than any
// description, and it is what makes a wrong pick obvious on review.
func (h *CatalogSkillsHandler) gatherGateways(ctx context.Context) []gatewayCandidate {
	var gws agentofficev1alpha1.AgentGatewayList
	if err := h.client.List(ctx, &gws, client.InNamespace(h.namespace)); err != nil {
		return nil
	}
	var aws agentofficev1alpha1.AgentWorkstationList
	_ = h.client.List(ctx, &aws, client.InNamespace(h.namespace))

	members := map[string][]string{}
	teamOf := map[string]string{}
	for i := range aws.Items {
		a := &aws.Items[i]
		gw := effectiveGatewayRef(a)
		if gw == "" {
			continue
		}
		label := a.Name
		if a.Spec.Role != "" {
			label += " (" + a.Spec.Role + ")"
		}
		members[gw] = append(members[gw], label)
		if t := strings.TrimSpace(a.Spec.Team); t != "" && teamOf[gw] == "" {
			teamOf[gw] = t
		}
	}

	out := make([]gatewayCandidate, 0, len(gws.Items))
	for i := range gws.Items {
		g := &gws.Items[i]
		m := members[g.Name]
		sort.Strings(m)
		out = append(out, gatewayCandidate{
			Name:        g.Name,
			Description: strings.TrimSpace(g.Spec.Description),
			Team:        teamOf[g.Name],
			Ready:       g.Status.Phase == "Ready",
			Members:     m,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// teamLines renders the candidates for the model prompt.
func teamLines(cands []gatewayCandidate) string {
	var b strings.Builder
	for _, c := range cands {
		b.WriteString("- gateway \"" + c.Name + "\"")
		if !c.Ready {
			b.WriteString(" (NOT ready)")
		}
		if c.Description != "" {
			b.WriteString(": " + oneLine(c.Description))
		}
		b.WriteString("\n")
		if len(c.Members) > 0 {
			b.WriteString("    crew: " + strings.Join(c.Members, ", ") + "\n")
		} else {
			b.WriteString("    crew: (empty)\n")
		}
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ownTeamFor is the team policy since 2026-08-25: EVERY hire starts on
// its own gateway, named for the agent. Description-matching against
// existing crews put a workshop hire on the newsroom desk — sharing a
// runtime, a browser node, and a blast radius with an unrelated
// production crew. Joining an existing team is now a deliberate git
// edit (point runtime.shared.gatewayRef at the crew's gateway in the
// agent's gitops repo), not something a keyword score decides.
// pickTeamFallback/resolveTeam remain for the join-a-crew flow if a
// future composer surface offers it explicitly.
func ownTeamFor(agentSlug string) *recommendTeam {
	slug := sanitizeAgentName(strings.TrimSuffix(strings.TrimSpace(agentSlug), "-gateway"))
	if slug == "" {
		slug = "custom"
	}
	return &recommendTeam{
		Gateway:  slug + "-gateway",
		Existing: false,
		Ready:    false,
		Reason:   "each hire starts on its own gateway; joining an existing crew is a gitops edit",
	}
}

// pickTeamFallback chooses a gateway without a model: score the job
// description against each team's description and the names of the
// agents already on it. Ready gateways win ties; an empty catalog
// yields nothing rather than a guess.
func pickTeamFallback(desc string, cands []gatewayCandidate) *recommendTeam {
	if len(cands) == 0 {
		return nil
	}
	terms := tokenize(strings.ToLower(desc))
	best, bestScore := -1, 0.0
	for i, c := range cands {
		hay := strings.ToLower(c.Name + " " + c.Description + " " + c.Team + " " + strings.Join(c.Members, " "))
		score := 0.0
		for _, t := range terms {
			if len(t) < 4 {
				continue
			}
			if strings.Contains(hay, t) {
				score++
			}
		}
		if c.Ready {
			score += 0.5
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	// Nothing genuinely matched. Forcing the agent onto whichever
	// gateway happens to be Ready is how unrelated crews end up sharing
	// a runtime, a browser and a blast radius — the opposite of what a
	// team means. Propose a NEW one instead; that is a real answer, and
	// the composer shows it as such.
	//
	// bestScore counts term hits plus a 0.5 readiness nudge, so a score
	// at or under 0.5 means no term matched at all.
	if best < 0 || bestScore <= 0.5 {
		return newTeamFor(desc)
	}
	c := cands[best]
	return &recommendTeam{
		Gateway: c.Name, Team: c.Team, Ready: c.Ready, Members: c.Members,
		Existing: true,
		Reason:   "closest match to the described job among existing teams",
	}
}

// resolveTeam validates a model's gateway choice against reality. A
// name that is not a real gateway is dropped rather than passed to the
// template, where it would fail at create time.
func resolveTeam(name, reason string, cands []gatewayCandidate) *recommendTeam {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	for _, c := range cands {
		if c.Name == name {
			return &recommendTeam{
				Gateway: c.Name, Team: c.Team, Ready: c.Ready,
				Members: c.Members, Existing: true, Reason: reason,
			}
		}
	}
	// Not an existing gateway. Previously this was rejected as a
	// hallucination, which quietly removed "start a new team" from the
	// set of possible answers and crammed every unrelated agent onto
	// whatever already existed. A name that is not in the list is now
	// read as a PROPOSAL — sanitized, marked new, and created by the
	// template rather than referenced.
	slug := sanitizeAgentName(strings.TrimSuffix(name, "-gateway"))
	if slug == "" {
		return nil
	}
	return &recommendTeam{
		Gateway:  slug + "-gateway",
		Existing: false,
		Ready:    false,
		Reason:   reason,
	}
}

// newTeamFor proposes a gateway for a job that fits no existing crew.
// Named from the description so the team reads as what it is for, not
// as gateway-7.
func newTeamFor(desc string) *recommendTeam {
	terms := tokenize(strings.ToLower(desc))
	if len(terms) > 2 {
		terms = terms[:2]
	}
	slug := sanitizeAgentName(strings.Join(terms, "-"))
	if slug == "" {
		slug = "custom"
	}
	return &recommendTeam{
		Gateway:  slug + "-gateway",
		Existing: false,
		Ready:    false,
		Reason:   "no existing team does this work; starting a new one",
	}
}
