/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
	"github.com/enterprisewebservice/agent-office-operator/internal/search"
)

// Dashboard rendering is pure (apart from time.Now in the header).
// We exercise it as a string-producing function — no envtest, no
// wiki tx (pass nil and assert the strategist-research section is
// gracefully empty).

func newARP(name string) *agentofficev1alpha1.AutoResearchProject {
	return &agentofficev1alpha1.AutoResearchProject{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-office"},
	}
}

func TestRenderDashboard_EmptyHistory(t *testing.T) {
	p := newARP("test-proj")
	md := renderDashboard(p, 1, QLoRAConfig{Round: 1, LoraRank: 8}, nil, search.Hypotheses{}, defaultQLoRASpace(), nil)
	checkContains(t, md, "# Dashboard — `test-proj`")
	checkContains(t, md, "## Project state")
	checkContains(t, md, "no completed rounds yet")
	checkContains(t, md, "Random (warmup")
	checkContains(t, md, "strategist has not run yet")
}

func TestRenderDashboard_WithHistory(t *testing.T) {
	p := newARP("test-proj")
	history := []search.Observation{
		{Params: map[string]any{"lora_rank": 8, "learning_rate": 2e-4, "num_training_steps": 200}, EvalLoss: 1.2},
		{Params: map[string]any{"lora_rank": 16, "learning_rate": 3e-4, "num_training_steps": 250}, EvalLoss: 0.95},
		{Params: map[string]any{"lora_rank": 32, "learning_rate": 1.5e-4, "num_training_steps": 300}, EvalLoss: 0.55},
		{Params: map[string]any{"lora_rank": 16, "learning_rate": 1.4e-4, "num_training_steps": 280}, EvalLoss: 0.52},
	}
	md := renderDashboard(p, 5, QLoRAConfig{Round: 5}, history, search.Hypotheses{}, defaultQLoRASpace(), nil)
	checkContains(t, md, "**0.52**") // best eval_loss
	checkContains(t, md, "Trajectory")
	checkContains(t, md, "| 4 | 0.52") // newest row
	checkContains(t, md, "| 1 | 1.2")  // oldest row in window
	checkContains(t, md, "Top")
	checkContains(t, md, "Random (warmup — 4/5 trials)")
}

func TestRenderDashboard_TPEKicksInPastWarmup(t *testing.T) {
	p := newARP("test-proj")
	history := make([]search.Observation, 6)
	for i := range history {
		history[i] = search.Observation{
			Params:   map[string]any{"lora_rank": 8 + i*2, "learning_rate": 2e-4, "num_training_steps": 200},
			EvalLoss: 1.0 - float64(i)*0.05,
		}
	}
	md := renderDashboard(p, 7, QLoRAConfig{Round: 7}, history, search.Hypotheses{}, defaultQLoRASpace(), nil)
	checkContains(t, md, "TPE (goptuna)")
	if strings.Contains(md, "Random (warmup") {
		t.Error("expected TPE mode at 6 observations, found 'Random (warmup' in dashboard")
	}
}

func TestRenderDashboard_StagnationFlagged(t *testing.T) {
	p := newARP("test-proj")
	// Best at index 0; 5 stagnant rounds after.
	history := []search.Observation{
		{Params: map[string]any{"lora_rank": 16}, EvalLoss: 0.5},
		{Params: map[string]any{"lora_rank": 16}, EvalLoss: 0.55},
		{Params: map[string]any{"lora_rank": 16}, EvalLoss: 0.60},
		{Params: map[string]any{"lora_rank": 16}, EvalLoss: 0.58},
		{Params: map[string]any{"lora_rank": 16}, EvalLoss: 0.57},
		{Params: map[string]any{"lora_rank": 16}, EvalLoss: 0.56},
	}
	md := renderDashboard(p, 7, QLoRAConfig{}, history, search.Hypotheses{}, defaultQLoRASpace(), nil)
	checkContains(t, md, "**STAGNATING**")
}

func TestRenderDashboard_HypothesesRendered(t *testing.T) {
	p := newARP("test-proj")
	low, high := 1e-4, 2e-4
	h := search.Hypotheses{
		GeneratedAfterRound: 8,
		Hypothesis:          "Plateau detected at 0.55; narrow LR to exploit best region.",
		AdjustSearchSpace: map[string]search.Adjustment{
			"learning_rate": {Low: &low, High: &high},
		},
		References: []string{"topics/lr-tuning.md", "raw/clips/2024-lr-paper.md"},
	}
	adjusted, _ := h.Apply(defaultQLoRASpace())
	md := renderDashboard(p, 9, QLoRAConfig{}, nil, h, adjusted, nil)
	checkContains(t, md, "Plateau detected at 0.55")
	checkContains(t, md, "topics/lr-tuning.md")
	checkContains(t, md, "raw/clips/2024-lr-paper.md")
	checkContains(t, md, "after round 8")
	// Search-space table notes that LR was narrowed.
	checkContains(t, md, "narrowed from")
}

func TestRenderDashboard_TopKShowsBestSorted(t *testing.T) {
	p := newARP("test-proj")
	// Out-of-order losses to verify sorting.
	history := []search.Observation{
		{Params: map[string]any{"lora_rank": 8, "learning_rate": 2e-4}, EvalLoss: 0.9},
		{Params: map[string]any{"lora_rank": 32, "learning_rate": 1e-4}, EvalLoss: 0.3},
		{Params: map[string]any{"lora_rank": 16, "learning_rate": 3e-4}, EvalLoss: 0.7},
		{Params: map[string]any{"lora_rank": 8, "learning_rate": 5e-4}, EvalLoss: 0.5},
	}
	md := renderDashboard(p, 5, QLoRAConfig{}, history, search.Hypotheses{}, defaultQLoRASpace(), nil)
	// Find the Top configurations section.
	idx := strings.Index(md, "Top")
	if idx < 0 {
		t.Fatal("Top section missing")
	}
	tail := md[idx:]
	// First data row in the Top table should be the lowest loss (0.3).
	if !strings.Contains(tail, "| 1 | 0.3 ") {
		t.Errorf("top row should be 0.3, got:\n%s", strings.SplitN(tail, "\n\n", 2)[0])
	}
}

func TestRenderDashboard_FooterPresent(t *testing.T) {
	p := newARP("test-proj")
	md := renderDashboard(p, 1, QLoRAConfig{}, nil, search.Hypotheses{}, defaultQLoRASpace(), nil)
	checkContains(t, md, "agent-office-operator")
}

func checkContains(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("dashboard missing %q\n--- full dashboard ---\n%s", sub, s)
	}
}
