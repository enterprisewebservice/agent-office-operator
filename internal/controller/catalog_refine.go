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
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// POST /catalog/refine — the conversational half of the composer.
//
// /catalog/recommend answers ONCE: brief in, composition out. When the
// first attempt isn't quite right, the fix used to be Re-suggest — a
// wholesale regeneration that threw away everything the user liked
// along with the one thing they didn't. This endpoint is the
// alternative: the hiring form turns the brief box into a chat, and
// each turn arrives here carrying the WHOLE conversation plus the
// CURRENT composition.
//
//	{"description": "<original brief>",
//	 "current": {"identity": {...}, "packs": ["openshift-docs", ...], "brain": "GPT-5.6 Sol"},
//	 "messages": [{"role":"user","content":"remove find token"}, ...]}
//	→ {"source": "model:..."|"fallback",
//	   "reply": "<what the composer says back>",
//	   "identity": {...}, "packs": [<full recommendPack>...],
//	   "team": {...}, "changed": true}
//
// The model does NOT return a new composition. It returns OPS — add
// this pack, remove that one, set this identity field — and the server
// applies them mechanically to the current state. A model that drifts
// into rebuilding cannot: anything it does not name stays untouched,
// hallucinated names are dropped with a note, and the same
// completeSelection/dropContained closure that recommend runs keeps the
// result dependency-complete. Ops-empty turns are answers, not edits —
// "is there a skill for incident response?" gets a reply grounded in
// the same rendered catalog the recommender sees, and changes nothing.
//
// Engines mirror recommend exactly — gateway subscription turn, then
// the AGENT_RECOMMENDER_URL endpoint, then a deterministic fallback
// (verb + name matching for add/remove, keyword scoring for questions)
// — so a cluster with no recommender still holds a useful conversation.
type refineMessage struct {
	Role    string `json:"role"` // "user" | "assistant"
	Content string `json:"content"`
}

type refineCurrent struct {
	Identity recommendIdentity `json:"identity"`
	// Names only — the server re-resolves against the live catalog, so
	// a client cannot smuggle in a pack shape the catalog never served.
	Packs []string `json:"packs"`
	// Display label of the chosen brain. Informational: the composer
	// can talk about it, but brain changes ride the form's own picker,
	// not chat ops.
	Brain string `json:"brain,omitempty"`
}

type refineRequest struct {
	Description string          `json:"description"`
	Current     refineCurrent   `json:"current"`
	Messages    []refineMessage `json:"messages"`
}

type refineResponse struct {
	Source   string            `json:"source"`
	Reply    string            `json:"reply"`
	Identity recommendIdentity `json:"identity"`
	Packs    []recommendPack   `json:"packs"`
	Team     *recommendTeam    `json:"team,omitempty"`
	Changed  bool              `json:"changed"`
}

// refineOp is the only way a model turn can touch the composition.
type refineOp struct {
	Op     string `json:"op"`               // add | remove | set
	Name   string `json:"name,omitempty"`   // add/remove: catalog / selection name
	Reason string `json:"reason,omitempty"` // add: why, shown on the pack row
	Field  string `json:"field,omitempty"`  // set: name|displayName|emoji|role|systemPrompt
	Value  string `json:"value,omitempty"`  // set: the new value
}

type refineModelOut struct {
	Reply string     `json:"reply"`
	Ops   []refineOp `json:"ops"`
}

func (h *CatalogSkillsHandler) refine(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeCatalogJSONError(w, http.StatusMethodNotAllowed,
			fmt.Errorf("refine is POST"))
		return
	}
	// 128KB: a chat history, not a brief. Still bounded — the server
	// trims to the last turns below regardless of what arrives.
	body, err := io.ReadAll(io.LimitReader(r.Body, 128*1024))
	if err != nil {
		writeCatalogJSONError(w, http.StatusBadRequest, fmt.Errorf("reading body: %w", err))
		return
	}
	var req refineRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeCatalogJSONError(w, http.StatusBadRequest,
			fmt.Errorf("body must be {description, current, messages}: %w", err))
		return
	}
	// Trim history server-side: the model needs recency, not the whole
	// transcript, and the prompt already carries current state.
	if len(req.Messages) > 12 {
		req.Messages = req.Messages[len(req.Messages)-12:]
	}
	for i := range req.Messages {
		if len(req.Messages[i].Content) > 4000 {
			req.Messages[i].Content = req.Messages[i].Content[:4000]
		}
	}
	last := ""
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" {
		last = strings.TrimSpace(req.Messages[n-1].Content)
	}
	if last == "" {
		writeCatalogJSONError(w, http.StatusBadRequest,
			fmt.Errorf("messages must end with a non-empty user turn"))
		return
	}

	packs, err := h.gatherPacks(r.Context(), nil)
	if err != nil {
		writeCatalogJSONError(w, http.StatusInternalServerError, err)
		return
	}
	byName := map[string]catalogPack{}
	for _, p := range packs {
		byName[p.Name] = p
	}
	// Re-resolve the current selection against the live catalog. A name
	// that no longer resolves (uninstalled since the last turn) drops
	// out here — same freshness rule recommend lives by.
	cur := []recommendPack{}
	for _, n := range req.Current.Packs {
		if p, ok := byName[n]; ok {
			cur = append(cur, recommendPack{catalogPack: p})
		}
	}

	prompt := refinePrompt(req, cur, packs)

	var out refineModelOut
	source := ""
	if raw, model, gerr := h.runGatewayTurn(r.Context(), prompt); gerr == nil {
		if perr := json.Unmarshal([]byte(extractJSON(raw)), &out); perr == nil && out.Reply != "" {
			source = "model:" + model
		}
	} else if !strings.Contains(gerr.Error(), "not configured") {
		// Same logging posture as recommend: a failed turn is a fallback,
		// not a 500.
		_ = gerr
	}
	if source == "" {
		if url := os.Getenv("AGENT_RECOMMENDER_URL"); url != "" {
			if o, merr := refineViaModel(r.Context(), url, prompt); merr == nil {
				out, source = *o, "model"
			}
		}
	}
	if source == "" {
		reply, ops := refineFallback(last, cur, packs)
		out, source = refineModelOut{Reply: reply, Ops: ops}, "fallback"
	}

	id, next, notes, changed := applyRefineOps(req.Current.Identity, cur, out.Ops, byName)

	// The same closure recommend runs — but only when ops actually
	// touched the selection. A question-only turn must not mutate
	// anything, closure included.
	if changed {
		pre := map[string]bool{}
		for _, p := range next {
			pre[p.Name] = true
		}
		next = dropContained(completeSelection(next, packs), packs)
		for _, p := range next {
			if !pre[p.Name] {
				notes = append(notes, fmt.Sprintf("also pulled in %s (required)", p.Name))
			}
			delete(pre, p.Name)
		}
		for n := range pre {
			notes = append(notes, fmt.Sprintf("dropped %s — already inside a selected container", n))
		}
	}

	reply := strings.TrimSpace(out.Reply)
	if reply == "" {
		if changed {
			reply = "Done. Anything else to adjust?"
		} else {
			reply = "Nothing changed. Anything else?"
		}
	}
	if len(notes) > 0 {
		reply += " (" + strings.Join(notes, "; ") + ")"
	}

	writeCatalogJSON(w, http.StatusOK, refineResponse{
		Source:   source,
		Reply:    reply,
		Identity: id,
		Packs:    next,
		Team:     ownTeamFor(id.Name),
		Changed:  changed,
	})
}

// refinePrompt renders the whole situation — rules, current state, the
// same hierarchy-rendered catalog recommend uses, and the conversation
// — as ONE prompt string, shared verbatim by both model engines.
func refinePrompt(req refineRequest, cur []recommendPack, packs []catalogPack) string {
	var b strings.Builder
	b.WriteString("You are the composer behind a governed agent-hiring form, now in REFINEMENT mode.\n")
	b.WriteString("The user already has a composed agent (CURRENT COMPOSITION below). You adjust it\n")
	b.WriteString("through conversation — targeted edits, never a rebuild.\n\n")
	b.WriteString("Reply with ONE JSON object, no prose, no code fence:\n")
	b.WriteString(`{"reply":"<1-3 short sentences to the user>","ops":[...]}` + "\n")
	b.WriteString("where each op is one of:\n")
	b.WriteString(`  {"op":"add","name":"<exact catalog name>","reason":"<one clause>"}` + "\n")
	b.WriteString(`  {"op":"remove","name":"<a currently selected name>"}` + "\n")
	b.WriteString(`  {"op":"set","field":"name|displayName|emoji|role|systemPrompt","value":"..."}` + "\n\n")
	b.WriteString("RULES:\n" +
		"- Edit ONLY what the user's latest message asks for; everything else stays exactly as it is. " +
		"\"ops\":[] is the correct answer to a pure question.\n" +
		"- Questions about capabilities (\"is there a skill for X?\") are answered from the CATALOG in reply: " +
		"name the real entries and what they do, and offer to add them — but do not add until asked. " +
		"If the catalog has nothing for it, say so plainly; never invent an entry.\n" +
		"- add may only name CATALOG entries; remove may only name CURRENT selections.\n" +
		"- The catalog is a HIERARCHY: a pack installs everything it CONTAINS, so never add something " +
		"already inside a current selection, and prefer the smallest entry that covers the request. " +
		"REQUIRES is a separate graph; the platform auto-completes missing requirements after your ops.\n" +
		"- The brain is chosen in the form's own picker, not here — if asked to change the model, " +
		"say to pick it in the Brain section below the constellation.\n" +
		"- systemPrompt edits preserve the existing prompt's voice and its never-fabricate rule; " +
		"change only what was asked.\n" +
		"- When you change something, reply confirms it in one sentence and asks if anything else " +
		"needs adjusting.\n\n")
	b.WriteString("CURRENT COMPOSITION:\n")
	id := req.Current.Identity
	fmt.Fprintf(&b, "identity: name=%s · displayName=%s · emoji=%s · role=%s\n",
		id.Name, id.DisplayName, id.Emoji, id.Role)
	fmt.Fprintf(&b, "systemPrompt: %s\n", id.SystemPrompt)
	if req.Current.Brain != "" {
		fmt.Fprintf(&b, "brain: %s\n", req.Current.Brain)
	}
	b.WriteString("selected:\n")
	if len(cur) == 0 {
		b.WriteString("  (nothing selected)\n")
	}
	for _, p := range cur {
		kind := p.Type
		if p.ArtifactKind != "" {
			kind = p.ArtifactKind
		}
		fmt.Fprintf(&b, "  - %s (%s)\n", p.Name, kind)
	}
	b.WriteString("\nCATALOG:\n")
	b.WriteString(renderCatalogLines(packs))
	b.WriteString("\n\nORIGINAL JOB DESCRIPTION:\n")
	b.WriteString(req.Description)
	b.WriteString("\n\nCONVERSATION (latest last):\n")
	for _, m := range req.Messages {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		fmt.Fprintf(&b, "%s: %s\n", role, m.Content)
	}
	return b.String()
}

// refineViaModel sends the shared prompt to the AGENT_RECOMMENDER_URL
// chat-completions endpoint — the same second engine recommend has.
func refineViaModel(ctx context.Context, url, prompt string) (*refineModelOut, error) {
	payload, _ := json.Marshal(map[string]any{
		"model": os.Getenv("AGENT_RECOMMENDER_MODEL"),
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
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
		return nil, fmt.Errorf("refiner %d: %s", hres.StatusCode, rb[:min(len(rb), 120)])
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rb, &completion); err != nil || len(completion.Choices) == 0 {
		return nil, fmt.Errorf("refiner: unparseable completion")
	}
	var out refineModelOut
	if err := json.Unmarshal([]byte(extractJSON(completion.Choices[0].Message.Content)), &out); err != nil {
		return nil, fmt.Errorf("refiner: bad ops JSON: %w", err)
	}
	if out.Reply == "" {
		return nil, fmt.Errorf("refiner: empty reply")
	}
	return &out, nil
}

// applyRefineOps applies model ops to the current state — mechanically,
// so a drifting model cannot rebuild what it was asked to touch up.
// Invalid ops become notes, never errors: the conversation absorbs
// them ("no catalog entry named q") instead of a 500 eating the turn.
func applyRefineOps(
	id recommendIdentity, cur []recommendPack, ops []refineOp, byName map[string]catalogPack,
) (recommendIdentity, []recommendPack, []string, bool) {
	next := append([]recommendPack{}, cur...)
	var notes []string
	changed := false

	selected := func(name string) int {
		for i, p := range next {
			if p.Name == name {
				return i
			}
		}
		return -1
	}

	for _, op := range ops {
		switch op.Op {
		case "add":
			if selected(op.Name) >= 0 {
				notes = append(notes, fmt.Sprintf("%s was already selected", op.Name))
				continue
			}
			p, ok := byName[op.Name]
			if !ok {
				notes = append(notes, fmt.Sprintf("no catalog entry named %s", op.Name))
				continue
			}
			reason := op.Reason
			if reason == "" {
				reason = "added in refinement"
			}
			next = append(next, recommendPack{catalogPack: p, Reason: reason})
			changed = true
		case "remove":
			i := selected(op.Name)
			if i < 0 {
				notes = append(notes, fmt.Sprintf("%s wasn't in the selection", op.Name))
				continue
			}
			next = append(next[:i], next[i+1:]...)
			changed = true
		case "set":
			v := strings.TrimSpace(op.Value)
			switch op.Field {
			case "name":
				if s := sanitizeAgentName(v); s != "" && s != id.Name {
					id.Name, changed = s, true
				} else if sanitizeAgentName(v) == "" {
					notes = append(notes, fmt.Sprintf("%q doesn't sanitize to a usable name", op.Value))
				}
			case "displayName":
				if v != "" && v != id.DisplayName {
					id.DisplayName, changed = v, true
				}
			case "emoji":
				if v != "" && v != id.Emoji {
					id.Emoji, changed = v, true
				}
			case "role":
				if v != "" && v != id.Role {
					id.Role, changed = v, true
				}
			case "systemPrompt":
				if v != "" && v != id.SystemPrompt {
					id.SystemPrompt, changed = v, true
				}
			default:
				notes = append(notes, fmt.Sprintf("unknown field %q", op.Field))
			}
		default:
			notes = append(notes, fmt.Sprintf("unknown op %q", op.Op))
		}
	}
	return id, next, notes, changed
}

// ---- deterministic fallback -----------------------------------------
//
// No recommender configured (or both engines failed) still gets a
// working conversation: explicit add/remove verbs are matched against
// real names, and anything else is treated as a catalog search whose
// top matches come back in the reply.

var removeVerbs = []string{"remove ", "drop ", "delete ", "take out ", "get rid of ", "without "}
var addVerbs = []string{"add ", "include ", "install ", "put in ", "give it ", "attach "}

func refineFallback(lastUser string, cur []recommendPack, packs []catalogPack) (string, []refineOp) {
	msg := strings.ToLower(lastUser)
	terms := tokenize(lastUser)

	// Token-overlap match against a specific list of names.
	bestMatch := func(names []string) string {
		best, bestHits := "", 0
		for _, n := range names {
			hits := 0
			hay := strings.ToLower(strings.ReplaceAll(n, "-", " "))
			for _, t := range terms {
				if strings.Contains(hay, t) {
					hits++
				}
			}
			if hits > bestHits {
				best, bestHits = n, hits
			}
		}
		return best
	}

	if containsAny(msg, removeVerbs...) {
		var names []string
		for _, p := range cur {
			names = append(names, p.Name)
		}
		if hit := bestMatch(names); hit != "" {
			return fmt.Sprintf("Removed %s. Anything else?", hit),
				[]refineOp{{Op: "remove", Name: hit}}
		}
		return fmt.Sprintf("I couldn't match that to the current selection (%s). Name one of those to remove it.",
			strings.Join(func() []string {
				var ns []string
				for _, p := range cur {
					ns = append(ns, p.Name)
				}
				return ns
			}(), ", ")), nil
	}

	if containsAny(msg, addVerbs...) {
		var names []string
		for _, p := range packs {
			names = append(names, p.Name)
		}
		if hit := bestMatch(names); hit != "" {
			return fmt.Sprintf("Added %s. Anything else?", hit),
				[]refineOp{{Op: "add", Name: hit, Reason: "you asked for it"}}
		}
		return "I couldn't match that to anything in the catalog. Ask me what's available for a topic and I'll list the closest entries.", nil
	}

	// A question: score the catalog against the message, name the top
	// matches, change nothing.
	type scored struct {
		p    catalogPack
		hits int
	}
	var all []scored
	for _, p := range packs {
		hay := strings.ToLower(p.Name + " " + p.DisplayName + " " + p.Description)
		hits := 0
		for _, t := range terms {
			if strings.Contains(hay, t) {
				hits++
			}
		}
		if hits > 0 {
			all = append(all, scored{p, hits})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].hits > all[j].hits })
	if len(all) == 0 {
		return "Nothing in the catalog matches that. The selection is unchanged — anything else?", nil
	}
	if len(all) > 3 {
		all = all[:3]
	}
	var lines []string
	for _, s := range all {
		d := s.p.Description
		if len(d) > 90 {
			d = d[:90] + "…"
		}
		lines = append(lines, fmt.Sprintf("%s — %s", s.p.Name, d))
	}
	return fmt.Sprintf("Closest catalog matches: %s. Say \"add <name>\" to include one.",
		strings.Join(lines, " · ")), nil
}
