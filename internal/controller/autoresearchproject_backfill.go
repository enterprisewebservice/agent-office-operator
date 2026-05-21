/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

// Wiki backfill — Phase 4 of the autoresearch flywheel refactor.
//
// Why this exists: every AutoResearchProject built under operator
// versions older than v0.0.71 has a wiki repo with NO
// `proposals/round-N.{yaml,md}` files and NO `results/round-N.md`
// files. The push refspec was malformed (buried `+` mid-path)
// causing go-git to silently no-op every push. The operator
// happily logged "result: round N committed" while pushing
// nothing to the remote. Sixteen-plus rounds of training across
// every project produced exactly two commits on the wiki repos:
// the initial commit + the bootstrap.
//
// Now that v0.1.0 (Phase 0) lands transactions correctly, every
// FUTURE round writes its proposal + result + dashboard. But the
// historical work — the configs that were tried, the eval_losses
// that were observed — is still lost from the deterministic
// searcher's perspective. It can't fit a TPE surrogate against
// observations it can't see.
//
// Backfill reconstructs those files from two sources:
//
//   - `Status.RecentRuns` — bounded ring on the ARP CR with each
//     completed round's eval_loss, runId, kept/reverted decision,
//     adapter URI, and round number. This is the operator's
//     internal bookkeeping; it survived the refspec bug because
//     it never went through git.
//
//   - DSP `RuntimeConfig.Parameters["qlora_config_json"]` — the
//     QLoRAConfig that was submitted as the pipeline's input.
//     One DSP `GetRun(runId)` per historical round retrieves it.
//
// The backfill is idempotent: it only writes files for rounds
// that are MISSING on the wiki AND have eval_loss data on the
// RecentRuns ring. Rounds with empty eval_loss (still in flight
// when the operator crashed, or scrape-failures) are skipped
// silently — the searcher's pair-based history loader treats
// them as if they never happened.
//
// One commit per backfill ("backfill: restore N historical rounds
// from MLMD/DSP"), one push. Failure surfaces as
// `WikiSyncOK=False` on Status.
//
// Triggered automatically: every reconcile checks whether the
// project HAS RecentRuns history but the wiki has FEWER
// proposals/ entries than RecentRuns entries. If so, backfill
// runs once and quiets down on subsequent reconciles when the
// numbers match. No CRD field, no human intervention.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
	"github.com/enterprisewebservice/agent-office-operator/internal/dsp"
	"github.com/enterprisewebservice/agent-office-operator/internal/wiki"
)

// backfillStats summarizes what one backfill turn did. Used to
// populate the Status.Conditions message and the commit message.
type backfillStats struct {
	written int      // rounds that landed proposal + result
	skipped int      // rounds with empty eval_loss or DSP fetch errors
	rounds  []int    // numeric round IDs that were written
	notices []string // per-round failure notes (DSP not found, parse error, etc.)
}

// backfillWikiHistoryIfNeeded checks whether the project's wiki
// has a complete history of its RecentRuns. If RecentRuns has
// more eval_loss-bearing entries than the wiki has
// `proposals/round-*.yaml` files, runs one backfill pass.
//
// Returns a stats struct describing what happened (zero-value if
// nothing was needed). Errors surface as `WikiSyncOK=False` on
// Status — the loop continues regardless.
func (r *AutoResearchProjectReconciler) backfillWikiHistoryIfNeeded(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
) (backfillStats, error) {
	lg := log.FromContext(ctx)

	// Count usable historical entries on the ARP CR.
	usable := 0
	for _, r := range p.Status.RecentRuns {
		if r.EvalLoss != "" {
			usable++
		}
	}
	if usable == 0 {
		// Project has no completed runs yet, or its history was
		// already wiped — nothing to backfill.
		return backfillStats{}, nil
	}

	// Open a wiki transaction to check what's already there.
	tx, err := r.beginWikiTx(ctx, p)
	if err != nil {
		return backfillStats{}, fmt.Errorf("open wiki for backfill: %w", err)
	}
	defer tx.Close()

	// Inventory existing wiki files. We compare ROUND NUMBERS
	// already on disk vs round numbers in RecentRuns. Any round
	// with eval_loss on the CR but no `proposals/round-N.yaml`
	// on the wiki gets backfilled.
	existing := map[int]bool{}
	if propFiles, err := tx.List("proposals"); err == nil {
		for _, name := range propFiles {
			n, ok := parseRoundFilename(name, ".yaml")
			if ok {
				existing[n] = true
			}
		}
	}

	// Identify rounds that need backfill.
	toBackfill := []agentofficev1alpha1.AutoResearchExperimentRecord{}
	for _, rec := range p.Status.RecentRuns {
		if rec.EvalLoss == "" {
			continue
		}
		if existing[int(rec.Round)] {
			continue
		}
		toBackfill = append(toBackfill, rec)
	}
	if len(toBackfill) == 0 {
		return backfillStats{}, nil
	}

	lg.Info("backfilling historical wiki content",
		"project", p.Name, "rounds", len(toBackfill),
		"usableInRecentRuns", usable, "onWiki", len(existing))

	// Build a DSP client so we can fetch each historical run's
	// input params.
	dspClient, err := dspClientFor(ctx, r.Client, p.Namespace)
	if err != nil {
		return backfillStats{}, fmt.Errorf("dsp client: %w", err)
	}

	stats := backfillStats{}
	for _, rec := range toBackfill {
		written, err := backfillOneRound(ctx, dspClient, tx, p.Name, rec)
		if err != nil {
			stats.notices = append(stats.notices,
				fmt.Sprintf("round %d: %v", rec.Round, err))
			stats.skipped++
			continue
		}
		if written {
			stats.written++
			stats.rounds = append(stats.rounds, int(rec.Round))
		}
	}

	if stats.written == 0 {
		// Nothing to commit. Log notices and move on.
		if len(stats.notices) > 0 {
			lg.Info("backfill skipped all rounds", "notices", strings.Join(stats.notices, "; "))
		}
		return stats, nil
	}

	sort.Ints(stats.rounds)
	commitMsg := fmt.Sprintf("backfill: restore %d historical rounds from DSP (%s)",
		stats.written, summarizeRoundRange(stats.rounds))
	if werr := tx.CommitAndPush(ctx, commitMsg); werr != nil {
		return stats, fmt.Errorf("commit backfill: %w", werr)
	}
	lg.Info("backfill committed",
		"rounds", stats.written, "skipped", stats.skipped, "msg", commitMsg)
	return stats, nil
}

// backfillOneRound stages the proposal + result files for a single
// historical round. Returns (true, nil) if both files were staged,
// (false, nil) if the round was silently skipped (DSP didn't have
// the config), (false, err) on an unexpected failure.
func backfillOneRound(
	ctx context.Context,
	dspClient *dsp.Client,
	tx *wiki.Transaction,
	projectName string,
	rec agentofficev1alpha1.AutoResearchExperimentRecord,
) (bool, error) {
	// 1. Fetch the run from DSP to get its input parameters.
	run, err := dspClient.GetRun(ctx, rec.RunID)
	if err != nil {
		return false, fmt.Errorf("get DSP run: %w", err)
	}
	if run == nil {
		// DSP no longer has this run (pruned or deleted).
		// Skip — we can't reconstruct the config without the
		// pipeline parameters.
		return false, nil
	}

	cfgJSONRaw, ok := run.RuntimeConfig.Parameters["qlora_config_json"]
	if !ok {
		return false, fmt.Errorf("run has no qlora_config_json parameter (param keys: %v)", paramKeys(run.RuntimeConfig.Parameters))
	}
	cfgJSONStr, ok := cfgJSONRaw.(string)
	if !ok {
		return false, fmt.Errorf("qlora_config_json is %T, want string", cfgJSONRaw)
	}

	var cfg QLoRAConfig
	if err := json.Unmarshal([]byte(cfgJSONStr), &cfg); err != nil {
		return false, fmt.Errorf("parse qlora_config_json: %w", err)
	}
	// Round on the cfg might not match rec.Round if the CR was
	// edited. Trust the CR.
	cfg.Round = int(rec.Round)
	if cfg.Notes == "" {
		cfg.Notes = fmt.Sprintf("backfilled from DSP run %s", rec.RunID)
	}

	cfgYAML, err := yaml.Marshal(cfg)
	if err != nil {
		return false, fmt.Errorf("marshal config: %w", err)
	}
	if werr := tx.Write(fmt.Sprintf("proposals/round-%d.yaml", rec.Round), cfgYAML); werr != nil {
		return false, werr
	}

	// 2. Build the proposal Markdown counterpart.
	md := buildBackfilledProposalMD(int(rec.Round), cfg, rec)
	if werr := tx.Write(fmt.Sprintf("proposals/round-%d.md", rec.Round), []byte(md)); werr != nil {
		return false, werr
	}

	// 3. Build the result Markdown from the CR record.
	evalLoss, err := strconv.ParseFloat(rec.EvalLoss, 64)
	if err != nil {
		return false, fmt.Errorf("parse eval_loss %q: %w", rec.EvalLoss, err)
	}
	keptStr := "reverted"
	if rec.Kept != nil && *rec.Kept {
		keptStr = "kept"
	}
	resultMD := fmt.Sprintf(`# Round %d — result (backfilled)

| Field | Value |
|-------|-------|
| eval_loss | %f |
| Decision | **%s** |
| Adapter URI | `+"`%s`"+` |
| Source | backfilled from Status.RecentRuns + DSP run %s |

_Reconstructed by the operator's backfill path. The original
push of this round's result was lost to the pre-v0.0.71 refspec
bug; the data was recovered from operator-side bookkeeping +
DSP RuntimeConfig._
`,
		rec.Round, evalLoss, keptStr, rec.AdapterArtifactURI, rec.RunID)
	if werr := tx.Write(fmt.Sprintf("results/round-%d.md", rec.Round), []byte(resultMD)); werr != nil {
		return false, werr
	}

	return true, nil
}

// buildBackfilledProposalMD constructs the human-readable proposal
// page for a backfilled round. Distinguishes itself from a
// real-time proposal by labeling itself "backfilled."
func buildBackfilledProposalMD(round int, cfg QLoRAConfig, rec agentofficev1alpha1.AutoResearchExperimentRecord) string {
	cfgYAML, _ := yaml.Marshal(cfg)
	startedAt := "(unknown)"
	if rec.StartedAt != nil {
		startedAt = rec.StartedAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	return fmt.Sprintf(`# Round %d — proposal (backfilled)

| Field | Value |
|-------|-------|
| Source | reconstructed from DSP RuntimeConfig.Parameters |
| Run ID | `+"`%s`"+` |
| Started | %s |
| Experimenter (at submit time) | %s |

**Config (machine-readable in `+"`round-%d.yaml`"+`):**

`+"```yaml\n%s```\n",
		round, rec.RunID, startedAt, rec.Experimenter, round, string(cfgYAML))
}

// parseRoundFilename matches `round-<N><suffix>` filenames and
// returns the round number. Suffix is matched literally (e.g.
// ".yaml" or ".md").
func parseRoundFilename(name, suffix string) (int, bool) {
	if !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	stem := strings.TrimSuffix(name, suffix)
	if !strings.HasPrefix(stem, "round-") {
		return 0, false
	}
	n, err := strconv.Atoi(stem[len("round-"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

// paramKeys is a tiny helper for the "what parameters does this
// run have" error message — DSP runs from old pipeline versions
// may have different parameter names than the current operator
// expects, and we want the diagnosis to be immediate.
func paramKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// summarizeRoundRange formats a sorted round-number list as a
// compact range string for the commit message — "rounds 1-15"
// instead of "rounds 1,2,3,4,...,15".
func summarizeRoundRange(rounds []int) string {
	if len(rounds) == 0 {
		return ""
	}
	if len(rounds) == 1 {
		return fmt.Sprintf("round %d", rounds[0])
	}
	// Detect contiguous range.
	contiguous := true
	for i := 1; i < len(rounds); i++ {
		if rounds[i] != rounds[i-1]+1 {
			contiguous = false
			break
		}
	}
	if contiguous {
		return fmt.Sprintf("rounds %d-%d", rounds[0], rounds[len(rounds)-1])
	}
	// Fall back to comma-separated.
	parts := make([]string, len(rounds))
	for i, n := range rounds {
		parts[i] = strconv.Itoa(n)
	}
	return "rounds " + strings.Join(parts, ", ")
}
