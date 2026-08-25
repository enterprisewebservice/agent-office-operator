/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"time"
)

// POST /catalog/recommend — the one-step composer's brain.
//
//	{"description": "I need weekly ops reports from our orders data"}
//	→ {"source": "model"|"fallback",
//	   "identity": {name, displayName, emoji, role, systemPrompt},
//	   "packs":    [<full catalogPack> + reason]}
//
// Each pack is the WHOLE catalog entry — tool recipes and live-enriched
// skill dependencies included — so the composer wires an agent from
// this ONE response. An earlier revision returned names only and had
// the client re-resolve them against /catalog/packs; when that second
// call lagged, the agent was created with an empty toolset and nothing
// said so.
//
// The caller (the AgentGenesis scaffolder field) turns this into a
// complete AgentWorkstation: identity from `identity`, compose wiring
// from `packs` (skills bring their dependencies), everything else
// defaulted. The user's remaining choices are the brain (codex or
// claude) and its existing auth path — nothing else.
//
// Two selection engines, same contract:
//
//   - MODEL: when AGENT_RECOMMENDER_URL is set (an OpenAI-compatible
//     chat-completions endpoint; AGENT_RECOMMENDER_MODEL names the
//     model, AGENT_RECOMMENDER_TOKEN optionally bears auth), the
//     catalog is inlined into the prompt and the model chooses. The
//     model may ONLY choose from the catalog: its output is validated
//     name-by-name and hallucinated packs are dropped. If the call
//     fails, times out (20s), or returns nothing usable, the fallback
//     answers instead — the one-step flow never hard-depends on
//     inference being up.
//
//   - FALLBACK: deterministic keyword scoring of the description
//     against pack name/displayName/description. Not clever, but
//     honest, instant, and correct on a small catalog.
//
// Set the env on the OLM-managed operator the OLM way:
//
//	spec.config.env on the Subscription (survives upgrades), e.g.
//	  - name: AGENT_RECOMMENDER_URL
//	    value: https://<endpoint>/v1/chat/completions
//	  - name: AGENT_RECOMMENDER_MODEL
//	    value: <model-id>
type recommendRequest struct {
	Description string `json:"description"`
}

type recommendIdentity struct {
	Name         string `json:"name"`
	DisplayName  string `json:"displayName"`
	Emoji        string `json:"emoji,omitempty"`
	Role         string `json:"role"`
	SystemPrompt string `json:"systemPrompt"`
}

// recommendPack is the full catalog entry plus the reason it was
// chosen. Deliberately the WHOLE pack (recipe, dependencies) rather
// than a name the client must re-resolve: the one-step composer wires
// its agent from this single response, so a failed or slow second
// lookup can never silently produce an agent with no tools.
type recommendPack struct {
	catalogPack
	Reason string `json:"reason,omitempty"`
}

type recommendResponse struct {
	Source   string            `json:"source"`
	Identity recommendIdentity `json:"identity"`
	Packs    []recommendPack   `json:"packs"`
	// Team — which gateway the agent joins. Chosen, not offered: the
	// composer displays it and does not let the user override, for the
	// same reason the pack list is not a multi-select.
	Team *recommendTeam `json:"team,omitempty"`
}

func (h *CatalogSkillsHandler) recommend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeCatalogJSONError(w, http.StatusMethodNotAllowed,
			fmt.Errorf("recommend is POST"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024))
	if err != nil {
		writeCatalogJSONError(w, http.StatusBadRequest, fmt.Errorf("reading body: %w", err))
		return
	}
	var req recommendRequest
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Description) == "" {
		writeCatalogJSONError(w, http.StatusBadRequest,
			fmt.Errorf("body must be {\"description\": \"...\"}"))
		return
	}
	desc := strings.TrimSpace(req.Description)

	packs, err := h.gatherPacks(r.Context(), nil)
	if err != nil {
		writeCatalogJSONError(w, http.StatusInternalServerError, err)
		return
	}

	// Which team the agent joins is recommended alongside the packs, so
	// the composer never has to render a gateway dropdown.
	teams := h.gatherGateways(r.Context())

	// 1. A real model turn through the gateway, on the ChatGPT/Codex
	//    subscription — no API key, no per-token billing.
	if resp, err := h.recommendViaGateway(r.Context(), desc, packs, teams); err == nil {
		resp.Team = ownTeamFor(resp.Identity.Name)
		writeCatalogJSON(w, http.StatusOK, resp)
		return
	} else if !strings.Contains(err.Error(), "not configured") {
		logf.FromContext(r.Context()).V(1).Info("gateway recommender failed", "err", err.Error())
	}
	// 2. An OpenAI-compatible endpoint, if one is configured.
	if url := os.Getenv("AGENT_RECOMMENDER_URL"); url != "" {
		if resp, err := recommendViaModel(r.Context(), url, desc, packs); err == nil {
			resp.Team = ownTeamFor(resp.Identity.Name)
			writeCatalogJSON(w, http.StatusOK, resp)
			return
		}
	}
	// 3. Deterministic scoring — always available.
	writeCatalogJSON(w, http.StatusOK, recommendFallback(desc, packs, teams))
}

// ---- model engine ---------------------------------------------------

func recommendViaModel(ctx context.Context, url, desc string, packs []catalogPack) (*recommendResponse, error) {
	var lines []string
	for _, p := range packs {
		lines = append(lines, fmt.Sprintf("- %s %q: %s", p.Type, p.Name, p.Description))
	}
	sys := "You compose governed AI agents from a fixed catalog. Given a job description, " +
		"choose ONLY from the catalog below (never invent entries) and draft an identity. " +
		"Respond with a single JSON object, no prose:\n" +
		"{\"name\":\"<dns-safe-short-name>\",\"displayName\":\"...\",\"emoji\":\"<one emoji>\"," +
		"\"role\":\"<one word>\",\"systemPrompt\":\"<2-4 sentences: the job, which governed " +
		"tools/skills to lean on, and: never fabricate data — if a tool cannot answer, say so>\"," +
		"\"packs\":[{\"name\":\"<catalog name>\",\"reason\":\"<why, one clause>\"}]}\n\nCATALOG:\n" +
		strings.Join(lines, "\n")

	payload, _ := json.Marshal(map[string]any{
		"model": os.Getenv("AGENT_RECOMMENDER_MODEL"),
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": desc},
		},
		"temperature": 0.2,
	})
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	hreq, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("AGENT_RECOMMENDER_TOKEN"); tok != "" {
		hreq.Header.Set("Authorization", "Bearer "+tok)
	}
	hres, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer hres.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(hres.Body, 256*1024))
	if hres.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("recommender %d: %s", hres.StatusCode, rb[:min(len(rb), 120)])
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rb, &completion); err != nil || len(completion.Choices) == 0 {
		return nil, fmt.Errorf("recommender: unparseable completion")
	}
	var out struct {
		recommendIdentity
		Packs []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"packs"`
	}
	if err := json.Unmarshal([]byte(extractJSON(completion.Choices[0].Message.Content)), &out); err != nil {
		return nil, fmt.Errorf("recommender: bad selection JSON: %w", err)
	}

	// Constrained choice enforced here: anything not in the catalog is
	// dropped silently, and an empty survivor set is a model failure.
	byName := map[string]catalogPack{}
	for _, p := range packs {
		byName[p.Name] = p
	}
	// Initialized, never nil: a nil slice marshals to `null` and every
	// client that iterates the field crashes on it.
	chosen := []recommendPack{}
	for _, sel := range out.Packs {
		if p, ok := byName[sel.Name]; ok {
			chosen = append(chosen, recommendPack{catalogPack: p, Reason: sel.Reason})
		}
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("recommender: no valid packs selected")
	}
	id := out.recommendIdentity
	id.Name = sanitizeAgentName(id.Name)
	if id.Name == "" || id.SystemPrompt == "" {
		return nil, fmt.Errorf("recommender: incomplete identity")
	}
	if id.Role == "" {
		id.Role = "assistant"
	}
	return &recommendResponse{Source: "model", Identity: id, Packs: chosen}, nil
}

// ---- deterministic fallback ----------------------------------------

var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "our": true, "with": true,
	"that": true, "this": true, "from": true, "need": true, "want": true,
	"will": true, "agent": true, "have": true, "gets": true, "get": true,
	"can": true, "should": true, "data": true,
}

// tokenize reduces a job description to its meaningful, de-duplicated
// terms. Shared so team scoring and pack scoring judge a description
// the same way.
func tokenize(desc string) []string {
	words := regexp.MustCompile(`[a-zA-Z][a-zA-Z-]+`).FindAllString(strings.ToLower(desc), -1)
	var terms []string
	seen := map[string]bool{}
	for _, w := range words {
		if len(w) > 2 && !stopwords[w] && !seen[w] {
			seen[w] = true
			terms = append(terms, w)
		}
	}
	return terms
}

func recommendFallback(desc string, packs []catalogPack, teams []gatewayCandidate) *recommendResponse {
	terms := tokenize(desc)

	// Weight each term by how RARE it is in this catalog (inverse document
	// frequency). Plain substring counting treats "report" and "terrain"
	// as equally informative, so a description about weekly ops reports
	// pulled in genesis-train — which mentions "report" once and has
	// nothing to do with the job. A term appearing in most packs says
	// almost nothing about which pack you want; a term in one or two says
	// almost everything.
	df := map[string]int{}
	for _, t := range terms {
		for _, p := range packs {
			if strings.Contains(strings.ToLower(p.Name+" "+p.DisplayName+" "+p.Description), t) {
				df[t]++
			}
		}
	}
	n := float64(len(packs))
	idf := func(t string) float64 {
		d := float64(df[t])
		if d <= 0 {
			return 0
		}
		return math.Log(1 + n/d) // rare term -> high weight, ubiquitous -> ~0
	}

	type scored struct {
		p     catalogPack
		score float64
		hits  []string
	}
	var all []scored
	for _, p := range packs {
		nameHay := strings.ToLower(p.Name + " " + p.DisplayName)
		descHay := strings.ToLower(p.Description)
		sc := 0.0
		var hits []string
		for _, t := range terms {
			w := idf(t)
			if w == 0 {
				continue
			}
			switch {
			case strings.Contains(nameHay, t):
				sc += w * 3 // the name is the strongest signal there is
				hits = append(hits, t)
			case strings.Contains(descHay, t):
				sc += w
				hits = append(hits, t)
			}
		}
		if sc > 0 {
			all = append(all, scored{p, sc, hits})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })

	// Quotas are a ceiling, never a target. The old code filled its 2
	// skills / 2 tools / 1 kb whatever the runner-up scored, so a
	// single weak word match always rode along. Anything scoring under
	// a third of the best hit is noise and is dropped, even if that
	// leaves one result — or none.
	cutoff := 0.0
	if len(all) > 0 {
		cutoff = all[0].score / 3
	}

	// Coverage, not a top-2. Every federated mindifact is typed "skill",
	// so a cap of 2 meant this path could never equip an agent for a job
	// spanning more than two capabilities no matter how well the rest
	// scored. The relevance cutoff above is what keeps noise out; the
	// cap is only a backstop against a pathological catalog.
	perType := map[string]int{}
	limit := map[string]int{"skill": 6, "tool": 3, "kb": 2}
	chosen := []recommendPack{}
	for _, s := range all {
		if s.score < cutoff {
			break // sorted desc: everything after this is weaker still
		}
		if perType[s.p.Type] >= limit[s.p.Type] {
			continue
		}
		perType[s.p.Type]++
		chosen = append(chosen, recommendPack{
			catalogPack: s.p,
			Reason:      fmt.Sprintf("matched: %s", strings.Join(s.hits, ", ")),
		})
	}

	// Same containment rule as the model path: scoring a pack and one of
	// its skills both highly is easy, and installing the pack already
	// brings the skill.
	chosen = dropContained(completeSelection(chosen, packs), packs)

	// Identity from the description itself — plain, predictable. A brief
	// that opens with the "You are <name>, ..." convention (what the hire
	// form's placeholder and the workshop guide both teach) declares the
	// name outright; honor it. Only briefs without a declared name fall
	// back to the leading terms ("You are user1-assistant, ..." used to
	// slug into "you-are-user").
	slug := ""
	if m := declaredNameRe.FindStringSubmatch(desc); m != nil {
		cand := strings.ToLower(m[1])
		// "You are an assistant..." declares no name — articles and other
		// stopwords fall through to the terms-based fallback.
		if len(cand) >= 3 && !stopwords[cand] {
			slug = sanitizeAgentName(cand)
		}
	}
	if slug == "" {
		base := terms
		if len(base) > 3 {
			base = base[:3]
		}
		slug = sanitizeAgentName(strings.Join(base, "-"))
	}
	if slug == "" {
		slug = "custom-agent"
	}
	display := strings.Title(strings.ReplaceAll(slug, "-", " "))

	role := "assistant"
	switch {
	case containsAny(desc, "report", "summary", "metrics", "ops"):
		role = "reporter"
	case containsAny(desc, "build", "code", "develop", "service", "deploy"):
		role = "developer"
	case containsAny(desc, "research", "investigate", "find", "browse"):
		role = "researcher"
	}

	var packNames []string
	for _, c := range chosen {
		packNames = append(packNames, c.Name)
	}
	prompt := fmt.Sprintf(
		"You are %s. Your job: %s. Work through the platform's governed tools and skills",
		display, desc)
	if len(packNames) > 0 {
		prompt += fmt.Sprintf(" — start with: %s", strings.Join(packNames, ", "))
	}
	prompt += ". Never fabricate figures or results: if a tool cannot answer, say exactly that."

	return &recommendResponse{
		Team:   ownTeamFor(slug),
		Source: "fallback",
		Identity: recommendIdentity{
			Name: slug, DisplayName: display, Emoji: "🤖",
			Role: role, SystemPrompt: prompt,
		},
		Packs: chosen,
	}
}

var nameRe = regexp.MustCompile(`[^a-z0-9-]+`)

// declaredNameRe captures an explicitly named identity at the head of a
// job brief: "You are user1-assistant, ..." → "user1-assistant".
var declaredNameRe = regexp.MustCompile(`(?i)^\s*you\s+are\s+([a-zA-Z0-9][a-zA-Z0-9_-]*)`)

// sanitizeAgentName forces a DNS-1123-safe, <=30 char name.
func sanitizeAgentName(s string) string {
	s = nameRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	s = strings.Trim(s, "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if len(s) > 30 {
		s = strings.Trim(s[:30], "-")
	}
	return s
}

func containsAny(s string, subs ...string) bool {
	l := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
