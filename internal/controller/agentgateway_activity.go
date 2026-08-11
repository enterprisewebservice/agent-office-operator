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
// Signal: each logical agent keeps its session state in
// ~/.openclaw/agents/<id>/agent/*.sqlite inside the gateway pod, and
// OpenClaw writes that database as it processes a turn. The newest
// mtime under an agent's directory is therefore "when this agent last
// did work". It is a proxy — a long-running turn looks like activity at
// its start, not its end — but it is honest, needs no OpenClaw API, and
// costs ONE exec per gateway rather than one per agent.
//
// Only ever moves forward: a lower timestamp than what status already
// holds is ignored, so a restarted pod with a fresh PVC copy cannot
// rewrite history backwards.
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

	// One shell, all agents: "<agent-id> <unix-seconds>" per line.
	script := `for d in /home/node/.openclaw/agents/*/; do
  [ -d "$d" ] || continue
  n=$(basename "$d")
  t=$(find "$d" -name '*.sqlite*' -printf '%T@\n' 2>/dev/null | sort -rn | head -1)
  [ -n "$t" ] && echo "$n ${t%%.*}"
done`
	out, err := r.execInGatewayPod(ctx, pod, []string{"sh", "-c", script})
	if err != nil {
		return fmt.Errorf("collect agent activity: %w", err)
	}

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
	if len(seen) == 0 {
		return nil
	}

	var aws agentofficev1alpha1.AgentWorkstationList
	if err := r.List(ctx, &aws, client.InNamespace(gw.Namespace)); err != nil {
		return fmt.Errorf("listing agent workstations: %w", err)
	}
	updated := 0
	for i := range aws.Items {
		aw := &aws.Items[i]
		// Only agents bound to THIS gateway — another gateway's pod
		// holds its own agents' state.
		if aw.Spec.Runtime.Shared == nil || aw.Spec.Runtime.Shared.GatewayRef != gw.Name {
			continue
		}
		at, ok := seen[aw.Name]
		if !ok {
			continue
		}
		if aw.Status.LastActivity != nil && !at.After(aw.Status.LastActivity.Time) {
			continue // never move backwards
		}
		patched := aw.DeepCopy()
		t := metav1.NewTime(at)
		patched.Status.LastActivity = &t
		if err := r.Status().Patch(ctx, patched, client.MergeFrom(aw)); err != nil {
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
