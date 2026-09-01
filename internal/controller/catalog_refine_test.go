/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"
)

func refineFixtureByName() (map[string]catalogPack, []catalogPack) {
	packs := recommendFixture()
	byName := map[string]catalogPack{}
	for _, p := range packs {
		byName[p.Name] = p
	}
	return byName, packs
}

// TestApplyRefineOps pins the mechanical contract: add appends the real
// catalog entry, remove drops exactly the named selection, set edits
// exactly the named identity field, and everything the ops do not name
// survives untouched.
func TestApplyRefineOps(t *testing.T) {
	byName, _ := refineFixtureByName()
	cur := []recommendPack{
		{catalogPack: byName["weekly-ops-report"]},
		{catalogPack: byName["ops-metrics"]},
	}
	id := recommendIdentity{
		Name: "ops-reporter", DisplayName: "Ops Reporter",
		Emoji: "📊", Role: "reporter", SystemPrompt: "You report. Never fabricate.",
	}
	outID, next, notes, changed := applyRefineOps(id, cur, []refineOp{
		{Op: "remove", Name: "ops-metrics"},
		{Op: "add", Name: "wiki-write", Reason: "user asked"},
		{Op: "set", Field: "displayName", Value: "Ops Scribe"},
	}, byName)

	if !changed {
		t.Fatal("changed: want true")
	}
	names := map[string]bool{}
	for _, p := range next {
		names[p.Name] = true
	}
	if names["ops-metrics"] || !names["wiki-write"] || !names["weekly-ops-report"] {
		t.Errorf("selection after ops: %+v", next)
	}
	if outID.DisplayName != "Ops Scribe" {
		t.Errorf("displayName: %q", outID.DisplayName)
	}
	// Untouched fields survive.
	if outID.Name != "ops-reporter" || outID.SystemPrompt != id.SystemPrompt {
		t.Errorf("identity drifted: %+v", outID)
	}
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
}

// TestApplyRefineOpsInvalid pins the failure posture: hallucinated adds
// and misdirected removes become notes, never errors, and never touch
// the composition.
func TestApplyRefineOpsInvalid(t *testing.T) {
	byName, _ := refineFixtureByName()
	cur := []recommendPack{{catalogPack: byName["weekly-ops-report"]}}
	id := recommendIdentity{Name: "a", SystemPrompt: "s"}

	outID, next, notes, changed := applyRefineOps(id, cur, []refineOp{
		{Op: "add", Name: "does-not-exist"},
		{Op: "remove", Name: "never-selected"},
		{Op: "set", Field: "name", Value: "!!!"},
		{Op: "frobnicate", Name: "x"},
	}, byName)

	if changed {
		t.Errorf("changed: want false, notes=%v", notes)
	}
	if len(next) != 1 || next[0].Name != "weekly-ops-report" {
		t.Errorf("selection touched: %+v", next)
	}
	if outID.Name != "a" {
		t.Errorf("name touched: %q", outID.Name)
	}
	if len(notes) != 4 {
		t.Errorf("want 4 notes, got %v", notes)
	}
}

// TestRefineFallbackRemove: an explicit remove verb matches the current
// selection by token overlap and produces exactly one remove op.
func TestRefineFallbackRemove(t *testing.T) {
	byName, packs := refineFixtureByName()
	cur := []recommendPack{
		{catalogPack: byName["weekly-ops-report"]},
		{catalogPack: byName["wiki-write"]},
	}
	reply, ops := refineFallback("please remove the wiki write skill", cur, packs)
	if len(ops) != 1 || ops[0].Op != "remove" || ops[0].Name != "wiki-write" {
		t.Fatalf("ops: %+v (reply %q)", ops, reply)
	}
}

// TestRefineFallbackAdd: an explicit add verb matches the catalog.
func TestRefineFallbackAdd(t *testing.T) {
	byName, packs := refineFixtureByName()
	cur := []recommendPack{{catalogPack: byName["weekly-ops-report"]}}
	reply, ops := refineFallback("add github", cur, packs)
	if len(ops) != 1 || ops[0].Op != "add" || ops[0].Name != "github" {
		t.Fatalf("ops: %+v (reply %q)", ops, reply)
	}
}

// TestRefineFallbackQuestion: a question changes nothing and the reply
// names real catalog entries.
func TestRefineFallbackQuestion(t *testing.T) {
	byName, packs := refineFixtureByName()
	cur := []recommendPack{{catalogPack: byName["weekly-ops-report"]}}
	reply, ops := refineFallback("is there anything for wiki pages?", cur, packs)
	if len(ops) != 0 {
		t.Fatalf("question must not produce ops: %+v", ops)
	}
	if !strings.Contains(reply, "wiki-write") {
		t.Errorf("reply doesn't ground in the catalog: %q", reply)
	}
}
