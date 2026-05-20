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

func TestParseHypotheses_EmptyBody(t *testing.T) {
	h, err := ParseHypotheses(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if h.Hypothesis != "" || h.AdjustSearchSpace != nil {
		t.Fatalf("expected zero value, got %+v", h)
	}
}

func TestParseHypotheses_FencedYAML(t *testing.T) {
	body := `# Hypotheses

Trying to be informative for the human reader before the YAML.

` + "```yaml" + `
generated_at: 2026-05-21T14:00:00Z
generated_after_round: 18
hypothesis: |
  Plateau at 0.52 across rounds 12-18.
adjust_search_space:
  learning_rate:
    low: 5.0e-5
    high: 2.0e-4
    log: true
  lora_dropout:
    low: 0.05
    high: 0.20
references:
  - topics/lora-plus-2024.md
` + "```" + `

Trailing notes for the human reader.
`
	h, err := ParseHypotheses([]byte(body))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if h.GeneratedAfterRound != 18 {
		t.Errorf("GeneratedAfterRound: got %d", h.GeneratedAfterRound)
	}
	if !strings.Contains(h.Hypothesis, "Plateau") {
		t.Errorf("Hypothesis: got %q", h.Hypothesis)
	}
	if got := h.AdjustSearchSpace["learning_rate"]; got.Low == nil || *got.Low != 5e-5 {
		t.Errorf("lr.Low: got %v", got.Low)
	}
	if got := h.AdjustSearchSpace["learning_rate"]; got.Log == nil || *got.Log != true {
		t.Errorf("lr.Log: got %v", got.Log)
	}
	if len(h.References) != 1 || h.References[0] != "topics/lora-plus-2024.md" {
		t.Errorf("References: got %v", h.References)
	}
}

func TestParseHypotheses_PlainYAML_NoFence(t *testing.T) {
	body := `generated_after_round: 5
adjust_search_space:
  lora_rank:
    low: 8
    high: 32
`
	h, err := ParseHypotheses([]byte(body))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if h.GeneratedAfterRound != 5 {
		t.Errorf("GeneratedAfterRound: got %d", h.GeneratedAfterRound)
	}
	adj, ok := h.AdjustSearchSpace["lora_rank"]
	if !ok {
		t.Fatal("lora_rank missing")
	}
	if adj.Low == nil || int(*adj.Low) != 8 {
		t.Errorf("Low: %v", adj.Low)
	}
}

func TestParseHypotheses_MalformedYAMLReturnsError(t *testing.T) {
	body := "```yaml\nadjust_search_space:\n  - not a map\n```\n"
	_, err := ParseHypotheses([]byte(body))
	if err == nil {
		t.Fatal("expected error on malformed YAML")
	}
}

func TestHypotheses_Apply_NarrowFloatBounds(t *testing.T) {
	orig := Space{
		Floats: map[string]FloatSpec{
			"learning_rate": {Low: 5e-5, High: 5e-4, Log: true},
		},
	}
	low, high := 1e-4, 3e-4
	h := Hypotheses{
		AdjustSearchSpace: map[string]Adjustment{
			"learning_rate": {Low: &low, High: &high},
		},
	}
	adj, notices := h.Apply(orig)
	if len(notices) != 0 {
		t.Fatalf("unexpected notices: %v", notices)
	}
	got := adj.Floats["learning_rate"]
	if got.Low != 1e-4 || got.High != 3e-4 {
		t.Errorf("bounds: got [%g, %g]", got.Low, got.High)
	}
	if !got.Log {
		t.Error("Log was lost — should be preserved when adj.Log is nil")
	}
	// Original untouched.
	if orig.Floats["learning_rate"].Low != 5e-5 {
		t.Error("Apply mutated the input Space")
	}
}

func TestHypotheses_Apply_ClampsWideningToOrigBounds(t *testing.T) {
	orig := Space{
		Floats: map[string]FloatSpec{
			"lora_dropout": {Low: 0.0, High: 0.2},
		},
	}
	low, high := -0.5, 0.99
	h := Hypotheses{
		AdjustSearchSpace: map[string]Adjustment{
			"lora_dropout": {Low: &low, High: &high},
		},
	}
	adj, notices := h.Apply(orig)
	got := adj.Floats["lora_dropout"]
	if got.Low != 0.0 || got.High != 0.2 {
		t.Errorf("expected clamp to [0.0, 0.2], got [%g, %g]", got.Low, got.High)
	}
	if len(notices) != 2 {
		t.Errorf("expected 2 clamp notices, got %d: %v", len(notices), notices)
	}
}

func TestHypotheses_Apply_CategoricalReplaces(t *testing.T) {
	orig := Space{
		Categoricals: map[string]CategoricalSpec{
			"target_modules": {Choices: []string{"q,v", "q,v,k,o"}},
		},
	}
	h := Hypotheses{
		AdjustSearchSpace: map[string]Adjustment{
			"target_modules": {Choices: []string{"q,v,k,o,gate", "q,v,k,o,gate,up,down"}},
		},
	}
	adj, notices := h.Apply(orig)
	if len(notices) != 0 {
		t.Fatalf("unexpected notices: %v", notices)
	}
	got := adj.Categoricals["target_modules"]
	if len(got.Choices) != 2 || got.Choices[0] != "q,v,k,o,gate" {
		t.Errorf("Choices not replaced: %v", got.Choices)
	}
	// Original untouched.
	if orig.Categoricals["target_modules"].Choices[0] != "q,v" {
		t.Error("Apply mutated the input Space")
	}
}

func TestHypotheses_Apply_UnknownParamIgnoredWithNotice(t *testing.T) {
	orig := Space{
		Floats: map[string]FloatSpec{
			"learning_rate": {Low: 5e-5, High: 5e-4, Log: true},
		},
	}
	low := 0.1
	h := Hypotheses{
		AdjustSearchSpace: map[string]Adjustment{
			"made_up_param": {Low: &low},
		},
	}
	adj, notices := h.Apply(orig)
	// Original space unaffected.
	if got := adj.Floats["learning_rate"]; got.Low != 5e-5 {
		t.Errorf("Floats touched: %+v", got)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "made_up_param") {
		t.Errorf("expected notice mentioning made_up_param, got %v", notices)
	}
}

func TestHypotheses_Apply_RevertsOnInvertedBounds(t *testing.T) {
	orig := Space{
		Ints: map[string]IntSpec{
			"lora_rank": {Low: 4, High: 64, Step: 2},
		},
	}
	low, high := 32.0, 8.0 // inverted
	h := Hypotheses{
		AdjustSearchSpace: map[string]Adjustment{
			"lora_rank": {Low: &low, High: &high},
		},
	}
	adj, notices := h.Apply(orig)
	got := adj.Ints["lora_rank"]
	if got.Low != 4 || got.High != 64 {
		t.Errorf("expected revert to original (4, 64), got (%d, %d)", got.Low, got.High)
	}
	if len(notices) == 0 {
		t.Error("expected a notice about the inverted bounds")
	}
}

func TestHypotheses_Apply_FeedsSearcher(t *testing.T) {
	// End-to-end: apply HYPOTHESES.md to the default QLoRA space,
	// build a searcher over the adjusted space, get a suggestion.
	// The suggestion must respect the adjusted (narrower) bounds.
	body := `
adjust_search_space:
  learning_rate:
    low: 1.0e-4
    high: 1.5e-4
    log: true
`
	h, err := ParseHypotheses([]byte(body))
	if err != nil {
		t.Fatalf("ParseHypotheses: %v", err)
	}
	orig := qloraSpace()
	adj, _ := h.Apply(orig)
	s, err := NewSearcher(Config{Space: adj, Seed: 42, Warmup: 5})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	trial, err := s.Suggest(nil)
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	lr, _ := trial.Params["learning_rate"].(float64)
	if lr < 1e-4-1e-9 || lr > 1.5e-4+1e-9 {
		t.Errorf("learning_rate %g outside narrowed bounds [1e-4, 1.5e-4]", lr)
	}
}

func TestExtractYAMLBlock_PicksFirstFence(t *testing.T) {
	body := "intro text\n```yaml\nfoo: 1\n```\nmore text\n```yaml\nbar: 2\n```\n"
	got := extractYAMLBlock(body)
	if got != "foo: 1" {
		t.Errorf("got %q want %q", got, "foo: 1")
	}
}

func TestExtractYAMLBlock_HandlesYmlTag(t *testing.T) {
	body := "```yml\nfoo: 1\n```\n"
	got := extractYAMLBlock(body)
	if got != "foo: 1" {
		t.Errorf("got %q want %q", got, "foo: 1")
	}
}

func TestExtractYAMLBlock_HandlesBareFence(t *testing.T) {
	body := "```\nfoo: 1\n```\n"
	got := extractYAMLBlock(body)
	if got != "foo: 1" {
		t.Errorf("got %q want %q", got, "foo: 1")
	}
}
