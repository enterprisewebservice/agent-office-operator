/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

// Strategist runner — Phase 2b of the autoresearch flywheel
// refactor.
//
// The strategist is the LLM agent's NEW role. Pre-refactor, the
// agent was on the proposal critical path: every round, ask the
// agent for a config, fall back to a starter when it was sick. Now
// the deterministic searcher (internal/search) owns proposals; the
// strategist runs on cadence + stagnation triggers and produces
// `HYPOTHESES.md` — a YAML doc the searcher reads to bias its
// sampling.
//
// Design rules:
//
//   - Strategist is OFF the critical path. The loop runs without
//     it. If it fails (timeout, malformed reply, openclaw down),
//     log + set StrategistOK=False, do NOT block the round.
//
//   - The strategist's only output channel is `HYPOTHESES.md`
//     in the wiki repo. Side effects in other directories
//     (raw/clips/, topics/, concepts/, queries/) come from the
//     wiki-* skills bound to the strategist AgentWorkstation
//     (configured in Phase 2c via SkillBinding) — those are NOT
//     parsed by the operator, they're for human readers + the
//     strategist's own future turns.
//
//   - One transaction per turn. The strategist commits exactly
//     one wiki commit ("strategist: HYPOTHESES.md after round N")
//     containing the updated HYPOTHESES.md. The skill-driven
//     side effects (clips, topics) happen via separate skill-
//     invoked git operations that are not part of this commit.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
	"github.com/enterprisewebservice/agent-office-operator/internal/search"
)

// strategistTimeout caps how long the operator waits for one
// strategist turn. The LLM gets to do real research (multiple
// wiki-clip / wiki-write tool calls) so the budget is generous;
// anything past this is treated as a stuck call.
const strategistTimeout = 15 * time.Minute

// maybeRunStrategist is the cheap entrypoint the reconcile loop
// calls every round. Opens a wiki transaction to read history +
// current HYPOTHESES.md, evaluates the cadence/stagnation gates,
// and runs the strategist turn iff due. Returns an error ONLY for
// wiki-read failures (so the controller knows to surface it);
// strategist turn failures are recorded on Status and not returned.
func (r *AutoResearchProjectReconciler) maybeRunStrategist(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
) error {
	// One transaction for the cadence-evaluation read. We don't
	// reuse the proposal transaction because (a) it doesn't exist
	// at this point in the reconcile, (b) the strategist's commit
	// (HYPOTHESES.md, if it runs) needs to land BEFORE
	// requestProposal opens its own transaction so the searcher
	// sees the new file.
	tx, err := r.beginWikiTx(ctx, p)
	if err != nil {
		return fmt.Errorf("open wiki for strategist gate: %w", err)
	}
	defer tx.Close()

	history, err := search.LoadWikiHistory(tx, defaultQLoRASpace())
	if err != nil {
		return fmt.Errorf("load wiki history: %w", err)
	}

	hypothesesBody, err := tx.Read("HYPOTHESES.md")
	if err != nil {
		return fmt.Errorf("read HYPOTHESES.md: %w", err)
	}
	current, _ := search.ParseHypotheses(hypothesesBody) // ignore parse error — we'll either run strategist or not, the searcher logs malformed body separately

	// Now decide + run. runStrategistIfDue handles its own wiki
	// open for the commit-and-push (separate transaction from
	// this read-only one).
	_ = r.runStrategistIfDue(ctx, p, history, current)
	return nil
}

// runStrategistIfDue runs one strategist turn iff the cadence /
// stagnation gates have fired. Returns the reason it ran (for
// logging) or "" if it skipped. Errors are surfaced on
// StrategistOK status — caller doesn't need to do anything with
// them.
//
// Inputs not currently spec-driven (still in code constants in
// v0.3.0): `everyN` (cadence in rounds), `stagN` (stagnation
// threshold). v0.4.0 promotes these to `spec.strategist.cadence`
// CRD fields.
func (r *AutoResearchProjectReconciler) runStrategistIfDue(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
	history []search.Observation,
	currentHypotheses search.Hypotheses,
) string {
	lg := log.FromContext(ctx)
	everyN := defaultStrategistEveryN
	stagN := defaultStrategistOnStagnation

	reason := search.StrategistDueReason(history, currentHypotheses, everyN, stagN)
	if reason == "" {
		return ""
	}
	lg.Info("strategist due — running turn", "reason", reason, "round", len(history))

	agentName := p.Spec.Experimenter.Workstation // re-used as strategist
	if agentName == "" {
		r.recordStrategistFailure(p, "spec.experimenter.workstation is empty")
		return ""
	}

	gwPod, err := r.findGatewayPodForAgent(ctx, p.Namespace, agentName)
	if err != nil {
		r.recordStrategistFailure(p, fmt.Sprintf("find gateway pod: %v", err))
		return reason
	}

	prompt := buildStrategistPrompt(p, history, currentHypotheses, reason)

	tctx, cancel := context.WithTimeout(ctx, strategistTimeout)
	defer cancel()
	out, err := r.execInGatewayPod(tctx, gwPod, []string{
		"openclaw", "agent",
		"--agent", agentName,
		"--message", prompt,
		"--json",
		"--timeout", "840", // a touch shy of strategistTimeout
	})
	if err != nil {
		r.recordStrategistFailure(p, fmt.Sprintf("openclaw agent exec: %v (out: %s)", err, truncateMsg(out, 200)))
		return reason
	}

	body, err := extractStrategistReply(out)
	if err != nil {
		r.recordStrategistFailure(p, fmt.Sprintf("parse strategist reply: %v (out: %s)", err, truncateMsg(out, 200)))
		return reason
	}
	if body == "" {
		r.recordStrategistFailure(p, "strategist returned empty body")
		return reason
	}

	// Validate before commit: a malformed HYPOTHESES.md would be
	// silently ignored by the searcher (good), but we'd rather
	// surface it loudly here so the LLM gets a feedback signal
	// (via StrategistOK=False) on its next turn.
	if _, perr := search.ParseHypotheses([]byte(body)); perr != nil {
		r.recordStrategistFailure(p, fmt.Sprintf("strategist YAML invalid: %v", perr))
		return reason
	}

	// Commit HYPOTHESES.md via the same atomic transaction we use
	// for every other wiki write.
	tx, err := r.beginWikiTx(ctx, p)
	if err != nil {
		r.recordStrategistFailure(p, fmt.Sprintf("open wiki: %v", err))
		return reason
	}
	defer tx.Close()

	if err := tx.Write("HYPOTHESES.md", []byte(body)); err != nil {
		r.recordStrategistFailure(p, fmt.Sprintf("stage HYPOTHESES.md: %v", err))
		return reason
	}
	commitMsg := fmt.Sprintf("strategist: HYPOTHESES.md after round %d (%s)",
		len(history), shortReason(reason))
	if err := tx.CommitAndPush(ctx, commitMsg); err != nil {
		r.recordStrategistFailure(p, fmt.Sprintf("push HYPOTHESES.md: %v", err))
		return reason
	}

	lg.Info("strategist turn complete", "round", len(history), "bytes", len(body))
	// We don't set StrategistOK=True here — requestProposal will
	// set it on the NEXT reconcile when it reads the new
	// HYPOTHESES.md and applies it cleanly. That keeps the
	// condition's truth tied to "the searcher actually used
	// this," not "the file landed." If the YAML is somehow
	// still malformed after parse-on-write (shouldn't happen but
	// belt-and-suspenders), requestProposal will flip it back
	// to False with a clearer reason.
	return reason
}

// defaultStrategistEveryN and defaultStrategistOnStagnation are
// the v0.3.0 hardcoded cadence values. v0.4.0 (Phase 3) promotes
// these to spec.strategist.cadence.{everyNRounds, onStagnationRounds}.
const (
	defaultStrategistEveryN       = 5 // run every 5 completed rounds
	defaultStrategistOnStagnation = 3 // run if no improvement in 3 rounds
)

// recordStrategistFailure flips StrategistOK=False with a structured
// message. Caller still proceeds with the round — the searcher
// uses the last good HYPOTHESES.md (or the default space).
func (r *AutoResearchProjectReconciler) recordStrategistFailure(p *agentofficev1alpha1.AutoResearchProject, msg string) {
	log.Log.Info("strategist turn failed; loop continues without new hypotheses", "msg", msg)
	p.Status.Conditions = mergeARPConditions(p.Status.Conditions,
		agentofficev1alpha1.AutoResearchProjectCondition{
			Type:               "StrategistOK",
			Status:             "False",
			Reason:             "TurnFailed",
			Message:            truncateMsg(msg, 256),
			LastTransitionTime: metav1.Now(),
		})
}

// shortReason trims a multi-clause reason string down to a single
// keyword for use in commit messages.
func shortReason(reason string) string {
	if strings.HasPrefix(reason, "stagnation") {
		return "stagnation"
	}
	if strings.HasPrefix(reason, "cadence") {
		return "cadence"
	}
	return "scheduled"
}

// buildStrategistPrompt assembles the message the strategist agent
// sees. Strategy: embed all wiki context the agent needs IN THE
// PROMPT so its tool calls (file_read) aren't on the critical
// path. The agent's job is to reason about the trajectory and
// produce a HYPOTHESES.md body.
//
// The system prompt (set on the AgentWorkstation CR by the
// karpathy template — see Phase 2c) tells the agent it can
// optionally use wiki-clip / wiki-write / wiki-search skills to
// enrich the KB. This user prompt focuses on the deliverable.
func buildStrategistPrompt(
	p *agentofficev1alpha1.AutoResearchProject,
	history []search.Observation,
	current search.Hypotheses,
	reason string,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the strategist for AutoResearchProject %q (round %d).\n\n",
		p.Name, len(history))
	fmt.Fprintf(&b, "Trigger: %s\n\n", reason)

	// Best so far + trajectory summary.
	best := bestObservation(history)
	if best != nil {
		fmt.Fprintf(&b, "Best eval_loss so far: %g\n", best.EvalLoss)
		fmt.Fprintf(&b, "Best config (subset):\n")
		fmt.Fprintf(&b, "  lora_rank=%v, lora_alpha=%v, learning_rate=%v, num_training_steps=%v\n",
			best.Params["lora_rank"], best.Params["lora_alpha"],
			best.Params["learning_rate"], best.Params["num_training_steps"])
		fmt.Fprintln(&b)
	}

	stag := search.StagnationCount(history)
	if stag > 0 {
		fmt.Fprintf(&b, "Stagnation: best eval_loss has NOT improved in the last %d rounds.\n\n", stag)
	}

	// Compact trajectory: the last 10 rounds in tabular form.
	fmt.Fprintln(&b, "Recent trajectory (last 10 completed rounds, oldest first):")
	start := len(history) - 10
	if start < 0 {
		start = 0
	}
	for i := start; i < len(history); i++ {
		fmt.Fprintf(&b, "  round=%d eval_loss=%g  lora_rank=%v lr=%v\n",
			i+1, history[i].EvalLoss,
			history[i].Params["lora_rank"],
			history[i].Params["learning_rate"])
	}
	fmt.Fprintln(&b)

	if current.Hypothesis != "" || len(current.AdjustSearchSpace) > 0 {
		fmt.Fprintf(&b, "Current HYPOTHESES.md (generated after round %d):\n%s\n\n",
			current.GeneratedAfterRound, current.Hypothesis)
	}

	// Deliverable spec.
	fmt.Fprintln(&b, "Your task: write a new HYPOTHESES.md.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Format: Markdown body with a single ```yaml fenced block. The YAML must follow this shape (every field optional):")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b, "generated_at: <ISO 8601 UTC>")
	fmt.Fprintf(&b, "generated_after_round: %d\n", len(history))
	fmt.Fprintln(&b, "hypothesis: |")
	fmt.Fprintln(&b, "  <1-3 sentences explaining your reasoning>")
	fmt.Fprintln(&b, "adjust_search_space:")
	fmt.Fprintln(&b, "  <param_name>:")
	fmt.Fprintln(&b, "    low: <number>     # narrows the lower bound (cannot widen)")
	fmt.Fprintln(&b, "    high: <number>    # narrows the upper bound (cannot widen)")
	fmt.Fprintln(&b, "    log: true|false   # for float params, sample log-uniformly")
	fmt.Fprintln(&b, "    choices: [...]    # for categorical params, REPLACES the choice list")
	fmt.Fprintln(&b, "references:")
	fmt.Fprintln(&b, "  - <path/to/wiki/file>.md")
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Constraints:")
	fmt.Fprintln(&b, "- The YAML fence MUST be present. The operator's parser extracts the first ```yaml block.")
	fmt.Fprintln(&b, "- Bound adjustments can only NARROW the original search space (the operator clamps wideningattempts).")
	fmt.Fprintln(&b, "- If you use the wiki-clip or wiki-write skill to enrich the knowledge base, reference those files in the `references:` list.")
	fmt.Fprintln(&b, "- If you have no new insight, output a minimal HYPOTHESES.md with the same adjust_search_space as before — DO NOT skip the file; the parser handles empty AdjustSearchSpace correctly.")
	fmt.Fprintln(&b, "- Reply with the file body only. No prose before the heading, no commentary after the closing fence.")
	return b.String()
}

// bestObservation returns the observation with the lowest eval_loss
// (assumes minimize). Returns nil for empty history.
func bestObservation(history []search.Observation) *search.Observation {
	if len(history) == 0 {
		return nil
	}
	best := &history[0]
	for i := range history {
		if history[i].EvalLoss < best.EvalLoss {
			best = &history[i]
		}
	}
	return best
}

// extractStrategistReply pulls the strategist's deliverable out of
// the openclaw `agent --json` envelope. Same ACP-payload shape as
// proposeViaAgent v0.0.66+: result.payloads[0].text holds the
// agent's reply body, which IS the HYPOTHESES.md content.
//
// Returns (body, nil) on success. Returns an error if the envelope
// is unparseable; the empty-body case is detected by the caller
// (empty != error).
func extractStrategistReply(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	// Reuse parseAgentProposal's envelope-detection logic by way
	// of inlining the relevant bit — we don't want to call
	// parseAgentProposal here because it expects QLoRAConfig
	// content. Keep it simple: try ACP envelope first, fall back
	// to "raw IS the body" if envelope doesn't match.
	body := raw
	type acpPayload struct {
		Text string `json:"text"`
		Type string `json:"type"`
	}
	type acpEnvelope struct {
		Result struct {
			Payloads []acpPayload `json:"payloads"`
		} `json:"result"`
		Status string `json:"status"`
	}
	var env acpEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err == nil && len(env.Result.Payloads) > 0 {
		// Concatenate all text payloads — the strategist might
		// emit multi-part replies (e.g. one part for the
		// hypothesis and one for clip mentions). The parser
		// only cares about the YAML fence, which we'll find
		// regardless.
		var concat strings.Builder
		for _, pl := range env.Result.Payloads {
			if pl.Text != "" {
				if concat.Len() > 0 {
					concat.WriteString("\n\n")
				}
				concat.WriteString(pl.Text)
			}
		}
		if concat.Len() > 0 {
			body = concat.String()
		}
	}
	return strings.TrimSpace(body), nil
}
