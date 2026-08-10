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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func recommendFixture() []catalogPack {
	return []catalogPack{
		{Type: "skill", Name: "weekly-ops-report", DisplayName: "Weekly Ops Report",
			Description: "Produce the Monday-morning weekly ops report (orders, revenue, stuck orders, top products) from the governed metrics tools."},
		{Type: "skill", Name: "wiki-write", DisplayName: "Wiki Write",
			Description: "Write and update wiki pages in a knowledge base."},
		{Type: "tool", Name: "github", DisplayName: "GitHub (governed)",
			Description: "Create issues, projects, and repos as a governed identity."},
		{Type: "tool", Name: "ops-metrics", DisplayName: "ops-metrics",
			Description: "Weekly ops answers from orders data: summary, stuck orders, top products."},
		{Type: "kb", Name: "platform-capabilities", DisplayName: "Agent Platform Capabilities",
			Description: "Source of truth for what the platform can do."},
	}
}

// TestRecommendFallback pins the deterministic engine: an ops-report
// description selects the ops skill and tool ahead of unrelated packs,
// the identity is DNS-safe, the role mapping fires, and the system
// prompt carries the never-fabricate line.
func TestRecommendFallback(t *testing.T) {
	out := recommendFallback(
		"I need weekly ops reports summarizing orders and stuck shipments", recommendFixture())
	if out.Source != "fallback" {
		t.Fatalf("source: %v", out.Source)
	}
	names := map[string]bool{}
	for _, p := range out.Packs {
		names[p.Name] = true
	}
	if !names["weekly-ops-report"] || !names["ops-metrics"] {
		t.Errorf("expected ops packs selected, got %+v", out.Packs)
	}
	if names["wiki-write"] {
		t.Errorf("unrelated skill selected: %+v", out.Packs)
	}
	if out.Identity.Role != "reporter" {
		t.Errorf("role mapping: want reporter, got %q", out.Identity.Role)
	}
	if s := sanitizeAgentName(out.Identity.Name); s != out.Identity.Name || s == "" {
		t.Errorf("identity name not DNS-safe: %q", out.Identity.Name)
	}
	if !strings.Contains(out.Identity.SystemPrompt, "Never fabricate") {
		t.Errorf("system prompt missing the honesty line: %q", out.Identity.SystemPrompt)
	}
}

func TestSanitizeAgentName(t *testing.T) {
	for in, want := range map[string]string{
		"Weekly Ops!! Agent":       "weekly-ops-agent",
		"--foo__bar--":             "foo-bar",
		"AVeryLongNameThatGoesOnAndOnForeverAndEver": "averylongnamethatgoesonandonfo",
	} {
		if got := sanitizeAgentName(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRecommendViaModel exercises the model path against a fake
// OpenAI-compatible server: the completion selects one real pack and
// one hallucinated pack; the hallucination must be dropped and the
// real selection kept, with the identity sanitized.
func TestRecommendViaModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sel := map[string]any{
			"name": "Ops Reporter!", "displayName": "Ops Reporter", "emoji": "📊",
			"role": "reporter",
			"systemPrompt": "You produce the weekly ops report. Never fabricate data.",
			"packs": []map[string]string{
				{"name": "weekly-ops-report", "reason": "directly matches"},
				{"name": "made-up-pack", "reason": "hallucinated"},
			},
		}
		selB, _ := json.Marshal(sel)
		resp := map[string]any{"choices": []map[string]any{
			{"message": map[string]string{"content": string(selB)}},
		}}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	out, err := recommendViaModel(context.Background(), srv.URL,
		"weekly ops reports", recommendFixture())
	if err != nil {
		t.Fatalf("model path: %v", err)
	}
	if out.Source != "model" {
		t.Errorf("source: %v", out.Source)
	}
	if len(out.Packs) != 1 || out.Packs[0].Name != "weekly-ops-report" {
		t.Errorf("hallucination not dropped / real pack lost: %+v", out.Packs)
	}
	if out.Identity.Name != "ops-reporter" {
		t.Errorf("identity not sanitized: %q", out.Identity.Name)
	}
}

// TestRecommendViaModel_AllHallucinated: a selection with zero valid
// packs is a model failure — the caller falls back.
func TestRecommendViaModel_AllHallucinated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"name\":\"x\",\"displayName\":\"X\",\"role\":\"assistant\",\"systemPrompt\":\"y\",\"packs\":[{\"name\":\"nope\"}]}"}}]}`)
	}))
	defer srv.Close()
	if _, err := recommendViaModel(context.Background(), srv.URL, "anything", recommendFixture()); err == nil {
		t.Fatal("want error when every selected pack is invalid")
	}
}
