/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package search

import (
	"fmt"
	"strings"
	"testing"
)

// fakeLister implements FileLister against an in-memory map of
// "<dir>/<filename>" → file body. Lets us write history tests
// without spinning up a real git repo.
type fakeLister struct {
	files map[string]string
}

func (f *fakeLister) List(dir string) ([]string, error) {
	out := []string{}
	prefix := dir + "/"
	for k := range f.files {
		if strings.HasPrefix(k, prefix) {
			out = append(out, strings.TrimPrefix(k, prefix))
		}
	}
	return out, nil
}

func (f *fakeLister) Read(relPath string) ([]byte, error) {
	if b, ok := f.files[relPath]; ok {
		return []byte(b), nil
	}
	return nil, nil
}

func TestLoadWikiHistory_EmptyWiki(t *testing.T) {
	fl := &fakeLister{files: map[string]string{}}
	obs, err := LoadWikiHistory(fl, qloraSpace())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if obs != nil {
		t.Fatalf("expected nil, got %d obs", len(obs))
	}
}

func TestLoadWikiHistory_PartialRound_Skipped(t *testing.T) {
	// proposal exists, result does not → round is in-flight,
	// skip silently.
	fl := &fakeLister{files: map[string]string{
		"proposals/round-3.yaml": "lora_rank: 16\n",
		"proposals/round-3.md":   "# round 3\n",
	}}
	obs, err := LoadWikiHistory(fl, qloraSpace())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(obs) != 0 {
		t.Fatalf("expected 0 obs (proposal-only), got %d", len(obs))
	}
}

func TestLoadWikiHistory_OneCompleteRound(t *testing.T) {
	fl := &fakeLister{files: map[string]string{
		"proposals/round-7.yaml": yamlForRound(7, 16, 32, 0.05, 2e-4, "cpu"),
		"results/round-7.md":     resultMDForRound(7, 0.521735),
	}}
	obs, err := LoadWikiHistory(fl, qloraSpace())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 obs, got %d", len(obs))
	}
	if obs[0].EvalLoss != 0.521735 {
		t.Errorf("EvalLoss: got %v", obs[0].EvalLoss)
	}
	if obs[0].Params["lora_rank"] != 16 {
		t.Errorf("lora_rank: got %v", obs[0].Params["lora_rank"])
	}
	if obs[0].Params["learning_rate"] != 2e-4 {
		t.Errorf("learning_rate: got %v", obs[0].Params["learning_rate"])
	}
	if obs[0].Params["offload_strategy"] != "cpu" {
		t.Errorf("offload_strategy: got %v", obs[0].Params["offload_strategy"])
	}
}

func TestLoadWikiHistory_RoundsInOrder(t *testing.T) {
	// Rounds 1, 5, 3 in storage map iteration order — output must
	// be 1, 3, 5.
	fl := &fakeLister{files: map[string]string{
		"proposals/round-5.yaml": yamlForRound(5, 32, 64, 0.1, 3e-4, "disk"),
		"results/round-5.md":     resultMDForRound(5, 0.6),
		"proposals/round-1.yaml": yamlForRound(1, 8, 16, 0.05, 2e-4, "cpu"),
		"results/round-1.md":     resultMDForRound(1, 1.2),
		"proposals/round-3.yaml": yamlForRound(3, 16, 32, 0.08, 1.5e-4, "cpu"),
		"results/round-3.md":     resultMDForRound(3, 0.7),
	}}
	obs, err := LoadWikiHistory(fl, qloraSpace())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(obs) != 3 {
		t.Fatalf("expected 3 obs, got %d", len(obs))
	}
	// Verify round order via eval_loss values (1.2, 0.7, 0.6).
	wantLoss := []float64{1.2, 0.7, 0.6}
	for i, w := range wantLoss {
		if obs[i].EvalLoss != w {
			t.Errorf("obs[%d].EvalLoss: got %v want %v", i, obs[i].EvalLoss, w)
		}
	}
}

func TestLoadWikiHistory_UnparseableEvalLoss_Skipped(t *testing.T) {
	fl := &fakeLister{files: map[string]string{
		"proposals/round-7.yaml": yamlForRound(7, 16, 32, 0.05, 2e-4, "cpu"),
		"results/round-7.md":     "# round 7 — result\n\n(missing eval_loss row)\n",
	}}
	obs, err := LoadWikiHistory(fl, qloraSpace())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(obs) != 0 {
		t.Fatalf("expected 0 obs (no eval_loss), got %d", len(obs))
	}
}

func TestLoadWikiHistory_IgnoresUnknownFields(t *testing.T) {
	// Add a `notes: "..."` field and a `target_modules: [...]`
	// list. Neither is in the search space — both should be
	// silently ignored, and the rest of the params should parse
	// normally.
	yaml := `lora_rank: 16
lora_alpha: 32
lora_dropout: 0.05
learning_rate: 0.0002
offload_strategy: cpu
notes: "round 7: best so far"
target_modules: [q_proj, v_proj]
`
	fl := &fakeLister{files: map[string]string{
		"proposals/round-7.yaml": yaml,
		"results/round-7.md":     resultMDForRound(7, 0.5),
	}}
	obs, err := LoadWikiHistory(fl, qloraSpace())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 obs, got %d", len(obs))
	}
	if obs[0].Params["lora_rank"] != 16 {
		t.Errorf("lora_rank: got %v", obs[0].Params["lora_rank"])
	}
}

func TestLoadWikiHistory_FeedsSearcher(t *testing.T) {
	// End-to-end: load history from a fake wiki + plug it into
	// the searcher + ask for the next config. Should succeed
	// without errors (we don't assert on the specific suggestion
	// here — TestSuggest_HistoryGuides_TPE already covers
	// quality).
	fl := &fakeLister{files: map[string]string{}}
	for n := 1; n <= 6; n++ {
		fl.files[fmt.Sprintf("proposals/round-%d.yaml", n)] = yamlForRound(n, 16, 32, 0.05, 2e-4, "cpu")
		fl.files[fmt.Sprintf("results/round-%d.md", n)] = resultMDForRound(n, 0.5+float64(n)*0.05)
	}
	history, err := LoadWikiHistory(fl, qloraSpace())
	if err != nil {
		t.Fatalf("LoadWikiHistory: %v", err)
	}
	if len(history) != 6 {
		t.Fatalf("expected 6 obs, got %d", len(history))
	}
	s, err := NewSearcher(Config{Space: qloraSpace(), Seed: 42, Warmup: 5})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	trial, err := s.Suggest(history)
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if len(trial.Params) == 0 {
		t.Fatal("Suggest returned empty params")
	}
}

// ─── helpers ────────────────────────────────────────────────────────

func yamlForRound(round, rank, alpha int, dropout, lr float64, offload string) string {
	return fmt.Sprintf(`round: %d
lora_rank: %d
lora_alpha: %d
lora_dropout: %g
target_modules: [q_proj, v_proj]
learning_rate: %g
num_training_steps: 200
per_device_batch_size: 4
gradient_accumulation_steps: 4
max_seq_length: 1024
warmup_steps: 20
weight_decay: 0.01
offload_strategy: %s
notes: "round %d"
`, round, rank, alpha, dropout, lr, offload, round)
}

func resultMDForRound(round int, evalLoss float64) string {
	return fmt.Sprintf(`# Round %d — result

| Field | Value |
|-------|-------|
| eval_loss | %g |
| Decision | **kept** |
| Adapter URI | quay.local/adapter:round-%d |
`, round, evalLoss, round)
}
