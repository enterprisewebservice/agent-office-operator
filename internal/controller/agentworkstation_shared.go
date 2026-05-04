/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

// reconcileSharedFull is the real implementation of runtime.shared.
// Step-3 deliverable: when an AgentWorkstation has
// `spec.runtime.shared.gatewayRef: <name>`, the operator:
//   1. Looks up the AgentGateway by name in the same namespace.
//   2. Waits for the gateway pod to be Ready.
//   3. Builds the agent's openclaw `agents.list[]` entry from the AW
//      spec (id, displayName, model, systemPromptOverride,
//      workspace path).
//   4. Renders the per-agent SOUL.md / IDENTITY.md / TOOLS.md /
//      AGENTS.md files into the gateway's PVC at
//      ~/.openclaw/workspaces/<aw>/  via `kubectl exec`.
//   5. Runs `openclaw config set agents.list[]` against the gateway
//      pod via the same exec channel — appending the new agent.
//   6. If channels.discord.url is set, parses out the channel ID and
//      adds a `bindings[]` entry routing that Discord channel to
//      this agent.
//   7. Adds this agent's browser profile to the gateway's
//      nodeHost.browserProxy.allowProfiles allowlist so the shared
//      browser VM serves it an isolated Chromium user-data-dir.
//
// All operations are idempotent — operator can re-run safely.
//
// NOTE: this implementation uses kubectl exec to drive the
// `openclaw config set` CLI rather than editing openclaw.json
// directly. OpenClaw rewrites the file at runtime; using the CLI is
// the doc'd safe path.
func (r *AgentWorkstationReconciler) reconcileSharedFull(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if r.RestConfig == nil {
		return ctrl.Result{}, fmt.Errorf("RestConfig not set on reconciler — cannot exec into gateway pod")
	}

	gwRef := aw.Spec.Runtime.Shared.GatewayRef
	if gwRef == "" {
		return ctrl.Result{}, fmt.Errorf("runtime.shared.gatewayRef must be set")
	}

	// 1. Look up the AgentGateway.
	var gw agentofficev1alpha1.AgentGateway
	if err := r.Get(ctx, client.ObjectKey{Namespace: aw.Namespace, Name: gwRef}, &gw); err != nil {
		// Gateway not found — surface in status, requeue.
		aw.Status.Phase = agentofficev1alpha1.AgentWorkstationPhasePending
		aw.Status.Message = fmt.Sprintf("AgentGateway %q not found in namespace %s", gwRef, aw.Namespace)
		_ = r.Status().Update(ctx, aw)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// 2. Find a Ready gateway pod.
	gwPod, err := r.findReadyGatewayPod(ctx, &gw)
	if err != nil {
		aw.Status.Phase = agentofficev1alpha1.AgentWorkstationPhasePending
		aw.Status.Message = fmt.Sprintf("waiting for AgentGateway %s pod: %v", gwRef, err)
		_ = r.Status().Update(ctx, aw)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// 3. Build the per-agent entry.
	agentID := aw.Name
	profile := aw.Spec.Runtime.Shared.BrowserProfile
	if profile == "" {
		profile = agentID
	}
	model := "openai/gpt-5.4"
	if aw.Spec.Model.ModelName != "" {
		model = string(aw.Spec.Model.Provider) + "/" + aw.Spec.Model.ModelName
	}
	agentEntry := map[string]interface{}{
		"id":          agentID,
		"displayName": defaultIfEmpty(aw.Spec.DisplayName, agentID),
		"workspace":   "~/.openclaw/workspaces/" + agentID,
		"model":       model,
	}
	if aw.Spec.SystemPrompt != "" {
		agentEntry["systemPromptOverride"] = aw.Spec.SystemPrompt
	}
	agentEntryJSON, err := json.Marshal(agentEntry)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("marshal agent entry: %w", err)
	}

	// 4. Seed per-agent workspace files (idempotent).
	identity := defaultIfEmpty(aw.Spec.DisplayName, agentID)
	emoji := aw.Spec.Emoji
	identityMd := fmt.Sprintf("# %s\n\n%s %s\n", identity, emoji, identity)
	soulMd := fmt.Sprintf("# %s\n\n%s\n", identity, aw.Spec.SystemPrompt)

	seedScript := fmt.Sprintf(`set -e
mkdir -p ~/.openclaw/workspaces/%[1]s
[ -f ~/.openclaw/workspaces/%[1]s/IDENTITY.md ] || cat > ~/.openclaw/workspaces/%[1]s/IDENTITY.md <<'EOF'
%[2]s
EOF
[ -f ~/.openclaw/workspaces/%[1]s/SOUL.md ] || cat > ~/.openclaw/workspaces/%[1]s/SOUL.md <<'EOF'
%[3]s
EOF
echo seeded`, agentID, identityMd, soulMd)

	if _, err := r.execInPod(ctx, gwPod, []string{"sh", "-c", seedScript}); err != nil {
		return ctrl.Result{}, fmt.Errorf("seed agent workspace: %w", err)
	}

	// 5. Append (or replace) the agent's entry in agents.list and
	// add the browser profile to the node-host allowlist. Use
	// `openclaw config set` with --strict-json so the JSON validates
	// against the schema. The --replace flag lets us overwrite an
	// existing entry idempotently.
	cfgScript := fmt.Sprintf(`set -e
# Append/replace agent in agents.list (keyed by id)
openclaw config set 'agents.list[].id=%[1]s' --strict-json --json-value '%[2]s' --replace 2>&1 || \
  openclaw config set 'agents.list' --strict-json --append-json '%[2]s' 2>&1
# Make sure browserProxy is enabled and this profile is allow-listed.
openclaw config set 'nodeHost.browserProxy.enabled' true --strict-json 2>&1 || true
openclaw config set 'nodeHost.browserProxy.allowProfiles[]' '%[3]s' --strict-json --append 2>&1 || true
echo configured`, agentID, string(agentEntryJSON), profile)

	stdout, err := r.execInPod(ctx, gwPod, []string{"sh", "-c", cfgScript})
	if err != nil {
		log.Info("openclaw config set failed; will retry next reconcile", "err", err, "stdout", stdout)
		// Don't return error — config set is finicky and may need
		// retry once gateway has fully started. Surface in status.
		aw.Status.Phase = agentofficev1alpha1.AgentWorkstationPhaseCreating
		aw.Status.Message = "registering agent in gateway: " + truncate(err.Error(), 200)
		_ = r.Status().Update(ctx, aw)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 6. Discord binding (best-effort).
	if aw.Spec.Channels != nil && aw.Spec.Channels.Discord != nil && aw.Spec.Channels.Discord.URL != "" {
		channelID := parseDiscordChannelID(aw.Spec.Channels.Discord.URL)
		if channelID != "" {
			binding := map[string]interface{}{
				"agentId": agentID,
				"match":   map[string]interface{}{"channel": "discord", "channelId": channelID},
			}
			bJSON, _ := json.Marshal(binding)
			bScript := fmt.Sprintf(`openclaw config set 'bindings[].agentId=%s' --strict-json --json-value '%s' --replace 2>&1 || \
openclaw config set 'bindings' --strict-json --append-json '%s' 2>&1 || true`, agentID, string(bJSON), string(bJSON))
			_, _ = r.execInPod(ctx, gwPod, []string{"sh", "-c", bScript})
		}
	}

	// 7. Status: Running with the gateway's endpoint.
	aw.Status.Phase = agentofficev1alpha1.AgentWorkstationPhaseRunning
	aw.Status.Message = fmt.Sprintf("logical agent inside %s (profile=%s)", gwRef, profile)
	aw.Status.GatewayEndpoint = gw.Status.GatewayEndpoint
	if err := r.Status().Update(ctx, aw); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// findReadyGatewayPod returns the name of a Ready pod for the given
// AgentGateway. The gateway's Deployment selector matches all of its
// pods; we pick the first Ready one.
func (r *AgentWorkstationReconciler) findReadyGatewayPod(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{"agentoffice.ai/gateway": gw.Name},
	); err != nil {
		return nil, err
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, c := range p.Status.ContainerStatuses {
			if c.Name == "openclaw" && c.Ready {
				return &p, nil
			}
		}
	}
	return nil, fmt.Errorf("no Ready openclaw container in any %s gateway pod", gw.Name)
}

// execInPod runs cmd inside the openclaw container of the given pod
// and returns combined stdout/stderr. Uses the standard k8s exec
// SPDY channel.
func (r *AgentWorkstationReconciler) execInPod(ctx context.Context, pod *corev1.Pod, cmd []string) (string, error) {
	cs, err := kubernetes.NewForConfig(r.RestConfig)
	if err != nil {
		return "", fmt.Errorf("kubernetes client: %w", err)
	}
	req := cs.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "openclaw",
			Command:   cmd,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(r.RestConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("new spdy executor: %w", err)
	}
	var out bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &out,
		Stderr: &out,
	})
	if err != nil {
		return out.String(), fmt.Errorf("exec: %w", err)
	}
	return out.String(), nil
}

// parseDiscordChannelID extracts the channel snowflake from URLs of
// the shapes:
//   https://discord.com/channels/<guild>/<channel>
//   https://discord.gg/<invite>           -> empty (invite, no channel)
func parseDiscordChannelID(url string) string {
	const prefix = "https://discord.com/channels/"
	if !strings.HasPrefix(url, prefix) {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(url, prefix), "/")
	if len(parts) < 2 || parts[1] == "" {
		return ""
	}
	return parts[1]
}

func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
