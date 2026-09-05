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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// Declared shifts. An agent's recurring work (a newsdesk's hourly sweep,
// a two-hourly review pass) used to be armed by hand with `openclaw cron
// add` inside the gateway pod, which made it runtime state: a re-created
// gateway came back with an empty cron table and nobody noticed until
// the site went quiet (upstreambeat, 2026-09-04). spec.schedules puts
// the shift in the AgentWorkstation, in git; this file converges it into
// the gateway's cron table on every reconcile.
//
// Matching is by job NAME within the workstation's agent id: the desks'
// hand-armed jobs carry the same names, so declaring them adopts the
// running jobs in place (no churn when nothing differs). Drift in the
// cron expression or message is corrected with `cron edit`; a name that
// left the spec but is still listed in status.schedules is removed —
// jobs the operator never declared are never touched.

// cronJob is the slice of `openclaw cron list --json` this file reads.
type cronJob struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	AgentID  string `json:"agentId"`
	Schedule struct {
		Kind string `json:"kind"`
		Expr string `json:"expr"`
	} `json:"schedule"`
	Payload struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"payload"`
}

type cronList struct {
	Jobs []cronJob `json:"jobs"`
}

// parseCronList tolerates the CLI's banner lines around the JSON.
func parseCronList(out string) ([]cronJob, error) {
	start := strings.Index(out, "{")
	if start < 0 {
		return nil, fmt.Errorf("no JSON in cron list output")
	}
	var l cronList
	if err := json.Unmarshal([]byte(out[start:]), &l); err != nil {
		return nil, fmt.Errorf("parsing cron list: %w", err)
	}
	return l.Jobs, nil
}

// scheduleOp is one command the converge step decided to run.
type scheduleOp struct {
	Kind string   // add | edit | rm
	Name string   // job name (for logs/status)
	Args []string // full openclaw argv
}

// planSchedules diffs the declared schedules against the gateway's jobs
// for this agent and returns the commands that make them equal.
// previouslyDeclared are the names this workstation declared last time
// (status.schedules): those, and only those, may be removed.
func planSchedules(agent string, declared []agentofficev1alpha1.AgentSchedule, previouslyDeclared []string, jobs []cronJob) []scheduleOp {
	byName := map[string]cronJob{}
	for _, j := range jobs {
		if j.AgentID == agent || j.AgentID == "" {
			byName[j.Name] = j
		}
	}
	var ops []scheduleOp
	want := map[string]bool{}
	for _, s := range declared {
		want[s.Name] = true
		j, exists := byName[s.Name]
		switch {
		case !exists:
			args := []string{"openclaw", "cron", "add", s.Name, s.Message, "--cron", s.Cron, "--agent", agent, "--best-effort-deliver", "--display-name", s.Name}
			if s.Description != "" {
				args = append(args, "--description", s.Description)
			}
			ops = append(ops, scheduleOp{Kind: "add", Name: s.Name, Args: args})
		case j.Schedule.Expr != s.Cron || j.Payload.Message != s.Message || (j.AgentID != "" && j.AgentID != agent):
			ops = append(ops, scheduleOp{Kind: "edit", Name: s.Name, Args: []string{"openclaw", "cron", "edit", j.ID, "--cron", s.Cron, "--message", s.Message, "--agent", agent, "--best-effort-deliver"}})
		}
	}
	// Removals: names we declared before and no longer do.
	prev := append([]string(nil), previouslyDeclared...)
	sort.Strings(prev)
	for _, name := range prev {
		if want[name] {
			continue
		}
		if j, exists := byName[name]; exists {
			ops = append(ops, scheduleOp{Kind: "rm", Name: name, Args: []string{"openclaw", "cron", "rm", j.ID}})
		}
	}
	return ops
}

// declaredNames is the sorted list recorded in status.
func declaredNames(declared []agentofficev1alpha1.AgentSchedule) []string {
	out := make([]string, 0, len(declared))
	for _, s := range declared {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// reconcileSchedules converges spec.schedules into the gateway pod's
// cron table. With nothing declared and nothing previously declared it
// is a no-op (no exec at all).
func (r *AgentWorkstationReconciler) reconcileSchedules(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation, gwPod *corev1.Pod) error {
	const condType = "SchedulesConverged"
	log := logf.FromContext(ctx)
	if len(aw.Spec.Schedules) == 0 && len(aw.Status.Schedules) == 0 {
		meta.RemoveStatusCondition(&aw.Status.Conditions, condType)
		return nil
	}
	if gwPod == nil {
		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{Type: condType, Status: metav1.ConditionFalse,
			Reason: "GatewayNotReady", Message: "waiting for a Ready gateway pod to converge the cron table"})
		return nil
	}
	out, err := r.execInPod(ctx, gwPod, []string{"openclaw", "cron", "list", "--json"})
	if err != nil {
		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{Type: condType, Status: metav1.ConditionFalse,
			Reason: "ListFailed", Message: truncate(err.Error(), 200)})
		return fmt.Errorf("cron list: %w", err)
	}
	jobs, err := parseCronList(out)
	if err != nil {
		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{Type: condType, Status: metav1.ConditionFalse,
			Reason: "ListUnparseable", Message: truncate(err.Error(), 200)})
		return err
	}
	ops := planSchedules(aw.Name, aw.Spec.Schedules, aw.Status.Schedules, jobs)
	var failed []string
	for _, op := range ops {
		if _, err := r.execInPod(ctx, gwPod, op.Args); err != nil {
			failed = append(failed, fmt.Sprintf("%s %s: %s", op.Kind, op.Name, truncate(err.Error(), 120)))
			continue
		}
		log.Info("schedule converged", "aw", aw.Name, "op", op.Kind, "job", op.Name)
	}
	aw.Status.Schedules = declaredNames(aw.Spec.Schedules)
	if len(failed) > 0 {
		meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{Type: condType, Status: metav1.ConditionFalse,
			Reason: "ApplyFailed", Message: truncate(strings.Join(failed, "; "), 400)})
		return fmt.Errorf("%d schedule op(s) failed", len(failed))
	}
	msg := fmt.Sprintf("%d schedule(s) declared", len(aw.Spec.Schedules))
	if len(ops) > 0 {
		msg += fmt.Sprintf(", %d change(s) applied", len(ops))
	}
	meta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{Type: condType, Status: metav1.ConditionTrue, Reason: "Converged", Message: msg})
	return nil
}
