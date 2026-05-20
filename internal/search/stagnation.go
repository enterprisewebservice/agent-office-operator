/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package search

import (
	"fmt"
	"math"
)

// StagnationCount returns the number of recent observations during
// which the best eval_loss has NOT improved. Returns 0 if the most
// recent observation is the best so far, len(history) if history
// is empty (no improvement is possible without observations).
//
// "Improvement" assumes minimization (lower eval_loss is better).
// Callers using a maximize objective should negate values before
// passing in, or roll their own equivalent.
//
// Example: history = [1.0, 0.8, 0.7, 0.9, 0.85] → best is 0.7 at
// index 2; stagnation count is len(5) - 1 - 2 = 2.
func StagnationCount(history []Observation) int {
	if len(history) == 0 {
		return 0
	}
	best := math.Inf(1)
	lastImprovedAt := -1
	for i, obs := range history {
		if obs.EvalLoss < best {
			best = obs.EvalLoss
			lastImprovedAt = i
		}
	}
	if lastImprovedAt < 0 {
		// Every observation was NaN or +Inf — treat as fully
		// stagnant.
		return len(history)
	}
	return len(history) - 1 - lastImprovedAt
}

// StrategistDueReason returns a non-empty human-readable reason if
// the strategist should run this reconcile, or "" if not due.
//
// Inputs:
//   - history: completed observations (operator-trusted)
//   - h: the current HYPOTHESES.md (zero-value if not yet present)
//   - everyN: run the strategist every N completed rounds
//     (0 disables cadence-based runs)
//   - stagN: run the strategist when stagnation hits this threshold
//     (0 disables stagnation-based runs)
//
// "Last ran" is read from h.GeneratedAfterRound — the strategist
// writes that field when it commits HYPOTHESES.md, so we don't
// need any operator-side state to track cadence.
//
// First-ever run: if history has at least `everyN` observations and
// HYPOTHESES.md is empty, the function returns the cadence reason
// so the strategist runs on its first cycle.
func StrategistDueReason(history []Observation, h Hypotheses, everyN, stagN int) string {
	currentRound := len(history)
	lastRan := h.GeneratedAfterRound

	if stagN > 0 {
		stag := StagnationCount(history)
		// Also gate: only fire stagnation if we haven't ALREADY
		// run the strategist since the stagnation started.
		// Otherwise we'd run on every reconcile while stagnating.
		// "Strategist ran since stagnation started" =
		// currentRound - lastRan < stag.
		stagPersists := stag >= stagN && (currentRound-lastRan) >= 1
		if stagPersists {
			return fmt.Sprintf("stagnation (no improvement in %d rounds; last strategist run after round %d)", stag, lastRan)
		}
	}
	if everyN > 0 && currentRound-lastRan >= everyN {
		return fmt.Sprintf("cadence (every %d rounds; last ran after round %d, currently at %d completed observations)",
			everyN, lastRan, currentRound)
	}
	return ""
}
