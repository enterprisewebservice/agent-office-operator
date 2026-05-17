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

// MLMD (kfp v2's ML Metadata DB, stored in the DSP MariaDB) keeps
// an Execution record per pipeline run + per step. When a run dies
// mid-flight (Konflux build failure, crashed trainer, deleted run
// record via the runs API), kfp's launcher doesn't always clean
// up its Executions — they stay stuck `last_known_state = 2`
// (RUNNING) in the DB. The OpenShift AI dashboard's Pipelines/
// Executions view then shows them as still Running indefinitely.
//
// This file owns the periodic janitor that transitions those
// orphans to FAILED so the UI matches reality.

const (
	// MLMD state codes (from the ml-metadata schema).
	mlmdStateUnknown  = 0
	mlmdStateNew      = 1
	mlmdStateRunning  = 2
	mlmdStateComplete = 3
	mlmdStateFailed   = 4
	mlmdStateCached   = 5
	mlmdStateCanceled = 6

	// Skip Executions whose last_update_time_since_epoch is
	// within this window — we don't want to mark an actively-
	// running training pod's Execution as Failed. 20 min covers
	// the longest plausible kfp gap between updates while still
	// catching truly-dead orphans quickly.
	mlmdStaleAfter = 20 * time.Minute

	// Cooldown between cleanup passes per namespace. The cleanup
	// itself is cheap (single UPDATE), but we don't need to run
	// it more often than once per autoresearch cycle.
	mlmdCleanupCooldown = 10 * time.Minute
)

// mlmdCleanupState tracks when we last ran the janitor per
// namespace, so multiple AutoResearchProjects in the same ns
// don't all hit the DB simultaneously.
type mlmdCleanupTracker struct {
	mu       sync.Mutex
	lastRun  map[string]time.Time
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

// cleanupStaleMLMDExecutions transitions orphaned RUNNING
// Executions in the project's DSP MariaDB to FAILED. Returns the
// row count touched. Safe to call repeatedly — the cooldown above
// prevents DB hammering, and the staleness window protects active
// runs from being marked failed prematurely.
//
// Why we connect to MariaDB directly rather than via the MLMD
// gRPC API: the gRPC API requires its own client + schema deps
// (ml-metadata is a large package), whereas an UPDATE statement
// is six SQL lines via the standard database/sql driver. We
// already have full read access to the kfp DB's password Secret
// in the project's namespace (the same one DSP itself uses).
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

	// Short timeout: this should be < 1 sec on a healthy DB. If
	// it hangs, the reconcile shouldn't wait long for it.
	dbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cutoffMillis := time.Now().Add(-mlmdStaleAfter).UnixMilli()
	result, err := db.ExecContext(dbCtx,
		`UPDATE Execution
		 SET last_known_state = ?
		 WHERE last_known_state = ?
		   AND last_update_time_since_epoch < ?`,
		mlmdStateFailed, mlmdStateRunning, cutoffMillis,
	)
	if err != nil {
		return 0, fmt.Errorf("UPDATE Execution: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows > 0 {
		log.Info("mlmd janitor: transitioned stale RUNNING executions to FAILED",
			"rows", rows, "ns", p.Namespace)
	}
	return rows, nil
}

// mlmdDSN reads the DSP MariaDB credentials from the
// ds-pipeline-db-dspa Secret (created by the DSP operator in
// every DSPA-enabled namespace) and returns a database/sql DSN
// pointing at the in-cluster mariadb-dspa Service.
//
// The DB name is `mlpipeline` — DSP uses one DB for both kfp's
// pipeline tables and the embedded MLMD tables (we verified the
// MLMD-schema tables Execution/Artifact/Context all live there).
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
	// In-cluster MariaDB Service. Plain TCP — TLS to MariaDB is
	// optional; the DSP setup uses cleartext on the cluster
	// internal network (same as the DSP API server's own
	// connection). If your DSPA is TLS-only, append ?tls=true.
	host := fmt.Sprintf("mariadb-dspa.%s.svc.cluster.local:3306", namespace)
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&timeout=5s",
		user, pass, host, dbname), nil
}
