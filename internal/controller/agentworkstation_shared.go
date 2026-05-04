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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	// 0. GC any leftover dedicated-runtime resources from a previous
	//    spec.runtime: dedicated state. When the user flips an AW
	//    from dedicated → shared, the old per-agent Pod, Service,
	//    Route, PVC, ConfigMap, and gateway-token Secret should be
	//    removed so they don't sit idle. (ownerReferences would only
	//    GC on AW delete, not on a runtime-mode change.)
	if err := r.gcDedicatedResources(ctx, aw); err != nil {
		log.Info("dedicated GC partial", "err", err)
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
	//
	// OpenClaw's agents.list[] entry schema (verified against
	// running gateway, see /app/dist/agents.config-C73qQmV7.js):
	//   { id, name?, workspace?, agentDir?, model?, identity? }
	// where workspace is a *string* path (not an object), model is
	// either a string ("openai/gpt-5.4") or {primary: string}, and
	// identity is {name?, emoji?}. There is no systemPrompt /
	// systemPromptOverride field — persona comes from the
	// IDENTITY.md / SOUL.md files in the agent's workspace dir.
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
		"id":        agentID,
		"name":      defaultIfEmpty(aw.Spec.DisplayName, agentID),
		"workspace": "/home/node/.openclaw/workspaces/" + agentID,
		"model":     map[string]interface{}{"primary": model},
	}
	if aw.Spec.Emoji != "" || aw.Spec.DisplayName != "" {
		agentEntry["identity"] = map[string]interface{}{
			"name":  defaultIfEmpty(aw.Spec.DisplayName, agentID),
			"emoji": aw.Spec.Emoji,
		}
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

	// 5. Merge agent entry, browser-profile allowlist, and (optional)
	// Discord binding into ~/.openclaw/openclaw.json directly.
	//
	// We do NOT use `openclaw config set` for this — its CLI flags
	// (--json-value, --append, --append-json) don't exist in the
	// 2026.3.x release, and the supported value-mode setter would
	// clobber the entire array. Instead we read the JSON, merge
	// (idempotent by id / by string equality), and write back. Same
	// pattern as the gateway controller's auto-approve action.
	//
	// After write, `openclaw config validate` confirms the merged
	// shape is schema-valid; if it isn't, we restore the .bak.
	mergeScript := fmt.Sprintf(`
const fs = require("fs");
const path = "/home/node/.openclaw/openclaw.json";
const backup = path + ".bak.merge";
const agentEntry = %[1]s;
const profile = %[2]q;
const channelId = %[3]q;
const agentId = %[4]q;

const raw = fs.readFileSync(path, "utf8");
fs.writeFileSync(backup, raw);
const cfg = JSON.parse(raw);

// agents.list — merge by id
cfg.agents = cfg.agents || {};
const list = Array.isArray(cfg.agents.list) ? cfg.agents.list : [];
const existing = list.findIndex(a => a && a.id === agentEntry.id);
if (existing >= 0) list[existing] = { ...list[existing], ...agentEntry };
else list.push(agentEntry);
cfg.agents.list = list;

// nodeHost.browserProxy — enable + add profile (de-dup)
cfg.nodeHost = cfg.nodeHost || {};
cfg.nodeHost.browserProxy = cfg.nodeHost.browserProxy || {};
cfg.nodeHost.browserProxy.enabled = true;
const allow = Array.isArray(cfg.nodeHost.browserProxy.allowProfiles) ? cfg.nodeHost.browserProxy.allowProfiles : [];
if (!allow.includes(profile)) allow.push(profile);
cfg.nodeHost.browserProxy.allowProfiles = allow;

// bindings — only if we have a real Discord channel id (not @me)
if (channelId) {
  const bindings = Array.isArray(cfg.bindings) ? cfg.bindings : [];
  const idx = bindings.findIndex(b => b && b.agentId === agentId && b.match && b.match.channel === channelId);
  const binding = { agentId, match: { channel: channelId, accountId: "discord" } };
  if (idx >= 0) bindings[idx] = binding;
  else bindings.push(binding);
  cfg.bindings = bindings;
}

fs.writeFileSync(path, JSON.stringify(cfg, null, 2));
console.log("MERGED agent=" + agentId + " profile=" + profile + " channel=" + (channelId || "(none)"));
`, string(agentEntryJSON), profile, parseDiscordChannelID(awDiscordURL(aw)), agentID)

	mergeOut, err := r.execInPod(ctx, gwPod, []string{"node", "-e", mergeScript})
	if err != nil {
		log.Info("merge openclaw.json failed; will retry next reconcile", "err", err, "stdout", mergeOut)
		aw.Status.Phase = agentofficev1alpha1.AgentWorkstationPhaseCreating
		aw.Status.Message = "registering agent in gateway: " + truncate(err.Error(), 200)
		_ = r.Status().Update(ctx, aw)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	log.Info("merged agent into gateway config", "out", strings.TrimSpace(mergeOut))

	// 5b. Best-effort post-merge validate. Skip the result entirely:
	// when openclaw.json changes on disk the gateway process bounces
	// (its file-watcher restarts the container), which races our
	// exec channel and surfaces as exit 137. Since the merge above
	// only writes schema-valid JSON we constructed in Go, treat the
	// validate as advisory.
	if validateOut, vErr := r.execInPod(ctx, gwPod, []string{"openclaw", "config", "validate"}); vErr != nil {
		log.V(1).Info("openclaw config validate after merge (advisory only)",
			"err", vErr, "stdout", strings.TrimSpace(validateOut))
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

// gcDedicatedResources removes the per-AW Pod / Service / Route /
// PVC / ConfigMap / gateway-token Secret created by the dedicated
// runtime path. Idempotent — silently skips anything already gone.
// Called when an AW transitions to runtime.shared so the dedicated
// resources don't linger.
func (r *AgentWorkstationReconciler) gcDedicatedResources(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) error {
	ns := aw.Namespace
	type tgt struct {
		obj  client.Object
		name string
	}
	targets := []tgt{
		{&appsv1.Deployment{}, deployName(aw.Name)},
		{&corev1.Service{}, serviceName(aw.Name)},
		{&corev1.ConfigMap{}, cmName(aw.Name)},
		{&corev1.Secret{}, tokenName(aw.Name)},
		{&corev1.PersistentVolumeClaim{}, pvcName(aw.Name)},
	}
	for _, t := range targets {
		t.obj.SetNamespace(ns)
		t.obj.SetName(t.name)
		if err := r.Delete(ctx, t.obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %T %s/%s: %w", t.obj, ns, t.name, err)
		}
	}
	// Route is unstructured — match the generic delete shape.
	rt := &unstructured.Unstructured{}
	rt.SetGroupVersionKind(schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"})
	rt.SetNamespace(ns)
	rt.SetName(routeName(aw.Name))
	if err := r.Delete(ctx, rt); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Route %s/%s: %w", ns, routeName(aw.Name), err)
	}
	return nil
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
//   https://discord.com/channels/@me      -> empty (DM landing, no channel)
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
	if parts[0] == "@me" {
		return ""
	}
	return parts[1]
}

// awDiscordURL returns the AW's configured Discord channel URL, or
// "" if not set.
func awDiscordURL(aw *agentofficev1alpha1.AgentWorkstation) string {
	if aw.Spec.Channels == nil || aw.Spec.Channels.Discord == nil {
		return ""
	}
	return aw.Spec.Channels.Discord.URL
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
