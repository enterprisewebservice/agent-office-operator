/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package search

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// FileLister returns the names of files under a relative directory
// in the wiki repo. Decoupled so callers can pass either a real
// `wiki.Transaction` (production) or a stub (tests). Glob patterns
// are not supported — return all files in the directory and the
// loader filters by suffix.
type FileLister interface {
	List(dir string) ([]string, error)
	Read(relPath string) ([]byte, error)
}

// roundRE matches "round-<N>.yaml" or "round-<N>.md".
var roundRE = regexp.MustCompile(`^round-(\d+)\.(yaml|md)$`)

// evalLossRE extracts the eval_loss value from a results/round-N.md
// table row. The renderer (autoresearchproject_agent.go's
// wikiPushResult) emits the row as:
//
//     | eval_loss | 0.521735 |
//
// so a forgiving extractor that finds "| eval_loss | <number> |"
// covers both the current format and any sensible variants
// (different decimal precision, surrounding whitespace).
var evalLossRE = regexp.MustCompile(`(?i)\|\s*eval_loss\s*\|\s*([0-9eE.+-]+)\s*\|`)

// LoadWikiHistory reads the proposals/ + results/ directories of a
// wiki and returns the list of completed (config, eval_loss)
// observations in round-number order.
//
// Pairing logic: a round contributes an Observation iff BOTH
// `proposals/round-N.yaml` and `results/round-N.md` are present.
// Rounds with only a proposal (training still in flight) or only a
// result (somehow lost the proposal) are skipped — the searcher
// works fine with partial history; it just doesn't see those
// rounds.
//
// All file-read errors are surfaced (one bad file shouldn't be
// silently ignored, especially during the v0.2.0 cutover where
// silent skipping would mask a parse regression).
func LoadWikiHistory(fl FileLister, paramSchema Space) ([]Observation, error) {
	// 1. List proposals/ to find rounds.
	proposalFiles, err := fl.List("proposals")
	if err != nil {
		// "directory doesn't exist" → no history yet. The kernel
		// is expected to handle empty history (warmup); the
		// listing should distinguish ENOENT from other errors.
		// We treat any non-nil error here as "no history" since
		// the wiki may be brand-new. Errors that prevent later
		// reads will surface there.
		return nil, nil
	}
	rounds := map[int]struct{}{}
	for _, f := range proposalFiles {
		m := roundRE.FindStringSubmatch(f)
		if m == nil {
			continue
		}
		// We require the YAML form; .md is the human-readable
		// counterpart and doesn't contain the config.
		if m[2] != "yaml" {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("LoadWikiHistory: parse round number from %q: %w", f, err)
		}
		rounds[n] = struct{}{}
	}
	if len(rounds) == 0 {
		return nil, nil
	}

	// 2. List results/ to find rounds that completed.
	resultFiles, err := fl.List("results")
	if err != nil || len(resultFiles) == 0 {
		// proposals exist but no results — every round is in
		// flight. No completed history yet.
		return nil, nil
	}
	completed := map[int]string{}
	for _, f := range resultFiles {
		m := roundRE.FindStringSubmatch(f)
		if m == nil || m[2] != "md" {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("LoadWikiHistory: parse round number from %q: %w", f, err)
		}
		if _, ok := rounds[n]; ok {
			completed[n] = f
		}
	}
	if len(completed) == 0 {
		return nil, nil
	}

	// 3. For each completed round, read the YAML + results.md
	//    and assemble an Observation.
	sortedRounds := make([]int, 0, len(completed))
	for n := range completed {
		sortedRounds = append(sortedRounds, n)
	}
	sort.Ints(sortedRounds)

	out := make([]Observation, 0, len(sortedRounds))
	for _, n := range sortedRounds {
		obs, err := loadRound(fl, n, paramSchema)
		if err != nil {
			return nil, fmt.Errorf("LoadWikiHistory: round %d: %w", n, err)
		}
		if obs != nil {
			out = append(out, *obs)
		}
	}
	return out, nil
}

// loadRound assembles one Observation from
// `proposals/round-N.yaml` + `results/round-N.md`. Returns
// (nil, nil) if either file is missing/empty — those rounds are
// silently skipped. Returns an error only on parse failures.
func loadRound(fl FileLister, round int, paramSchema Space) (*Observation, error) {
	propPath := filepath.Join("proposals", fmt.Sprintf("round-%d.yaml", round))
	propRaw, err := fl.Read(propPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", propPath, err)
	}
	if len(propRaw) == 0 {
		return nil, nil
	}

	resPath := filepath.Join("results", fmt.Sprintf("round-%d.md", round))
	resRaw, err := fl.Read(resPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", resPath, err)
	}
	if len(resRaw) == 0 {
		return nil, nil
	}

	evalLoss, ok := parseEvalLoss(string(resRaw))
	if !ok {
		// Result file exists but has no eval_loss row. That's
		// not necessarily a bug — partial reverts can land —
		// so skip.
		return nil, nil
	}

	params, err := parseProposalYAML(propRaw, paramSchema)
	if err != nil {
		return nil, fmt.Errorf("parse proposal %s: %w", propPath, err)
	}

	return &Observation{Params: params, EvalLoss: evalLoss}, nil
}

// parseEvalLoss extracts the eval_loss numeric value from a
// results/round-N.md body. Returns (0, false) if not found.
func parseEvalLoss(body string) (float64, bool) {
	m := evalLossRE.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(m[1]), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseProposalYAML extracts only the params named in paramSchema
// from a proposal YAML file. Unknown fields are ignored, which
// keeps the loader robust to schema changes (e.g. operator adds
// a new search dim — old proposal YAMLs just don't contribute
// that dim's history).
//
// We use a minimal, inline YAML parser rather than the full
// sigs.k8s.io/yaml package because the proposal YAML is known
// to be flat key:value pairs (no nesting, no anchors). This
// keeps the package's dependency surface tight and avoids
// pulling YAML parsing into the search-kernel hot path.
func parseProposalYAML(body []byte, schema Space) (map[string]any, error) {
	params := map[string]any{}
	lines := strings.Split(string(body), "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		valRaw := strings.TrimSpace(line[colon+1:])
		// Strip surrounding quotes if present.
		valRaw = strings.TrimSuffix(strings.TrimPrefix(valRaw, `"`), `"`)
		valRaw = strings.TrimSuffix(strings.TrimPrefix(valRaw, "'"), "'")

		// Lists like target_modules: [q_proj, v_proj] — we
		// don't currently search over these, so skip.
		if strings.HasPrefix(valRaw, "[") {
			continue
		}

		if _, ok := schema.Ints[key]; ok {
			n, err := strconv.Atoi(valRaw)
			if err != nil {
				return nil, fmt.Errorf("param %q: not an int (%q)", key, valRaw)
			}
			params[key] = n
			continue
		}
		if _, ok := schema.Floats[key]; ok {
			f, err := strconv.ParseFloat(valRaw, 64)
			if err != nil {
				return nil, fmt.Errorf("param %q: not a float (%q)", key, valRaw)
			}
			params[key] = f
			continue
		}
		if _, ok := schema.Categoricals[key]; ok {
			params[key] = valRaw
			continue
		}
		// Unknown key — ignore. Don't error: the proposal
		// YAML may carry "round", "notes", "target_modules"
		// fields that aren't part of the search dim.
	}
	return params, nil
}
