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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// AgentGatewayReconciler owns the gateway runtime: Deployment, PVC,
// Service, Route, ConfigMap, token Secret. Multiple
// AgentWorkstations with `spec.runtime.shared.gatewayRef: <this>`
// slot in as logical openclaw agents inside this gateway's pod.
type AgentGatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// RestConfig powers the auto-approve action's exec into the
	// gateway pod — we drive `openclaw nodes approve` so the
	// running daemon's in-memory pairing store gets updated
	// alongside the on-disk paired.json (file-only edits leave the
	// daemon rejecting the node until pod restart).
	RestConfig *rest.Config
}

// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentgateways,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentgateways/finalizers,verbs=update

func (r *AgentGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var gw agentofficev1alpha1.AgentGateway
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1. Materialize gateway runtime (Pod/PVC/Service/Route/CM/Secret).
	if err := r.reconcileGatewayChildren(ctx, &gw); err != nil {
		log.Error(err, "reconcileGatewayChildren")
		return ctrl.Result{}, err
	}

	// 2. Auto-approve the configured node-host's pending pairing
	//    request (if any). One-shot per pairing — once approved, the
	//    paired entry persists in the gateway PVC.
	autoApprove := gw.Spec.AutoApproveNodeHost == nil || *gw.Spec.AutoApproveNodeHost
	if autoApprove && gw.Spec.NodeHostRef != nil && gw.Spec.NodeHostRef.Name != "" {
		if err := r.maybeAutoApprovePending(ctx, &gw); err != nil {
			log.Info("auto-approve skipped", "err", err)
			// non-fatal — the gateway pod may not be Ready yet, will retry
		}
	}

	// 3. Ensure the gateway's openclaw.json has models.providers
	//    declared. The init container is seed-only, so existing PVCs
	//    that pre-date the providers field need an in-place merge.
	//    Idempotent: if the provider already exists, this is a no-op.
	if err := r.maybeMergeModelsProviders(ctx, &gw); err != nil {
		log.Info("models.providers merge skipped", "err", err)
	}

	// 4. Pre-approve channel senders listed in spec.allowedUsers by
	//    merging them into <channel>-<accountId>-allowFrom.json.
	//    Skips the "OpenClaw: access not configured" prompt for those
	//    users on first contact. Idempotent.
	if err := r.maybeMergeAllowedUsers(ctx, &gw); err != nil {
		log.Info("allowedUsers merge skipped", "err", err)
	}

	// 5. Compute status from observed Deployment + Route + AW count.
	return r.reconcileGatewayStatus(ctx, &gw)
}

// maybeMergeAllowedUsers writes the configured spec.allowedUsers IDs
// into the matching <channel>-<accountId>-allowFrom.json on the
// gateway PVC. OpenClaw consults that file before prompting a sender
// to pair, so listed users skip the pairing handshake entirely.
// Idempotent — only writes when there are new entries to add.
func (r *AgentGatewayReconciler) maybeMergeAllowedUsers(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) error {
	if len(gw.Spec.AllowedUsers) == 0 {
		return nil
	}
	if r.RestConfig == nil {
		return fmt.Errorf("RestConfig not set; cannot exec")
	}
	pod, err := r.findReadyGatewayPod(ctx, gw)
	if err != nil {
		return err
	}

	// Group IDs by (channel, accountId) so we write at most one file
	// per group and avoid races against ourselves.
	groups := map[[2]string][]string{}
	for _, u := range gw.Spec.AllowedUsers {
		ch := u.Channel
		if ch == "" {
			ch = "discord"
		}
		acct := u.AccountID
		if acct == "" {
			acct = "default"
		}
		id := strings.TrimSpace(u.ID)
		if id == "" {
			continue
		}
		key := [2]string{ch, acct}
		groups[key] = append(groups[key], id)
	}

	for key, ids := range groups {
		ch, acct := key[0], key[1]
		idsJSON, _ := json.Marshal(ids)
		// Path layout matches /app/dist/pairing-store-*.js:
		//   resolveAllowFromPath(channel, env, accountId) →
		//     <oauthDir>/<channel>-<accountId>-allowFrom.json
		// (or <channel>-allowFrom.json when accountId is empty —
		// we always pass one so we hit the namespaced path).
		fileName := fmt.Sprintf("%s-%s-allowFrom.json", ch, acct)
		script := fmt.Sprintf(`
const fs = require("fs"); const path = require("path");
const dir = "/home/node/.openclaw/credentials";
const file = path.join(dir, %q);
const want = %s;
let store = { version: 1, allowFrom: [] };
try { store = JSON.parse(fs.readFileSync(file, "utf8")); } catch (e) {}
const list = Array.isArray(store.allowFrom) ? store.allowFrom : [];
const set = new Set(list.map(String));
let added = 0;
for (const id of want) {
  const s = String(id).trim();
  if (!s || set.has(s)) continue;
  list.push(s); set.add(s); added++;
}
if (added > 0) {
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(file, JSON.stringify({ version: 1, allowFrom: list }, null, 2));
  console.log("ALLOWED_USERS_MERGED file=" + file + " added=" + added);
} else {
  console.log("NO_CHANGE file=" + file);
}
`, fileName, string(idsJSON))

		out, err := r.execInGatewayPod(ctx, pod, []string{"node", "-e", script})
		if err != nil {
			return fmt.Errorf("merge allowedUsers (%s/%s): %w (out=%s)", ch, acct, err, out)
		}
		logf.FromContext(ctx).V(1).Info("allowedUsers merge",
			"channel", ch, "accountId", acct, "result", strings.TrimSpace(out))
	}
	return nil
}

// maybeMergeModelsProviders ensures models.providers.openai exists in
// the gateway's openclaw.json. Existing PVCs created before the
// providers feature don't have it; without it, agents fail with
// "No API key found for provider openai" even when OPENAI_API_KEY is
// in the pod env. Idempotent — only writes when the field is absent
// or shape-incomplete.
func (r *AgentGatewayReconciler) maybeMergeModelsProviders(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) error {
	if r.RestConfig == nil {
		return fmt.Errorf("RestConfig not set; cannot exec")
	}
	pod, err := r.findReadyGatewayPod(ctx, gw)
	if err != nil {
		return err
	}
	script := `
const fs = require("fs");
const p = "/home/node/.openclaw/openclaw.json";
const cfg = JSON.parse(fs.readFileSync(p, "utf8"));
let changed = false;
cfg.gateway = cfg.gateway || {};
cfg.gateway.auth = cfg.gateway.auth || {};
cfg.gateway.remote = cfg.gateway.remote || {};
cfg.models = cfg.models || {};
cfg.models.providers = cfg.models.providers || {};

// Sync gateway.auth.token / remote.token to the CURRENT env value
// of OPENCLAW_GATEWAY_TOKEN. The init container is seed-only and
// the gateway compares incoming WS auth against the literal in
// openclaw.json (env-var refs aren't expanded for this path), so
// when the Secret rotates the openclaw.json gets stranded with
// the old token and every connection fails with token_mismatch.
// Re-running this on every reconcile keeps openclaw.json in sync
// with the live env. Idempotent — only writes when out of sync.
const envTok = process.env.OPENCLAW_GATEWAY_TOKEN || "";
if (envTok && cfg.gateway.auth.token !== envTok) {
  cfg.gateway.auth.token = envTok;
  changed = true;
}
if (envTok && cfg.gateway.remote.token !== envTok) {
  cfg.gateway.remote.token = envTok;
  changed = true;
}

// Pay-per-request OpenAI provider (uses OPENAI_API_KEY env).
if (!cfg.models.providers.openai || !cfg.models.providers.openai.apiKey) {
  cfg.models.providers.openai = {
    baseUrl: "https://api.openai.com/v1",
    api: "openai-completions",
    apiKey: "${OPENAI_API_KEY}",
    models: [
      { id: "gpt-5.5", name: "GPT-5.5" },
      { id: "gpt-5.4", name: "gpt-5.4" },
      { id: "gpt-4o-mini", name: "gpt-4o-mini" },
      { id: "gpt-4o", name: "gpt-4o" }
    ]
  };
  changed = true;
}

// ChatGPT Pro / Codex provider (OAuth via ~/.codex/auth.json).
// pi-ai's stock model catalog is outdated and only goes up to
// gpt-5.3-codex; declare current models here so users can ask for
// gpt-5.4/5.5 without OpenClaw silently falling back to the
// rate-limited openai/gpt-X path.
const codexNeedsSeed = !cfg.models.providers["openai-codex"]
  || !Array.isArray(cfg.models.providers["openai-codex"].models)
  || cfg.models.providers["openai-codex"].models.length === 0;
if (codexNeedsSeed) {
  cfg.models.providers["openai-codex"] = {
    baseUrl: "https://chatgpt.com/backend-api",
    api: "openai-codex-responses",
    models: [
      { id: "gpt-5.5", name: "GPT-5.5", api: "openai-codex-responses" },
      { id: "gpt-5.4", name: "GPT-5.4", api: "openai-codex-responses" },
      { id: "gpt-5.3-codex", name: "GPT-5.3 Codex", api: "openai-codex-responses" }
    ]
  };
  changed = true;
}

if (changed) {
  fs.writeFileSync(p, JSON.stringify(cfg, null, 2));
  console.log("MERGED_MODELS_PROVIDERS");
} else {
  console.log("NO_CHANGE");
}
`
	out, err := r.execInGatewayPod(ctx, pod, []string{"node", "-e", script})
	if err != nil {
		return fmt.Errorf("merge models.providers: %w (out=%s)", err, out)
	}
	logf.FromContext(ctx).V(1).Info("models.providers merge", "result", strings.TrimSpace(out))
	return nil
}

// maybeAutoApprovePending approves any pending node-host pairing
// request whose displayName matches the configured nodeHostRef.
//
// Implementation note: this used to hand-edit
// ~/.openclaw/devices/paired.json — wrong on two counts. Node-host
// pairings live under ~/.openclaw/nodes/{pending,paired}.json (the
// devices/ tree stores operator-CLI pairings), and even when the
// path was right, file edits don't propagate to the gateway's
// in-memory pairing store, so the running daemon kept rejecting the
// node with "pairing required" until the pod restarted. Both
// failure modes meant `spec.autoApproveNodeHost: true` silently did
// nothing and every gateway-pod restart left the VM disconnected.
//
// We now drive the in-cluster `openclaw nodes` CLI, which both
// updates the in-memory store and persists nodes/paired.json — no
// pod bounce needed, and the next reconcile sees ALREADY_PAIRED.
// Idempotent.
func (r *AgentGatewayReconciler) maybeAutoApprovePending(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) error {
	if r.RestConfig == nil {
		return fmt.Errorf("RestConfig not set; cannot exec")
	}
	pod, err := r.findReadyGatewayPod(ctx, gw)
	if err != nil {
		return err
	}

	// 1. If a node-host whose displayName matches our nodeHostRef is
	//    already paired+connected, we're done.
	statusOut, err := r.execInGatewayPod(ctx, pod, []string{"openclaw", "nodes", "status", "--json"})
	if err == nil {
		// `openclaw nodes status --json` shape:
		//   [{ "displayName": "...", "status": "paired" | "pending" | ..., ...}]
		// We accept either "paired" or any string starting with "paired"
		// (e.g. "paired · connected") just in case the CLI tweaks it.
		var nodes []struct {
			DisplayName string `json:"displayName"`
			Status      string `json:"status"`
		}
		if jerr := json.Unmarshal([]byte(extractJSON(statusOut)), &nodes); jerr == nil {
			for _, n := range nodes {
				if strings.Contains(n.DisplayName, gw.Spec.NodeHostRef.Name) &&
					strings.HasPrefix(n.Status, "paired") {
					return nil // already paired — nothing to do
				}
			}
		}
	}

	// 2. List pending requests; find one whose displayName contains
	//    the configured nodeHostRef.Name. Pending entries we care
	//    about advertise node-host capabilities (browser/system).
	pendingOut, err := r.execInGatewayPod(ctx, pod, []string{"openclaw", "nodes", "pending", "--json"})
	if err != nil {
		return fmt.Errorf("list pending: %w (out=%s)", err, pendingOut)
	}
	var pending []struct {
		RequestID   string   `json:"requestId"`
		DisplayName string   `json:"displayName"`
		Caps        []string `json:"caps"`
	}
	if jerr := json.Unmarshal([]byte(extractJSON(pendingOut)), &pending); jerr != nil {
		// Empty / "No pending pairing requests." text — nothing to do.
		return nil
	}
	var reqID string
	for _, p := range pending {
		if !strings.Contains(p.DisplayName, gw.Spec.NodeHostRef.Name) {
			continue
		}
		hasNodeCap := false
		for _, c := range p.Caps {
			if c == "browser" || c == "system" {
				hasNodeCap = true
				break
			}
		}
		if !hasNodeCap {
			continue
		}
		reqID = p.RequestID
		break
	}
	if reqID == "" {
		return nil // no matching pending request
	}

	// 3. Approve via CLI — updates in-memory store + nodes/paired.json
	//    in one shot. No pod bounce needed.
	approveOut, err := r.execInGatewayPod(ctx, pod, []string{"openclaw", "nodes", "approve", reqID})
	if err != nil {
		return fmt.Errorf("approve %s: %w (out=%s)", reqID, err, approveOut)
	}
	logf.FromContext(ctx).Info("auto-approved node-host pairing",
		"nodeHost", gw.Spec.NodeHostRef.Name, "requestId", reqID,
		"result", strings.TrimSpace(approveOut))
	return nil
}

// extractJSON pulls the first JSON document out of a CLI response —
// `openclaw nodes ... --json` sometimes prefixes its output with
// human-readable preamble. Returns "[]" if no JSON found so callers
// can safely json.Unmarshal the result.
func extractJSON(s string) string {
	// Find the first '[' or '{' and return from there.
	for i, c := range s {
		if c == '[' || c == '{' {
			return s[i:]
		}
	}
	return "[]"
}

func (r *AgentGatewayReconciler) findReadyGatewayPod(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) (*corev1.Pod, error) {
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

func (r *AgentGatewayReconciler) execInGatewayPod(ctx context.Context, pod *corev1.Pod, cmd []string) (string, error) {
	cs, err := kubernetes.NewForConfig(r.RestConfig)
	if err != nil {
		return "", err
	}
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(pod.Namespace).Name(pod.Name).
		SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: "openclaw", Command: cmd, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(r.RestConfig, "POST", req.URL())
	if err != nil {
		return "", err
	}
	// Separate buffers for stdout/stderr — same data-race fix as
	// AutoResearchProjectReconciler.execInGatewayPod. See that
	// function's comment for the wire-level story.
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}


// reconcileGatewayStatus reads the owned Deployment + Route and
// counts how many AgentWorkstations reference this gateway. Updates
// status.phase / gatewayEndpoint / agentCount.
func (r *AgentGatewayReconciler) reconcileGatewayStatus(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var dep appsv1.Deployment
	depErr := r.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: gwDeployName(gw.Name)}, &dep)

	phase := agentofficev1alpha1.AgentGatewayPhasePending
	message := "no Deployment yet"
	switch {
	case depErr == nil && dep.Status.ReadyReplicas >= 1:
		phase = agentofficev1alpha1.AgentGatewayPhaseReady
		message = fmt.Sprintf("Deployment ready (%d/%d)", dep.Status.ReadyReplicas, dep.Status.Replicas)
	case depErr == nil:
		phase = agentofficev1alpha1.AgentGatewayPhaseProvisioning
		message = fmt.Sprintf("Deployment present but not ready (%d/%d)", dep.Status.ReadyReplicas, dep.Status.Replicas)
	case apierrors.IsNotFound(depErr):
		// keep Pending
	default:
		return ctrl.Result{}, fmt.Errorf("get Deployment: %w", depErr)
	}

	endpoint := ""
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"})
	if err := r.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: gwRouteName(gw.Name)}, route); err == nil {
		host, _, _ := unstructured.NestedString(route.Object, "spec", "host")
		if host != "" {
			endpoint = "https://" + host
		}
	}

	// Count AWs referencing this gateway.
	var awList agentofficev1alpha1.AgentWorkstationList
	if err := r.List(ctx, &awList, client.InNamespace(gw.Namespace)); err == nil {
		count := int32(0)
		for _, aw := range awList.Items {
			if aw.Spec.Runtime != nil && aw.Spec.Runtime.Shared != nil && aw.Spec.Runtime.Shared.GatewayRef == gw.Name {
				count++
			}
		}
		gw.Status.AgentCount = count
	}

	if gw.Status.Phase == phase && gw.Status.Message == message && gw.Status.GatewayEndpoint == endpoint {
		// only AgentCount may have changed; still update if so
	}
	gw.Status.Phase = phase
	gw.Status.Message = message
	gw.Status.GatewayEndpoint = endpoint
	if err := r.Status().Update(ctx, gw); err != nil {
		return ctrl.Result{}, err
	}
	log.V(1).Info("gateway status", "phase", phase, "endpoint", endpoint, "agents", gw.Status.AgentCount)
	return ctrl.Result{}, nil
}

func (r *AgentGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// When a KnowledgeBase is created/updated/deleted with a
	// gatewayRef pointing at us, re-reconcile so the gateway
	// Deployment picks up the matching volume + mount on its
	// next pass.
	mapKB := func(ctx context.Context, obj client.Object) []reconcile.Request {
		kb, ok := obj.(*agentofficev1alpha1.KnowledgeBase)
		if !ok || kb.Spec.GatewayRef.Name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Namespace: kb.Namespace, Name: kb.Spec.GatewayRef.Name,
		}}}
	}

	// v1.4.1: when an AgentWorkstation with spec.tools.mcpServers
	// targeting this gateway changes, re-reconcile so
	// collectMCPEnvFromSecrets() picks up new/removed Secret refs
	// and the Deployment's envFrom + Reloader annotation stay in
	// sync with the union of AWs targeting this gateway.
	mapAW := func(ctx context.Context, obj client.Object) []reconcile.Request {
		aw, ok := obj.(*agentofficev1alpha1.AgentWorkstation)
		if !ok || aw.Spec.Runtime == nil || aw.Spec.Runtime.Shared == nil {
			return nil
		}
		if aw.Spec.Runtime.Shared.GatewayRef == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Namespace: aw.Namespace, Name: aw.Spec.Runtime.Shared.GatewayRef,
		}}}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.AgentGateway{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&agentofficev1alpha1.KnowledgeBase{}, handler.EnqueueRequestsFromMapFunc(mapKB)).
		Watches(&agentofficev1alpha1.AgentWorkstation{}, handler.EnqueueRequestsFromMapFunc(mapAW)).
		Named("agentgateway").
		Complete(r)
}
