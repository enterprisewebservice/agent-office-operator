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

	// 4. Seed per-agent workspace files. Always overwrite on every
	//    reconcile so AW spec edits (system-prompt tweaks, emoji
	//    changes, displayName updates) propagate to the gateway. The
	//    previous `[ -f ... ] || cat` skip-if-exists pattern stranded
	//    SOUL.md with stale content the moment the AW spec changed —
	//    in particular, the wbr agent ended up with an early
	//    truncated systemPrompt that omitted the "use browser with
	//    target: node" guidance, so the agent silently fell back to
	//    web_fetch / training data instead of driving the VM browser.
	//    Use Go templating (no shell HEREDOCs) to dodge quoting
	//    pitfalls when the prompt contains backticks / `$`.
	identity := defaultIfEmpty(aw.Spec.DisplayName, agentID)
	emoji := aw.Spec.Emoji
	identityMd := fmt.Sprintf("# %s\n\n%s %s\n", identity, emoji, identity)
	soulMd := fmt.Sprintf("# %s\n\n%s\n", identity, aw.Spec.SystemPrompt)

	// Resolve skills granted to this AW via SkillBindings. We
	// always render IDENTITY.md / SOUL.md; we render at most one
	// SKILL_<name>.md per granted skill (Anthropic Skills Open
	// Standard filename pattern). Skill files NOT in the granted
	// set are deleted on each reconcile so revoking a binding
	// removes the skill from the agent's workspace cleanly.
	resolvedSkills, skillResolveErr := r.listAppliedSkills(ctx, aw)
	if skillResolveErr != nil {
		log.Info("skill resolution failed; proceeding without skills",
			"err", skillResolveErr, "aw", aw.Name)
	}

	// Build the file map. Order: SOUL/IDENTITY first, then skills.
	writes := map[string]string{
		"IDENTITY.md": identityMd,
		"SOUL.md":     soulMd,
	}
	keepSkillFiles := map[string]struct{}{}
	for _, rs := range resolvedSkills {
		fname := fmt.Sprintf("SKILL_%s.md", rs.Skill.Name)
		writes[fname] = rs.Rendered
		keepSkillFiles[fname] = struct{}{}
	}

	writesJSON, _ := json.Marshal(writes)
	keepJSON, _ := json.Marshal(keepSkillFiles)

	seedScript := fmt.Sprintf(`
const fs = require("fs"); const path = require("path");
const dir = "/home/node/.openclaw/workspaces/" + %[1]q;
fs.mkdirSync(dir, { recursive: true });
const writes = %[2]s;
const keepSkillFiles = %[3]s;
let touched = 0;
for (const [f, content] of Object.entries(writes)) {
  const p = path.join(dir, f);
  let cur = "";
  try { cur = fs.readFileSync(p, "utf8"); } catch (e) {}
  if (cur !== content) { fs.writeFileSync(p, content); touched++; }
}
// Garbage-collect SKILL_*.md files that no current binding grants.
let gcd = 0;
try {
  for (const f of fs.readdirSync(dir)) {
    if (!f.startsWith("SKILL_") || !f.endsWith(".md")) continue;
    if (Object.prototype.hasOwnProperty.call(keepSkillFiles, f)) continue;
    fs.unlinkSync(path.join(dir, f));
    gcd++;
  }
} catch (e) {}
console.log("SEEDED dir=" + dir + " touched=" + touched + " gcd=" + gcd);
`, agentID, string(writesJSON), string(keepJSON))

	if _, err := r.execInPod(ctx, gwPod, []string{"node", "-e", seedScript}); err != nil {
		return ctrl.Result{}, fmt.Errorf("seed agent workspace: %w", err)
	}

	// 5. Merge agent entry, browser-profile allowlist, and Discord
	// binding into ~/.openclaw/openclaw.json.
	//
	// Loop-safety contract (this is the file the gateway daemon
	// watches; every write triggers a hot-reload, and unconditional
	// writes were how we ended up in a reconcile/reload thrash on a
	// previous slice): the merge is read → mutate-in-memory →
	// compare → write-only-if-changed. When the AW spec hasn't
	// drifted, this is a pure no-op and the gateway never sees a
	// file event.
	//
	// Binding model — one channel per agent persona, no catch-alls:
	//   * If the AW has spec.channels.discord.url with a real
	//     channel ID, write { agentId, match: { channel: <id>,
	//     accountId: "default" } }. accountId is the bot account
	//     name in openclaw.json (default = the gateway's lone bot),
	//     NOT the transport. Earlier slices had `"discord"` here
	//     (transport name), which never matches.
	//   * If the URL is empty / @me / not parseable, leave bindings
	//     alone for this agent and surface a friendly status hint.
	//   * Drop any binding whose match has neither `peer.id` nor
	//     `guildId` nor `teamId` AND whose accountPattern isn't a
	//     specific account — those are real catch-alls that route
	//     EVERY message on a transport to one agent (almost never
	//     intended in shared-runtime mode, where multiple agents
	//     coexist behind one bot).
	//   * Strip stale bindings for THIS agent (so renaming the
	//     channel updates routing without leaving an old rule
	//     behind) — keyed on agentId, regardless of peer.id, so
	//     every old rule for this agent is replaced by the one
	//     fresh write below.
	//
	// `openclaw config validate` is run advisory after; failures are
	// logged but not fatal because the merge above only ever
	// produces schema-valid JSON we constructed in Go.
	//
	// SCHEMA NOTE: openclaw's binding matcher reads `match.channel`
	// as the TRANSPORT NAME ("discord"/"slack"/etc), not the
	// channel ID. The actual Discord channel ID goes in
	// `match.peer = { kind: "channel", id: "<id>" }`. Earlier
	// slices wrote `match.channel: "<channel-id>"` and got "channel
	// id never matches" silently — every message fell through to
	// the `main` agent. See dist/resolve-route-*.js
	// `buildEvaluatedBindingsByChannel` for the lookup.
	mergeScript := fmt.Sprintf(`
const fs = require("fs");
const path = "/home/node/.openclaw/openclaw.json";
const agentEntry = %[1]s;
const profile = %[2]q;
const channelId = %[3]q;
const agentId = %[4]q;

const raw = fs.readFileSync(path, "utf8");
const cfg = JSON.parse(raw);
const before = JSON.stringify(cfg);

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

// bindings — strip stale-for-this-agent (any rule keyed on this
// agent gets replaced by the one fresh write below), strip wide
// catch-alls (transport-only with no peer/guild/team — they
// shadow specific routes), then re-add a single peer-specific
// entry if we have a real channel id from the AW spec.
function isWideCatchAll(b) {
  if (!b || !b.match) return true;
  const m = b.match;
  const hasPeer = m.peer && m.peer.id;
  const hasGuild = m.guildId;
  const hasTeam = m.teamId;
  const accountWild = !m.accountId || m.accountId === "*";
  return !hasPeer && !hasGuild && !hasTeam && accountWild;
}
let bindings = Array.isArray(cfg.bindings) ? cfg.bindings : [];
bindings = bindings.filter(b => {
  if (!b || !b.match) return false;
  if (b.agentId === agentId) return false;     // this agent's old rule(s) — re-added fresh below
  if (isWideCatchAll(b)) return false;         // wide catch-alls shadow specifics
  return true;
});
if (channelId) {
  bindings.push({
    agentId,
    match: {
      channel: "discord",
      accountId: "default",
      peer: { kind: "channel", id: channelId },
    },
  });
}
// Stable sort by (agentId, peer.id) for deterministic file content.
bindings.sort((a,b) => {
  const aKey = (a.agentId || "") + "\t" + ((a.match && a.match.peer && a.match.peer.id) || "");
  const bKey = (b.agentId || "") + "\t" + ((b.match && b.match.peer && b.match.peer.id) || "");
  return aKey.localeCompare(bKey);
});
cfg.bindings = bindings;

const after = JSON.stringify(cfg);
if (after === before) {
  console.log("NO_CHANGE agent=" + agentId + " profile=" + profile + " channel=" + (channelId || "(none)"));
} else {
  fs.writeFileSync(path, JSON.stringify(cfg, null, 2));
  console.log("MERGED agent=" + agentId + " profile=" + profile + " channel=" + (channelId || "(none)") + " bindings=" + bindings.length);
}
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

	// 5b. Best-effort post-merge validate (advisory). The merge above
	// only writes schema-valid JSON we constructed in Go, and the
	// gateway hot-reloads bindings without bouncing the pod, but we
	// still call validate so genuine drift surfaces in operator
	// logs. Failures are logged at V(1) and never fatal.
	if validateOut, vErr := r.execInPod(ctx, gwPod, []string{"openclaw", "config", "validate"}); vErr != nil {
		log.V(1).Info("openclaw config validate after merge (advisory only)",
			"err", vErr, "stdout", strings.TrimSpace(validateOut))
	}

	// 7. Status — Running with a hint about Discord routing so
	// users can see at a glance whether their @-mentions will
	// reach this agent.
	channelID := parseDiscordChannelID(awDiscordURL(aw))
	channelHint := "no Discord binding (set spec.channels.discord.url to a channel link)"
	if channelID != "" {
		channelHint = fmt.Sprintf("Discord channel %s", channelID)
	}
	aw.Status.Phase = agentofficev1alpha1.AgentWorkstationPhaseRunning
	aw.Status.Message = fmt.Sprintf("logical agent inside %s (profile=%s, %s)", gwRef, profile, channelHint)
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
