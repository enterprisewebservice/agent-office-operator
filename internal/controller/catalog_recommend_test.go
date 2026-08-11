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

// TestRecommendCarriesFullPacks is the guard for the defect that shipped
// in v1.7.12: a recommendation whose packs are names only forces the
// client into a second lookup, and a slow or failed lookup silently
// produces an agent with no tools. Every recommended pack must arrive
// wired — tool recipe present, skill dependencies present.
func TestRecommendCarriesFullPacks(t *testing.T) {
	fixture := recommendFixture()
	fixture[3].Recipe = &toolRecipe{URL: "http://gw/mcp", Type: "http"}
	fixture[0].Dependencies = []catalogDependency{
		{Kind: "mcpServer", Name: "ops-metrics", Available: true, GatewayURL: "http://gw/mcp"},
	}
	out := recommendFallback("weekly ops report on orders and stuck shipments", fixture)

	var sawSkillDeps, sawToolRecipe bool
	for _, p := range out.Packs {
		if p.Type == "skill" && p.Name == "weekly-ops-report" {
			if len(p.Dependencies) == 0 {
				t.Errorf("skill pack arrived without dependencies: %+v", p)
			}
			sawSkillDeps = true
		}
		if p.Type == "tool" && p.Name == "ops-metrics" {
			if p.Recipe == nil || p.Recipe.URL == "" {
				t.Errorf("tool pack arrived without a recipe: %+v", p)
			}
			sawToolRecipe = true
		}
		if p.Reason == "" {
			t.Errorf("pack %s has no reason — the UI shows it to the user", p.Name)
		}
	}
	if !sawSkillDeps || !sawToolRecipe {
		t.Fatalf("expected both the ops skill and ops tool selected, got %d packs", len(out.Packs))
	}
}

// TestRecommendNoMatchEmitsEmptyArray: a description matching nothing
// must serialize packs as [] — a Go nil slice becomes `null`, and the
// composer field crashed on it with "null is not an object". Asserted
// on the JSON, not the struct, because the struct is nil either way.
func TestRecommendNoMatchEmitsEmptyArray(t *testing.T) {
	out := recommendFallback("zzzqqq unmatchable xyzzy", recommendFixture())
	if len(out.Packs) != 0 {
		t.Fatalf("fixture should not match: %+v", out.Packs)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"packs":null`) {
		t.Errorf("packs serialized as null — clients iterate this: %s", b)
	}
	if !strings.Contains(string(b), `"packs":[]`) {
		t.Errorf("want an empty array, got: %s", b)
	}
}

func TestSanitizeAgentName(t *testing.T) {
	for in, want := range map[string]string{
		"Weekly Ops!! Agent": "weekly-ops-agent",
		"--foo__bar--":       "foo-bar",
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
			"role":         "reporter",
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

// TestRecommendRejectsWeakMatches is the regression for the complaint that
// Suggest "picks the wrong ones". With flat substring scoring, a single
// common word dragged in an unrelated pack: "weekly ops report" pulled
// genesis-train on the word "report" alone, and "reviews pull requests"
// pulled a newsroom skill on "reviews". Rare terms must outweigh common
// ones, and a runner-up far below the best hit must be dropped rather
// than used to fill the quota.
func TestRecommendRejectsWeakMatches(t *testing.T) {
	catalog := []catalogPack{
		{Type: "skill", Name: "weekly-ops-report", DisplayName: "Weekly Ops Report",
			Description: "Produce the weekly ops report: orders, revenue, stuck orders."},
		{Type: "skill", Name: "genesis-train", DisplayName: "Genesis Train",
			Description: "Run the genesis demo train and report progress for each beat."},
		{Type: "skill", Name: "nl2sql-ranking-edition", DisplayName: "Ranking Edition",
			Description: "Editorial reviews and ranking for the newsroom edition."},
		{Type: "skill", Name: "parkforge-terrain-strokes", DisplayName: "Terrain Strokes",
			Description: "Sculpt terrain and build paths in the Unreal park."},
		{Type: "tool", Name: "github", DisplayName: "GitHub",
			Description: "Create issues and pull requests as a governed identity."},
	}

	names := func(r *recommendResponse) []string {
		var out []string
		for _, p := range r.Packs {
			out = append(out, p.Name)
		}
		return out
	}
	has := func(list []string, n string) bool {
		for _, x := range list {
			if x == n {
				return true
			}
		}
		return false
	}

	got := names(recommendFallback("weekly ops report on orders and revenue", catalog))
	if !has(got, "weekly-ops-report") {
		t.Errorf("lost the obvious hit: %v", got)
	}
	if has(got, "genesis-train") {
		t.Errorf(`"report" alone must not pull in genesis-train: %v`, got)
	}

	got = names(recommendFallback("an agent that reviews pull requests on github", catalog))
	if !has(got, "github") {
		t.Errorf("lost the obvious hit: %v", got)
	}
	if has(got, "nl2sql-ranking-edition") {
		t.Errorf(`"reviews" alone must not pull in the newsroom skill: %v`, got)
	}

	// A genuinely multi-signal query should still return its real matches.
	got = names(recommendFallback("sculpt terrain and build paths in unreal", catalog))
	if !has(got, "parkforge-terrain-strokes") {
		t.Errorf("strong multi-term match lost: %v", got)
	}
}
