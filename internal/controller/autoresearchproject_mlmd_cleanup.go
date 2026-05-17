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

	// Retention for FAILED Executions per type. Beyond this,
	// older Failed Executions are deleted entirely from MLMD
	// (including their ExecutionProperty/Event/Association rows)
	// so the UI doesn't drown in historical failures from
	// iteration. 10 is enough to spot a regression pattern,
	// not enough to fill the page. SUCCEEDED Executions are
	// kept indefinitely — they're real research history.
	mlmdFailedRetention = 10
)

// mlmdPrunableTypes lists the Execution type names we GC. Limited
// to kfp v2's standard pipeline-step types — we don't want to
// touch any Executions another tool may have produced.
var mlmdPrunableTypes = []string{
	"system.DAGExecution",
	"system.ContainerExecution",
}

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

	// Now also prune old FAILED Executions beyond the retention
	// cap. This runs in the same DB connection / cooldown window
	// as the orphan transition above, so we get both housekeeping
	// passes for one cooldown's cost.
	if pruned, perr := r.pruneOldFailedExecutions(dbCtx, db, p.Namespace); perr != nil {
		log.Error(perr, "pruning old FAILED Executions (continuing)")
	} else if pruned > 0 {
		log.Info("mlmd janitor: pruned old FAILED Executions",
			"rows", pruned, "ns", p.Namespace)
		rowsAffected += pruned
	}
	return rowsAffected, nil
}

// pruneOldFailedExecutions deletes FAILED Executions beyond the
// mlmdFailedRetention cap per Execution type. Removes their
// dependent rows (ExecutionProperty, Association, Event,
// EventPath) so MLMD's foreign-key invariants stay intact.
//
// Ordering matters: child tables first, then parent. Wrapped in
// a transaction so a partial failure rolls back rather than
// leaving inconsistent foreign-key references.
//
// Note: SUCCEEDED Executions (state=3) are NOT pruned. Those
// represent successful research history users + the operator
// reference for keep/revert + leaderboards.
func (r *AutoResearchProjectReconciler) pruneOldFailedExecutions(
	ctx context.Context,
	db *sql.DB,
	namespace string,
) (int64, error) {
	var totalPruned int64
	for _, typeName := range mlmdPrunableTypes {
		// Find IDs to delete: all FAILED Executions of this type
		// except the most recent mlmdFailedRetention. Ordering
		// by create_time_since_epoch DESC + OFFSET keeps the
		// newest N and returns the rest.
		rows, err := db.QueryContext(ctx, `
			SELECT e.id
			  FROM Execution e
			  JOIN Type t ON e.type_id = t.id
			 WHERE t.name = ?
			   AND e.last_known_state = ?
			 ORDER BY e.create_time_since_epoch DESC
			 LIMIT 18446744073709551615 OFFSET ?
		`, typeName, mlmdStateFailed, mlmdFailedRetention)
		if err != nil {
			return totalPruned, fmt.Errorf("select prune candidates (%s): %w", typeName, err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return totalPruned, fmt.Errorf("scan prune id: %w", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) == 0 {
			continue
		}

		// Cascade-delete in dependent order, wrapped in a
		// transaction. MLMD doesn't define DB-level cascades, so
		// we walk the foreign-key relationships ourselves.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return totalPruned, fmt.Errorf("begin tx (%s): %w", typeName, err)
		}
		placeholders := strings.Repeat("?,", len(ids))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, len(ids))
		for _, id := range ids {
			args = append(args, id)
		}

		// Dependent tables, in safe-to-delete order:
		//   EventPath → Event → Association → ExecutionProperty → Execution
		deletes := []struct {
			label string
			query string
		}{
			{"EventPath", fmt.Sprintf(
				`DELETE FROM EventPath WHERE event_id IN (
				   SELECT id FROM Event WHERE execution_id IN (%s)
				 )`, placeholders)},
			{"Event", fmt.Sprintf(
				`DELETE FROM Event WHERE execution_id IN (%s)`, placeholders)},
			{"Association", fmt.Sprintf(
				`DELETE FROM Association WHERE execution_id IN (%s)`, placeholders)},
			{"ExecutionProperty", fmt.Sprintf(
				`DELETE FROM ExecutionProperty WHERE execution_id IN (%s)`, placeholders)},
			{"Execution", fmt.Sprintf(
				`DELETE FROM Execution WHERE id IN (%s)`, placeholders)},
		}
		var typePruned int64
		for _, d := range deletes {
			res, derr := tx.ExecContext(ctx, d.query, args...)
			if derr != nil {
				_ = tx.Rollback()
				return totalPruned, fmt.Errorf("prune %s (%s): %w", d.label, typeName, derr)
			}
			if d.label == "Execution" {
				typePruned, _ = res.RowsAffected()
			}
		}
		if err := tx.Commit(); err != nil {
			return totalPruned, fmt.Errorf("commit prune tx (%s): %w", typeName, err)
		}
		totalPruned += typePruned
	}
	return totalPruned, nil
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
