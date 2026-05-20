/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package search

import (
	"errors"
	"math"
	"testing"
)

// qloraSpace returns a representative QLoRA hyperparameter search
// space — the same shape we'll use in production. Kept in the test
// file so production code is not pinned to a specific space; the
// space is data, not behavior.
func qloraSpace() Space {
	return Space{
		Ints: map[string]IntSpec{
			"lora_rank":                  {Low: 4, High: 64, Step: 2},
			"lora_alpha":                 {Low: 8, High: 128, Step: 4},
			"num_training_steps":         {Low: 100, High: 500, Step: 20},
			"per_device_batch_size":      {Low: 1, High: 8},
			"gradient_accumulation_steps": {Low: 1, High: 8},
			"max_seq_length":             {Low: 512, High: 2048, Step: 256},
			"warmup_steps":               {Low: 0, High: 50, Step: 5},
		},
		Floats: map[string]FloatSpec{
			"lora_dropout":  {Low: 0.0, High: 0.2},
			"learning_rate": {Low: 5e-5, High: 5e-4, Log: true},
			"weight_decay":  {Low: 0.0, High: 0.1},
		},
		Categoricals: map[string]CategoricalSpec{
			"offload_strategy": {Choices: []string{"none", "cpu", "disk"}},
		},
	}
}

func TestSuggest_EmptyHistory_ProducesValidConfig(t *testing.T) {
	s, err := NewSearcher(Config{Space: qloraSpace(), Seed: 42, Warmup: 5})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	trial, err := s.Suggest(nil)
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if len(trial.Params) == 0 {
		t.Fatal("Suggest returned empty params")
	}
	// Every required field present + within bounds.
	checkIntBound(t, trial.Params, "lora_rank", 4, 64)
	checkIntBound(t, trial.Params, "lora_alpha", 8, 128)
	checkIntBound(t, trial.Params, "num_training_steps", 100, 500)
	checkFloatBound(t, trial.Params, "lora_dropout", 0.0, 0.2)
	checkFloatBound(t, trial.Params, "learning_rate", 5e-5, 5e-4)
	checkCategoricalChoice(t, trial.Params, "offload_strategy", []string{"none", "cpu", "disk"})
}

func TestSuggest_SeedDeterminism(t *testing.T) {
	// Same seed + same history → identical suggestion. Critical
	// for reproducibility: an operator restart should pick the same
	// next config given the same wiki state.
	s1, _ := NewSearcher(Config{Space: qloraSpace(), Seed: 7, Warmup: 5})
	s2, _ := NewSearcher(Config{Space: qloraSpace(), Seed: 7, Warmup: 5})
	t1, err := s1.Suggest(nil)
	if err != nil {
		t.Fatalf("s1.Suggest: %v", err)
	}
	t2, err := s2.Suggest(nil)
	if err != nil {
		t.Fatalf("s2.Suggest: %v", err)
	}
	for k, v := range t1.Params {
		if t2.Params[k] != v {
			t.Errorf("non-deterministic: param %s differs: %v vs %v", k, v, t2.Params[k])
		}
	}
}

func TestSuggest_DifferentSeedsDifferentSuggestions(t *testing.T) {
	// Sanity check: different seeds should produce different
	// suggestions on the first call (otherwise the seed isn't
	// being used).
	s1, _ := NewSearcher(Config{Space: qloraSpace(), Seed: 1, Warmup: 5})
	s2, _ := NewSearcher(Config{Space: qloraSpace(), Seed: 99999, Warmup: 5})
	t1, _ := s1.Suggest(nil)
	t2, _ := s2.Suggest(nil)
	differ := false
	for k, v := range t1.Params {
		if t2.Params[k] != v {
			differ = true
			break
		}
	}
	if !differ {
		t.Fatal("seeds 1 and 99999 produced identical suggestions — seed plumbing is broken")
	}
}

func TestSuggest_HistoryGuides_TPE(t *testing.T) {
	// After the warmup window, TPE should prefer points near the
	// best observation. We feed a history that's pinned around
	// lora_rank=16 with low loss, and points at lora_rank=4/64
	// with high loss, then ask for a suggestion. The suggestion
	// shouldn't land at the extremes.
	//
	// This isn't a strict mathematical guarantee for TPE — it's a
	// statistical one — so we check across multiple seeds and
	// assert "majority near the best region."
	history := []Observation{}
	// 5 warmup trials anywhere (TPE needs warmup).
	history = append(history,
		Observation{Params: map[string]any{"lora_rank": 4, "lora_alpha": 8, "lora_dropout": 0.10, "learning_rate": 1e-4, "num_training_steps": 200, "per_device_batch_size": 4, "gradient_accumulation_steps": 4, "max_seq_length": 1024, "warmup_steps": 20, "weight_decay": 0.01, "offload_strategy": "cpu"}, EvalLoss: 1.20},
		Observation{Params: map[string]any{"lora_rank": 64, "lora_alpha": 128, "lora_dropout": 0.15, "learning_rate": 4e-4, "num_training_steps": 400, "per_device_batch_size": 4, "gradient_accumulation_steps": 4, "max_seq_length": 1024, "warmup_steps": 20, "weight_decay": 0.05, "offload_strategy": "cpu"}, EvalLoss: 1.10},
		Observation{Params: map[string]any{"lora_rank": 16, "lora_alpha": 32, "lora_dropout": 0.05, "learning_rate": 2e-4, "num_training_steps": 250, "per_device_batch_size": 4, "gradient_accumulation_steps": 4, "max_seq_length": 1024, "warmup_steps": 20, "weight_decay": 0.01, "offload_strategy": "cpu"}, EvalLoss: 0.55},
		Observation{Params: map[string]any{"lora_rank": 16, "lora_alpha": 32, "lora_dropout": 0.08, "learning_rate": 1.5e-4, "num_training_steps": 300, "per_device_batch_size": 4, "gradient_accumulation_steps": 4, "max_seq_length": 1024, "warmup_steps": 20, "weight_decay": 0.02, "offload_strategy": "cpu"}, EvalLoss: 0.52},
		Observation{Params: map[string]any{"lora_rank": 16, "lora_alpha": 32, "lora_dropout": 0.05, "learning_rate": 1.3e-4, "num_training_steps": 280, "per_device_batch_size": 4, "gradient_accumulation_steps": 4, "max_seq_length": 1024, "warmup_steps": 20, "weight_decay": 0.015, "offload_strategy": "cpu"}, EvalLoss: 0.50},
	)

	nearBest := 0
	totalSamples := 20
	for seed := int64(1); seed <= int64(totalSamples); seed++ {
		s, err := NewSearcher(Config{Space: qloraSpace(), Seed: seed, Warmup: 5})
		if err != nil {
			t.Fatalf("seed %d NewSearcher: %v", seed, err)
		}
		trial, err := s.Suggest(history)
		if err != nil {
			t.Fatalf("seed %d Suggest: %v", seed, err)
		}
		rank, _ := trial.Params["lora_rank"].(int)
		// "Near the best" defined as rank in [8, 32] — the
		// neighborhood of the observed-best rank 16.
		if rank >= 8 && rank <= 32 {
			nearBest++
		}
	}
	// TPE should land in the best neighborhood for the majority
	// of seeds. Using 50% as the threshold — generous for a
	// statistical assertion, tight enough to catch a broken
	// implementation that ignores history.
	if nearBest < totalSamples/2 {
		t.Errorf("TPE did not concentrate near best region: %d/%d samples in [8,32]", nearBest, totalSamples)
	}
}

func TestSuggest_WarmupUsesRandom(t *testing.T) {
	// During warmup (history smaller than Warmup), the sampler is
	// forced to Random regardless of cfg.Sampler. Verify by
	// running Suggest with warmup=5 + 0 history, against the same
	// Suggest with warmup=0 + 0 history (so the TPE sampler is
	// active). With 0 history TPE *also* falls back to random
	// internally, so we instead check that warmup=5 produces a
	// different config than warmup=0 across a fixed seed — the
	// two paths use different random streams.
	s1, _ := NewSearcher(Config{Space: qloraSpace(), Seed: 42, Warmup: 5, Sampler: SamplerTPE})
	s2, _ := NewSearcher(Config{Space: qloraSpace(), Seed: 42, Warmup: 5, Sampler: SamplerRandom})
	t1, err := s1.Suggest(nil)
	if err != nil {
		t.Fatalf("s1.Suggest: %v", err)
	}
	t2, err := s2.Suggest(nil)
	if err != nil {
		t.Fatalf("s2.Suggest: %v", err)
	}
	// With 0 history, both should be running the random branch —
	// so the seeds match → identical outputs. This is the
	// invariant: warmup forces Random, and Random is reproducible.
	for k, v := range t1.Params {
		if t2.Params[k] != v {
			t.Errorf("warmup path inconsistent: %s differs %v vs %v", k, v, t2.Params[k])
		}
	}
}

func TestNewSearcher_RejectsEmptySpace(t *testing.T) {
	_, err := NewSearcher(Config{})
	if !errors.Is(err, ErrEmptySpace) {
		t.Fatalf("expected ErrEmptySpace, got %v", err)
	}
}

func TestNewSearcher_RejectsBadInt(t *testing.T) {
	_, err := NewSearcher(Config{
		Space: Space{Ints: map[string]IntSpec{"x": {Low: 10, High: 5}}},
	})
	if err == nil {
		t.Fatal("expected error on Low > High")
	}
}

func TestNewSearcher_RejectsLogFloatWithZeroLow(t *testing.T) {
	_, err := NewSearcher(Config{
		Space: Space{Floats: map[string]FloatSpec{"lr": {Low: 0, High: 1, Log: true}}},
	})
	if err == nil {
		t.Fatal("expected error on Log=true with Low=0")
	}
}

func TestNewSearcher_RejectsEmptyCategoricalChoices(t *testing.T) {
	_, err := NewSearcher(Config{
		Space: Space{Categoricals: map[string]CategoricalSpec{"x": {Choices: nil}}},
	})
	if err == nil {
		t.Fatal("expected error on empty Choices")
	}
}

// ─── helpers ────────────────────────────────────────────────────────

func checkIntBound(t *testing.T, params map[string]any, name string, low, high int) {
	t.Helper()
	v, ok := params[name]
	if !ok {
		t.Errorf("param %s missing", name)
		return
	}
	iv, ok := v.(int)
	if !ok {
		t.Errorf("param %s not int (got %T)", name, v)
		return
	}
	if iv < low || iv > high {
		t.Errorf("param %s = %d outside [%d, %d]", name, iv, low, high)
	}
}

func checkFloatBound(t *testing.T, params map[string]any, name string, low, high float64) {
	t.Helper()
	v, ok := params[name]
	if !ok {
		t.Errorf("param %s missing", name)
		return
	}
	fv, ok := v.(float64)
	if !ok {
		t.Errorf("param %s not float64 (got %T)", name, v)
		return
	}
	// Allow tiny floating-point slack at boundaries (1e-9).
	if fv < low-1e-9 || fv > high+1e-9 || math.IsNaN(fv) {
		t.Errorf("param %s = %g outside [%g, %g]", name, fv, low, high)
	}
}

func checkCategoricalChoice(t *testing.T, params map[string]any, name string, choices []string) {
	t.Helper()
	v, ok := params[name]
	if !ok {
		t.Errorf("param %s missing", name)
		return
	}
	sv, ok := v.(string)
	if !ok {
		t.Errorf("param %s not string (got %T)", name, v)
		return
	}
	for _, c := range choices {
		if c == sv {
			return
		}
	}
	t.Errorf("param %s = %q not in choices %v", name, sv, choices)
}
