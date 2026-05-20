/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package search

import (
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// Hypotheses is the strategist's deliverable — a YAML document the
// LLM commits to the wiki as `HYPOTHESES.md` and the searcher
// reads on every reconcile to bias its sampling.
//
// Design rule: every field is OPTIONAL. The searcher must continue
// to function even if HYPOTHESES.md is empty, malformed, or stale.
// The strategist is enrichment; the kernel is the floor.
//
// YAML shape:
//
//	generated_at: 2026-05-21T14:00:00Z
//	generated_after_round: 18
//	hypothesis: |
//	  Plateau at 0.52 across rounds 12-18. Literature suggests…
//	adjust_search_space:
//	  learning_rate: {low: 5.0e-5, high: 2.0e-4, log: true}
//	  lora_dropout: {low: 0.05, high: 0.20}
//	  target_modules:
//	    choices:
//	      - "q_proj,v_proj"
//	      - "q_proj,v_proj,k_proj,o_proj,gate_proj,up_proj"
//	references:
//	  - topics/lora-plus-2024.md
//	  - raw/clips/2024-lora-plus.md
//
// Markdown wrapping: the file is named HYPOTHESES.md (.md, not
// .yaml) so it renders nicely in Obsidian and on GitHub. Real
// strategists wrap the YAML in a code fence:
//
//	# Hypotheses
//
//	Plateau at 0.52 across rounds 12-18…
//
//	```yaml
//	generated_at: ...
//	adjust_search_space:
//	  ...
//	```
//
// ParseHypotheses extracts the first fenced YAML block; if none is
// found it tries to parse the whole body as YAML. Either way the
// human-friendly Markdown around the YAML is preserved on disk for
// readers and ignored by the parser.
type Hypotheses struct {
	GeneratedAt         string                `json:"generated_at,omitempty"`
	GeneratedAfterRound int                   `json:"generated_after_round,omitempty"`
	Hypothesis          string                `json:"hypothesis,omitempty"`
	AdjustSearchSpace   map[string]Adjustment `json:"adjust_search_space,omitempty"`
	References          []string              `json:"references,omitempty"`
}

// Adjustment narrows or replaces the spec for one parameter. Every
// field is optional; the applier interprets fields relative to the
// existing spec for that param (Int / Float / Categorical). Fields
// that don't apply to the param's type are silently ignored.
//
//   - For an IntSpec or FloatSpec: Low / High / Step / Log adjust
//     the existing bounds. The new bounds must be a SUB-range of
//     the original (the searcher rejects widening to prevent the
//     LLM from inventing dimensions outside what the operator
//     defined).
//
//   - For a CategoricalSpec: Choices REPLACES the existing list
//     (the LLM can re-order, prune, or expand to new strings —
//     no validation against the original; categorical universes
//     are explicitly meant to be discoverable).
//
// Pointer-shaped numerics so we can distinguish "omitted" from
// "explicitly set to zero." Defaults (nil pointer) → keep the
// original value.
type Adjustment struct {
	Low     *float64 `json:"low,omitempty"`
	High    *float64 `json:"high,omitempty"`
	Step    *int     `json:"step,omitempty"`
	Log     *bool    `json:"log,omitempty"`
	Choices []string `json:"choices,omitempty"`
}

// ParseHypotheses extracts a Hypotheses from a HYPOTHESES.md body.
// Returns (zero-value, nil) for an empty / nil body — callers treat
// that as "no hypotheses yet, use defaults." Returns an error only
// for genuinely malformed YAML so the caller can distinguish
// "absent" from "corrupt."
func ParseHypotheses(body []byte) (Hypotheses, error) {
	if len(body) == 0 {
		return Hypotheses{}, nil
	}
	yamlBlock := extractYAMLBlock(string(body))
	if yamlBlock == "" {
		return Hypotheses{}, nil
	}
	var h Hypotheses
	if err := yaml.Unmarshal([]byte(yamlBlock), &h); err != nil {
		return Hypotheses{}, fmt.Errorf("ParseHypotheses: %w", err)
	}
	return h, nil
}

// extractYAMLBlock returns the contents of the first ```yaml fenced
// block in s. If no fence is found, returns s itself (whitespace-
// trimmed) on the assumption the body is plain YAML. Returns "" if
// the whole body is empty.
//
// The fence-detection is intentionally permissive — accepts ```yaml,
// ```yml, or just ``` (case-insensitive on the language tag) — so
// strategist agents writing slightly different markdown render
// styles still parse correctly.
func extractYAMLBlock(s string) string {
	lower := strings.ToLower(s)
	// Find the first ``` line. Each candidate language tag is
	// tried in order so we don't accidentally pick up `bash or
	// `python.
	for _, tag := range []string{"```yaml", "```yml", "```"} {
		idx := strings.Index(lower, tag)
		if idx < 0 {
			continue
		}
		// Move past the tag + the newline.
		nlAfter := strings.IndexByte(s[idx:], '\n')
		if nlAfter < 0 {
			continue
		}
		start := idx + nlAfter + 1
		// Find the closing fence.
		end := strings.Index(s[start:], "```")
		if end < 0 {
			continue
		}
		return strings.TrimSpace(s[start : start+end])
	}
	// No fence — treat the whole body as YAML.
	return strings.TrimSpace(s)
}

// ErrAdjustmentRejected is returned by Apply when an Adjustment
// would widen a bound beyond the original Spec (which would let
// the LLM invent dimensions outside the operator-defined space).
// Caller logs + falls back to the un-adjusted space — never
// crashes.
var ErrAdjustmentRejected = errors.New("search: adjustment rejected (out of bounds or unknown param)")

// Apply layers Hypotheses adjustments onto a Space, returning a
// new Space. The original is not mutated.
//
// Per-adjustment behavior:
//   - Unknown param name → ignored (forward-compatible: an LLM
//     reading concepts/* and recommending a hypothetical param the
//     operator hasn't exposed yet doesn't break the round).
//   - Int/Float adjustment widening beyond the original bounds →
//     clamped to the original bounds with a per-param notice
//     returned in `notices`.
//   - Categorical with empty Choices → ignored.
//   - Float adjustment toggling Log on a non-log param → applied
//     (LLM is allowed to switch to log-uniform if it learns the
//     dimension is multiplicative). Toggling Log → requires Low > 0;
//     otherwise the adjustment is rejected for that param.
//
// `notices` are human-readable strings describing soft rejections
// (clamps, log-with-zero-low, etc.) — caller surfaces them on
// Status / DASHBOARD.md so the strategist can see what got
// ignored on its next turn.
func (h Hypotheses) Apply(orig Space) (adjusted Space, notices []string) {
	// Deep-copy the original so callers can keep the un-adjusted
	// Space for fallback.
	adjusted = Space{
		Ints:         make(map[string]IntSpec, len(orig.Ints)),
		Floats:       make(map[string]FloatSpec, len(orig.Floats)),
		Categoricals: make(map[string]CategoricalSpec, len(orig.Categoricals)),
	}
	for k, v := range orig.Ints {
		adjusted.Ints[k] = v
	}
	for k, v := range orig.Floats {
		adjusted.Floats[k] = v
	}
	for k, v := range orig.Categoricals {
		adjusted.Categoricals[k] = CategoricalSpec{Choices: append([]string{}, v.Choices...)}
	}

	for name, adj := range h.AdjustSearchSpace {
		if spec, ok := adjusted.Ints[name]; ok {
			newSpec, n := applyToInt(spec, adj, name)
			adjusted.Ints[name] = newSpec
			notices = append(notices, n...)
			continue
		}
		if spec, ok := adjusted.Floats[name]; ok {
			newSpec, n := applyToFloat(spec, adj, name)
			adjusted.Floats[name] = newSpec
			notices = append(notices, n...)
			continue
		}
		if spec, ok := adjusted.Categoricals[name]; ok {
			newSpec, n := applyToCategorical(spec, adj, name)
			adjusted.Categoricals[name] = newSpec
			notices = append(notices, n...)
			continue
		}
		// Unknown param — ignored. Notice surfaces the
		// discrepancy so the next strategist turn can clean up.
		notices = append(notices, fmt.Sprintf("hypothesis param %q is not in the search space — ignored", name))
	}
	return adjusted, notices
}

func applyToInt(spec IntSpec, adj Adjustment, name string) (IntSpec, []string) {
	out := spec
	var notices []string
	if adj.Low != nil {
		v := int(*adj.Low)
		if v < spec.Low {
			notices = append(notices, fmt.Sprintf("%s: Low=%d clamped to spec floor %d", name, v, spec.Low))
			v = spec.Low
		}
		out.Low = v
	}
	if adj.High != nil {
		v := int(*adj.High)
		if v > spec.High {
			notices = append(notices, fmt.Sprintf("%s: High=%d clamped to spec ceiling %d", name, v, spec.High))
			v = spec.High
		}
		out.High = v
	}
	if adj.Step != nil {
		if *adj.Step >= 0 {
			out.Step = *adj.Step
		}
	}
	if out.Low > out.High {
		notices = append(notices, fmt.Sprintf("%s: post-adjustment Low (%d) > High (%d); reverting to original", name, out.Low, out.High))
		return spec, notices
	}
	return out, notices
}

func applyToFloat(spec FloatSpec, adj Adjustment, name string) (FloatSpec, []string) {
	out := spec
	var notices []string
	if adj.Low != nil {
		if *adj.Low < spec.Low {
			notices = append(notices, fmt.Sprintf("%s: Low=%g clamped to spec floor %g", name, *adj.Low, spec.Low))
			out.Low = spec.Low
		} else {
			out.Low = *adj.Low
		}
	}
	if adj.High != nil {
		if *adj.High > spec.High {
			notices = append(notices, fmt.Sprintf("%s: High=%g clamped to spec ceiling %g", name, *adj.High, spec.High))
			out.High = spec.High
		} else {
			out.High = *adj.High
		}
	}
	if adj.Log != nil {
		out.Log = *adj.Log
	}
	if out.Log && out.Low <= 0 {
		notices = append(notices, fmt.Sprintf("%s: Log=true requires Low > 0 (got %g); reverting to original", name, out.Low))
		return spec, notices
	}
	if out.Low > out.High {
		notices = append(notices, fmt.Sprintf("%s: post-adjustment Low (%g) > High (%g); reverting to original", name, out.Low, out.High))
		return spec, notices
	}
	return out, notices
}

func applyToCategorical(spec CategoricalSpec, adj Adjustment, name string) (CategoricalSpec, []string) {
	if len(adj.Choices) == 0 {
		return spec, nil
	}
	// REPLACE — categoricals are explicitly meant to be
	// discoverable by the strategist (the LLM is allowed to invent
	// new target_modules combinations the operator never imagined).
	return CategoricalSpec{Choices: append([]string{}, adj.Choices...)}, nil
}
