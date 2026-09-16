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
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// recordAgentActivity fills in AgentWorkstation.status.lastActivity for
// every agent on this gateway.
//
// The field has existed since the CRD was written and nothing ever set
// it, so the console could only show what an agent IS (phase, model,
// tools) and never whether it had done anything. That is the difference
// between an inventory and an operations view.
//
// Signal: OpenClaw appends an agent's session transcript
// (~/.openclaw/agents/<id>/sessions/*.jsonl, plus its trajectory) while
// a turn runs, so the newest transcript mtime is "when this agent last
// did work". It is a proxy — a long-running turn looks like activity
// until its last write — but it is honest, needs no OpenClaw API, and
// costs ONE exec per gateway rather than one per agent.
//
// Until v1.7.77 the signal was any *.sqlite* file. That was not work:
// SQLite creates the -shm/-wal files when a connection opens and removes
// them when it closes, and the main database is written at gateway start
// without any turn. Every config reload therefore read as activity for
// every agent, and the status patch below re-ran the agent reconcile
// that had caused the reload — one half of the loop that rewrote
// openclaw.json every few seconds.
//
// Only ever moves forward once the transcript signal owns the field
// (activitySignalCondition): a lower timestamp is ignored, so a
// restarted pod with a fresh PVC copy cannot rewrite history backwards.
// The first pass under the transcript signal replaces whatever the old
// signal left, including clearing it for an agent with no transcript.
func (r *AgentGatewayReconciler) recordAgentActivity(
	ctx context.Context, gw *agentofficev1alpha1.AgentGateway,
) error {
	if r.RestConfig == nil {
		return fmt.Errorf("RestConfig not set; cannot exec")
	}
	pod, err := r.findReadyGatewayPod(ctx, gw)
	if err != nil {
		return err
	}

	out, err := r.execInGatewayPod(ctx, pod, []string{"sh", "-c", agentActivityScript})
	if err != nil {
		return fmt.Errorf("collect agent activity: %w", err)
	}
	seen := parseAgentActivity(out)

	var aws agentofficev1alpha1.AgentWorkstationList
	if err := r.List(ctx, &aws, client.InNamespace(gw.Namespace)); err != nil {
		return fmt.Errorf("listing agent workstations: %w", err)
	}
	updated := 0
	for i := range aws.Items {
		aw := &aws.Items[i]
		// Only agents bound to THIS gateway — another gateway's pod
		// holds its own agents' state.
		if aw.Spec.Runtime == nil || aw.Spec.Runtime.Shared == nil || aw.Spec.Runtime.Shared.GatewayRef != gw.Name {
			continue
		}
		at, ok := seen[aw.Name]
		patched, patch, changed := planActivityPatch(aw, at, ok)
		if !changed {
			continue
		}
		if err := r.Status().Patch(ctx, patched, patch); err != nil {
			logf.FromContext(ctx).V(1).Info("activity patch failed",
				"agent", aw.Name, "err", err.Error())
			continue
		}
		updated++
	}
	logf.FromContext(ctx).V(1).Info("agent activity recorded",
		"gateway", gw.Name, "seen", len(seen), "updated", updated)
	return nil
}

// agentActivityScript prints "<agent-id> <unix-seconds>" for every agent
// that has a session transcript, using the newest one.
const agentActivityScript = `for d in /home/node/.openclaw/agents/*/; do
  [ -d "$d" ] || continue
  n=$(basename "$d")
  t=$(find "$d" -path '*/sessions/*.jsonl' -printf '%T@\n' 2>/dev/null | sort -rn | head -1)
  [ -n "$t" ] && echo "$n ${t%%.*}"
done`

// activitySignalCondition marks an AgentWorkstation whose
// status.lastActivity comes from session transcripts (v1.7.77+).
const (
	activitySignalCondition = "ActivitySignal"
	activitySignalReason    = "SessionTranscripts"
)

func parseAgentActivity(out string) map[string]time.Time {
	seen := map[string]time.Time{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) != 2 {
			continue
		}
		secs, convErr := strconv.ParseInt(f[1], 10, 64)
		if convErr != nil || secs <= 0 {
			continue
		}
		seen[f[0]] = time.Unix(secs, 0).UTC()
	}
	return seen
}

// planActivityPatch decides the status patch for one agent. at/seen is
// its newest transcript write. Once the transcript signal is recorded on
// the agent, lastActivity only moves forward, via a patch that carries
// nothing else. Before that, the value is replaced (or cleared when
// there is no transcript) together with the marker condition; that patch
// rewrites the conditions list, so it is guarded by resourceVersion and
// a concurrent status write simply defers it to the next pass.
func planActivityPatch(aw *agentofficev1alpha1.AgentWorkstation, at time.Time, seen bool) (*agentofficev1alpha1.AgentWorkstation, client.Patch, bool) {
	marked := false
	if c := meta.FindStatusCondition(aw.Status.Conditions, activitySignalCondition); c != nil &&
		c.Status == metav1.ConditionTrue && c.Reason == activitySignalReason {
		marked = true
	}
	patched := aw.DeepCopy()
	if marked {
		if !seen || (aw.Status.LastActivity != nil && !at.After(aw.Status.LastActivity.Time)) {
			return nil, nil, false // never move backwards
		}
		t := metav1.NewTime(at)
		patched.Status.LastActivity = &t
		return patched, client.MergeFrom(aw), true
	}
	patched.Status.LastActivity = nil
	if seen {
		t := metav1.NewTime(at)
		patched.Status.LastActivity = &t
	}
	meta.SetStatusCondition(&patched.Status.Conditions, metav1.Condition{
		Type:               activitySignalCondition,
		Status:             metav1.ConditionTrue,
		Reason:             activitySignalReason,
		Message:            "lastActivity is the newest session transcript write in the gateway",
		ObservedGeneration: aw.Generation,
	})
	return patched, client.MergeFromWithOptions(aw, client.MergeFromWithOptimisticLock{}), true
}
