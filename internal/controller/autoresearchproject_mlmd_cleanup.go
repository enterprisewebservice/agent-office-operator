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
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	// Anonymous import registers the MySQL driver with database/sql.
	_ "github.com/go-sql-driver/mysql"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// kfp v2 keeps MLMD Execution records in the DSP MariaDB. When a
// pipeline run dies mid-flight, kfp's launcher doesn't always
// close its Executions — they stay `last_known_state = 2` (RUNNING)
// indefinitely, polluting the OpenShift AI dashboard's Pipelines/
// Executions view with stale entries.
//
// This janitor cleans them up DETERMINISTICALLY: for each Execution
// still marked RUNNING, look up the DSP run it belongs to. If DSP
// itself reports that run as terminal (FAILED / SUCCEEDED /
// CANCELED / SKIPPED) or the run is gone, the Execution is an
// orphan and gets transitioned to FAILED. If DSP still has the run
// alive, the Execution is left alone — never falsely killed.
//
// We discovered the schema linkage by inspection: each Execution
// is associated (via the Association table) with a Context of
// `system.PipelineRun` type, and that Context's name field equals
// the DSP run UUID.

const (
	// MLMD state codes (from the ml-metadata schema).
	mlmdStateUnknown  = 0
	mlmdStateNew      = 1
	mlmdStateRunning  = 2
	mlmdStateComplete = 3
	mlmdStateFailed   = 4
	mlmdStateCached   = 5
	mlmdStateCanceled = 6

	// Cooldown between cleanup passes per namespace. Reads the
	// MariaDB + makes one DSP API call per stuck Execution; this
	// is cheap, but doesn't need to run more often than once per
	// autoresearch cycle.
	mlmdCleanupCooldown = 10 * time.Minute

	// Cap how many orphan candidates we check per pass — prevents
	// the operator from blocking reconcile for minutes if some
	// pathological state leaves thousands stuck. The next pass
	// picks up the remainder.
	mlmdMaxOrphansPerPass = 500
)

// mlmdCleanupTracker tracks when we last ran the janitor per
// namespace, so multiple AutoResearchProjects in the same ns
// don't all hit the DB simultaneously.
type mlmdCleanupTracker struct {
	mu      sync.Mutex
	lastRun map[string]time.Time
}

var mlmdCleanups = &mlmdCleanupTracker{lastRun: map[string]time.Time{}}

func (t *mlmdCleanupTracker) shouldRun(namespace string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if last, ok := t.lastRun[namespace]; ok && now.Sub(last) < mlmdCleanupCooldown {
		return false
	}
	t.lastRun[namespace] = now
	return true
}

// cleanupStaleMLMDExecutions deterministically transitions
// orphaned RUNNING Executions to FAILED. Each candidate is
// verified against DSP's actual run state before being touched.
// Returns the row count actually transitioned.
//
// Safe to call from every reconcile — the cooldown above
// prevents DB hammering, and the deterministic check protects
// genuinely-active runs from being marked failed.
func (r *AutoResearchProjectReconciler) cleanupStaleMLMDExecutions(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
) (int64, error) {
	log := logf.FromContext(ctx)
	if !mlmdCleanups.shouldRun(p.Namespace) {
		return 0, nil
	}

	dsn, err := mlmdDSN(ctx, r.Client, p.Namespace)
	if err != nil {
		return 0, fmt.Errorf("build MariaDB DSN: %w", err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return 0, fmt.Errorf("open mysql: %w", err)
	}
	defer db.Close()

	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Find all (execution_id, dsp_run_id) pairs where the
	// Execution is still RUNNING in MLMD. dsp_run_id comes from
	// the system.PipelineRun Context the Execution is associated
	// with — that Context's name field IS the DSP run UUID.
	rows, err := db.QueryContext(dbCtx, `
		SELECT e.id, c.name
		  FROM Execution e
		  JOIN Association a ON a.execution_id = e.id
		  JOIN Context c     ON a.context_id   = c.id
		  JOIN Type t        ON c.type_id      = t.id
		 WHERE e.last_known_state = ?
		   AND t.name = 'system.PipelineRun'
		 LIMIT ?
	`, mlmdStateRunning, mlmdMaxOrphansPerPass)
	if err != nil {
		return 0, fmt.Errorf("query stuck executions: %w", err)
	}

	type candidate struct {
		executionID int64
		dspRunID    string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.executionID, &c.dspRunID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate candidates: %w", err)
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	// For each candidate, ask DSP whether the run is actually
	// terminal. Cache per-run lookups so multi-step pipelines
	// (one DAGExecution + N ContainerExecutions sharing a
	// run_id) only cost one DSP API call.
	dspClient, err := dspClientFor(ctx, r.Client, p.Namespace)
	if err != nil {
		return 0, fmt.Errorf("dsp client: %w", err)
	}
	runTerminal := map[string]bool{} // dspRunID → true if confirmed orphan
	var orphanExecIDs []int64
	for _, c := range candidates {
		t, known := runTerminal[c.dspRunID]
		if !known {
			run, gerr := dspClient.GetRun(ctx, c.dspRunID)
			switch {
			case gerr != nil && strings.Contains(gerr.Error(), "404"):
				// Run was deleted entirely — definitely orphan.
				t = true
			case gerr != nil:
				// Transient DSP error — skip this candidate
				// this pass, try next time.
				continue
			default:
				t = isRunTerminal(run.State)
			}
			runTerminal[c.dspRunID] = t
		}
		if t {
			orphanExecIDs = append(orphanExecIDs, c.executionID)
		}
	}
	if len(orphanExecIDs) == 0 {
		log.V(1).Info("mlmd janitor: no orphans this pass",
			"candidates", len(candidates), "ns", p.Namespace)
		return 0, nil
	}

	// Transition only the confirmed orphans. Build a single
	// IN-clause UPDATE so this is one round trip.
	placeholders := strings.Repeat("?,", len(orphanExecIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(orphanExecIDs)+1)
	args = append(args, mlmdStateFailed)
	for _, id := range orphanExecIDs {
		args = append(args, id)
	}
	q := fmt.Sprintf(`UPDATE Execution SET last_known_state = ? WHERE id IN (%s)`, placeholders)
	result, err := db.ExecContext(dbCtx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("UPDATE Execution orphans: %w", err)
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected > 0 {
		log.Info("mlmd janitor: transitioned confirmed-orphan executions to FAILED",
			"rows", rowsAffected,
			"checkedRuns", len(runTerminal),
			"ns", p.Namespace)
	}
	return rowsAffected, nil
}

// mlmdDSN reads the DSP MariaDB credentials from the
// ds-pipeline-db-dspa Secret (created by the DSP operator in
// every DSPA-enabled namespace) and returns a database/sql DSN
// pointing at the in-cluster mariadb-dspa Service.
func mlmdDSN(ctx context.Context, c client.Client, namespace string) (string, error) {
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      "ds-pipeline-db-dspa",
	}, &sec); err != nil {
		return "", fmt.Errorf("get ds-pipeline-db-dspa secret: %w", err)
	}
	user := strings.TrimSpace(string(sec.Data["username"]))
	if user == "" {
		user = "mlpipeline" // DSP default
	}
	pass := strings.TrimSpace(string(sec.Data["password"]))
	if pass == "" {
		return "", fmt.Errorf("ds-pipeline-db-dspa secret has empty password")
	}
	dbname := strings.TrimSpace(string(sec.Data["database"]))
	if dbname == "" {
		dbname = "mlpipeline" // DSP default
	}
	host := fmt.Sprintf("mariadb-dspa.%s.svc.cluster.local:3306", namespace)
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&timeout=5s",
		user, pass, host, dbname), nil
}
