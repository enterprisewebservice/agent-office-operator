/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package search

import (
	"strings"
	"testing"
)

func obs(loss float64) Observation {
	return Observation{EvalLoss: loss}
}

func TestStagnationCount_EmptyHistory(t *testing.T) {
	if got := StagnationCount(nil); got != 0 {
		t.Errorf("got %d", got)
	}
}

func TestStagnationCount_BestIsLast(t *testing.T) {
	// Improving every step — stagnation count is 0.
	hist := []Observation{obs(1.0), obs(0.9), obs(0.8), obs(0.7)}
	if got := StagnationCount(hist); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestStagnationCount_PlateauAfterBest(t *testing.T) {
	// Best at index 2, then 4 stagnant rounds after.
	hist := []Observation{obs(1.0), obs(0.8), obs(0.5), obs(0.6), obs(0.55), obs(0.7), obs(0.6)}
	if got := StagnationCount(hist); got != 4 {
		t.Errorf("got %d, want 4", got)
	}
}

func TestStagnationCount_AllSame(t *testing.T) {
	// Best is at index 0; everything else stagnates.
	hist := []Observation{obs(0.5), obs(0.5), obs(0.5)}
	if got := StagnationCount(hist); got != 2 {
		t.Errorf("got %d, want 2", got)
	}
}

func TestStrategistDueReason_NotDue_NoHistory(t *testing.T) {
	if r := StrategistDueReason(nil, Hypotheses{}, 5, 3); r != "" {
		t.Errorf("got %q, want empty (no history → nothing to research)", r)
	}
}

func TestStrategistDueReason_CadenceFires(t *testing.T) {
	// 5 completed rounds, never ran → cadence fires.
	history := []Observation{obs(1), obs(0.9), obs(0.8), obs(0.7), obs(0.6)}
	r := StrategistDueReason(history, Hypotheses{}, 5, 3)
	if !strings.Contains(r, "cadence") {
		t.Errorf("got %q, want cadence reason", r)
	}
}

func TestStrategistDueReason_CadenceNotYet(t *testing.T) {
	// Strategist ran after round 5; we're now at 7 completed
	// rounds. Cadence is every 5 → still 3 away.
	history := []Observation{obs(1), obs(0.9), obs(0.8), obs(0.7), obs(0.6), obs(0.5), obs(0.55)}
	r := StrategistDueReason(history, Hypotheses{GeneratedAfterRound: 5}, 5, 3)
	if r != "" {
		t.Errorf("got %q, want empty (cadence not yet)", r)
	}
}

func TestStrategistDueReason_StagnationOverridesCadence(t *testing.T) {
	// Best at round 1, then 4 stagnant rounds. Stagnation
	// threshold is 3 → should fire even though cadence (5) isn't
	// hit and the strategist already ran after round 1.
	history := []Observation{obs(1.0), obs(0.5), obs(0.6), obs(0.7), obs(0.55), obs(0.6)}
	r := StrategistDueReason(history, Hypotheses{GeneratedAfterRound: 1}, 5, 3)
	if !strings.Contains(r, "stagnation") {
		t.Errorf("got %q, want stagnation reason", r)
	}
}

func TestStrategistDueReason_StagnationGatedByLastRun(t *testing.T) {
	// 4 stagnant rounds, but strategist ran after the LAST of
	// those rounds (currentRound == lastRan). Don't fire again
	// immediately — wait for the next round to come in.
	history := []Observation{obs(1.0), obs(0.5), obs(0.6), obs(0.7), obs(0.55), obs(0.6)}
	r := StrategistDueReason(history, Hypotheses{GeneratedAfterRound: 6}, 0, 3)
	if r != "" {
		t.Errorf("got %q, want empty (already ran post-stagnation)", r)
	}
}

func TestStrategistDueReason_BothDisabledMeansNeverFires(t *testing.T) {
	history := []Observation{obs(1), obs(1), obs(1), obs(1), obs(1), obs(1)}
	if r := StrategistDueReason(history, Hypotheses{}, 0, 0); r != "" {
		t.Errorf("got %q, want empty (both gates disabled)", r)
	}
}
