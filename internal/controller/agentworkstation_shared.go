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

	// Render the FULL local skill catalog into the agent's workspace
	// (v1.6.0 discovery model). Every Skill CR in the namespace lands
	// at skills/<name>/SKILL.md — the Anthropic Skills Open Standard
	// folder layout, as adopted by Claude Code, Claude.ai, Claude
	// Agent SDK, and the wider ecosystem.
	//
	// "Render everything" instead of "render what SkillBinding grants"
	// is the discovery shift v1.6.0 establishes. The agent's runtime
	// decides at runtime which skills are relevant to the current
	// task — scans frontmatter for metadata, loads body only on
	// trigger. Pre-deciding via SkillBinding was the binding model
	// we're explicitly moving away from. SkillBinding still exists
	// for backward compat (the binding controller still updates
	// status fields; AWs authored before v1.6.0 keep working) but
	// the AW reconciler no longer consults it.
	//
	// Folder-per-skill (vs the legacy SKILL_<name>.md flat-file
	// pattern that's GC'd below) earns its keep:
	//   1) Bundled scripts/references/assets that real Anthropic
	//      Skills carry have a natural home (skills/<name>/scripts/
	//      etc.) — the flat-file layout couldn't represent them.
	//   2) Runtimes that support Anthropic-native progressive
	//      disclosure (scan dir + read frontmatter only at session
	//      start; lazy-load body on trigger) find the files in the
	//      shape they expect — no rework when OpenClaw catches up.
	resolvedSkills, skillResolveErr := r.listAllCatalogSkills(ctx, aw.Namespace)
	if skillResolveErr != nil {
		log.Info("skill catalog list failed; proceeding without skills",
			"err", skillResolveErr, "aw", aw.Name)
	}

	// Resolve KnowledgeBases attached to the gateway this AW
	// shares. Each KB shows up at /home/node/.openclaw/wiki/<kb>/
	// inside the gateway pod, visible to every logical agent.
	// We render a `WIKI.md` workspace file listing the available
	// wikis so the agent reads about them at session start (per
	// the openclaw AGENTS.md convention) and knows where to look
	// for `wiki-search` / `wiki-write` skill operations.
	wikiMd, _ := r.renderWikiMd(ctx, aw, gwRef)

	// Non-skill workspace files (always written from spec).
	writes := map[string]string{
		"IDENTITY.md": identityMd,
		"SOUL.md":     soulMd,
	}
	if wikiMd != "" {
		writes["WIKI.md"] = wikiMd
	}
	// Skills (v1.6.1): the SOURCE OF TRUTH is the skills-catalog image
	// volume mounted at skillsCatalogMountPath on the gateway pod. The
	// seed copies whole skill folders out of it — so bundled
	// scripts/references/assets ride along, which inline rendering
	// couldn't carry. inlineSkillWrites is the FALLBACK used only when
	// the image volume isn't present (old pod mid-rollout, or the
	// mount failed) so we never strand an agent with no skills.
	inlineSkillWrites := map[string]string{}
	keepSkillDirs := map[string]struct{}{}
	for _, rs := range resolvedSkills {
		inlineSkillWrites[fmt.Sprintf("skills/%s/SKILL.md", rs.Skill.Name)] = rs.Rendered
		keepSkillDirs[rs.Skill.Name] = struct{}{}
	}

	writesJSON, _ := json.Marshal(writes)
	inlineSkillJSON, _ := json.Marshal(inlineSkillWrites)
	keepJSON, _ := json.Marshal(keepSkillDirs)

	seedScript := fmt.Sprintf(`
const fs = require("fs"); const path = require("path");
const dir = "/home/node/.openclaw/workspaces/" + %[1]q;
fs.mkdirSync(dir, { recursive: true });
const writes = %[2]s;
const inlineSkillWrites = %[3]s;
const keepSkillDirs = %[4]s;
const catalogDir = %[5]q;

// 1. Non-skill files (IDENTITY/SOUL/WIKI) — write-if-changed.
let touched = 0;
for (const [f, content] of Object.entries(writes)) {
  const p = path.join(dir, f);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  let cur = ""; try { cur = fs.readFileSync(p, "utf8"); } catch (e) {}
  if (cur !== content) { fs.writeFileSync(p, content); touched++; }
}

// 2. Skills. Prefer the mounted catalog image (full folders, incl.
//    bundled resources). Fall back to inline SKILL.md only if the
//    mount isn't present yet.
const skillsRoot = path.join(dir, "skills");
let keep = {};
let skillSrc = "none";
let catalogNames = [];
try {
  catalogNames = fs.readdirSync(catalogDir).filter(n => {
    try { return fs.statSync(path.join(catalogDir, n)).isDirectory(); }
    catch (e) { return false; }
  });
} catch (e) {}

if (catalogNames.length > 0) {
  skillSrc = "image";
  fs.mkdirSync(skillsRoot, { recursive: true });
  for (const name of catalogNames) {
    // cpSync(recursive) brings SKILL.md + scripts/ + references/ etc.
    fs.cpSync(path.join(catalogDir, name), path.join(skillsRoot, name),
      { recursive: true, force: true });
    keep[name] = true;
    touched++;
  }
} else {
  skillSrc = "inline-fallback";
  for (const [f, content] of Object.entries(inlineSkillWrites)) {
    const p = path.join(dir, f);
    fs.mkdirSync(path.dirname(p), { recursive: true });
    let cur = ""; try { cur = fs.readFileSync(p, "utf8"); } catch (e) {}
    if (cur !== content) { fs.writeFileSync(p, content); touched++; }
  }
  keep = keepSkillDirs;
}

// GC #1: remove workspace skill folders not in the current source set.
let gcd = 0;
try {
  for (const name of fs.readdirSync(skillsRoot)) {
    if (Object.prototype.hasOwnProperty.call(keep, name)) continue;
    fs.rmSync(path.join(skillsRoot, name), { recursive: true, force: true });
    gcd++;
  }
} catch (e) {}

// GC #2: legacy SKILL_<name>.md flat files from pre-v1.6.0 reconciles.
let legacyGcd = 0;
try {
  for (const f of fs.readdirSync(dir)) {
    if (f.startsWith("SKILL_") && f.endsWith(".md")) {
      fs.unlinkSync(path.join(dir, f));
      legacyGcd++;
    }
  }
} catch (e) {}

console.log("SEEDED dir=" + dir +
  " touched=" + touched +
  " skillSrc=" + skillSrc +
  " skills=" + Object.keys(keep).length +
  " gcd=" + gcd +
  " legacy_gcd=" + legacyGcd);
`, agentID, string(writesJSON), string(inlineSkillJSON), string(keepJSON), skillsCatalogMountPath)

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

	// 6. Reconcile any MCP servers declared on this AW into the
	//    gateway's openclaw mcp config + the gateway Deployment's
	//    envFrom (idempotent; safe to skip-on-error per call).
	//    This is what wires a PM Agent (or any AW) to the Kuadrant
	//    MCP Gateway endpoint so it can call federated tools like
	//    `github_list_issues` without ever seeing a credential.
	if aw.Spec.Tools != nil && len(aw.Spec.Tools.MCPServers) > 0 {
		if err := r.reconcileMCPServers(ctx, aw, &gw, gwPod); err != nil {
			log.Info("mcp servers reconcile partial; will retry next loop",
				"err", err)
			// Non-fatal — the rest of the agent is still up. Surface
			// in status so it's visible but stay Running.
			aw.Status.Message = fmt.Sprintf("%s — MCP setup pending: %s",
				aw.Status.Message, truncate(err.Error(), 120))
		}
	}

	// 6b. v1.5.0: render the per-agent KBS.md from spec.knowledgeBaseRefs.
	//     The KB PVCs are mounted at the gateway level already (handled
	//     by the AG reconciler's existing attachedKBs logic). What's
	//     per-agent is the curated list with role hints, surfaced as a
	//     markdown file in the agent's workspace dir. The agent's
	//     system prompt can teach the agent to read
	//     ~/.openclaw/workspaces/<agent>/KBS.md at conversation start.
	if len(aw.Spec.KnowledgeBaseRefs) > 0 {
		if err := r.reconcileKnowledgeBaseRefs(ctx, aw, &gw, gwPod); err != nil {
			log.Info("knowledgeBaseRefs reconcile partial; will retry",
				"err", err)
			aw.Status.Message = fmt.Sprintf("%s — KB attach pending: %s",
				aw.Status.Message, truncate(err.Error(), 120))
		}
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
	// Separate buffers — bytes.Buffer is not goroutine-safe, and
	// client-go writes stdout/stderr from different goroutines.
	// See AutoResearchProjectReconciler.execInGatewayPod for the
	// full story; passing the same buffer for both streams was the
	// root cause of intermittent `parse agent reply: empty reply`.
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return stdout.String() + stderr.String(), fmt.Errorf("exec: %w", err)
	}
	return stdout.String(), nil
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

// reconcileMCPServers handles spec.tools.mcpServers on a shared-runtime
// AW. Added in v1.4.0; substantially refactored in v1.4.1 to fix a
// race with the AgentGateway reconciler.
//
// Two responsibilities here:
//
//  1. For each declared MCP server, exec `openclaw mcp set
//     <name> '<json>'` inside the gateway pod. Idempotent — openclaw
//     replaces by name. The pod auto-reloads on openclaw.json change.
//
//  2. If any server declares an EnvFromSecret, TOUCH the AgentGateway
//     CR (annotation bump) so the AgentGateway reconciler runs
//     immediately and picks up the new EnvFromSecret in its
//     collectMCPEnvFromSecrets() pass — which is where the envFrom
//     + Reloader annotation actually get applied to the Deployment.
//
// v1.4.0 tried to do step 2's work HERE (patch the Deployment
// directly). That lost the race against the AG reconciler, which
// owns the Deployment via SetControllerReference and rebuilds the
// pod template from scratch every pass — wiping our patches. v1.4.1
// moves the merge into the AG reconciler and uses this AW reconciler
// only to nudge it.
//
// Errors are returned for the caller to surface in status; the
// caller treats partial failure as non-fatal (the agent itself is
// up regardless).
//
// Known limitation: removing an entry from spec.tools.mcpServers
// does NOT call `openclaw mcp unset` — the entry stays in
// openclaw.json until manually removed. Adding the unset path
// requires tracking "what we previously set"; deferred until first
// user request.
func (r *AgentWorkstationReconciler) reconcileMCPServers(
	ctx context.Context,
	aw *agentofficev1alpha1.AgentWorkstation,
	gw *agentofficev1alpha1.AgentGateway,
	gwPod *corev1.Pod,
) error {
	log := logf.FromContext(ctx)

	// Step 1: openclaw mcp set per server.
	for _, srv := range aw.Spec.Tools.MCPServers {
		mcpType := srv.Type
		if mcpType == "" {
			mcpType = "http"
		}
		cfg := map[string]interface{}{
			"url":  srv.URL,
			"type": mcpType,
		}
		if len(srv.Headers) > 0 {
			cfg["headers"] = srv.Headers
		}
		cfgJSON, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshal mcp config for %q: %w", srv.Name, err)
		}
		out, err := r.execInPod(ctx, gwPod, []string{
			"openclaw", "mcp", "set", srv.Name, string(cfgJSON),
		})
		if err != nil {
			return fmt.Errorf("openclaw mcp set %q: %w (out=%s)",
				srv.Name, err, strings.TrimSpace(out))
		}
		log.Info("mcp server registered with openclaw",
			"name", srv.Name, "agent", aw.Name,
			"url", srv.URL, "type", mcpType)
	}

	// Step 2: any EnvFromSecret declared? Touch the AgentGateway so
	// its reconciler runs and re-builds the Deployment with the new
	// secrets in envFrom + Reloader annotation. Watches on
	// AgentWorkstation also trigger this, but the explicit touch
	// guarantees promptness for users testing manually.
	hasEnvFromSecret := false
	for _, srv := range aw.Spec.Tools.MCPServers {
		if srv.EnvFromSecret != "" {
			hasEnvFromSecret = true
			break
		}
	}
	if !hasEnvFromSecret {
		return nil
	}
	if gw.Annotations == nil {
		gw.Annotations = map[string]string{}
	}
	desired := fmt.Sprintf("%d", time.Now().Unix())
	if gw.Annotations["agentoffice.ai/mcp-secrets-touch"] != desired {
		gw.Annotations["agentoffice.ai/mcp-secrets-touch"] = desired
		if err := r.Update(ctx, gw); err != nil {
			return fmt.Errorf("touch gateway %s/%s for mcp reconcile: %w",
				gw.Namespace, gw.Name, err)
		}
		log.Info("touched gateway for mcp envFrom reconcile",
			"gateway", gw.Name, "agent", aw.Name)
	}
	return nil
}

// reconcileKnowledgeBaseRefs (v1.5.0+) renders a per-agent KBS.md into
// the agent's workspace directory inside the gateway pod, describing
// each KB attached to this AW via spec.knowledgeBaseRefs.
//
// Why per-agent and not per-gateway: the KB's PVC is mounted at the
// gateway level (existing AG reconciler logic — handles dedup,
// git-sync sidecars, volume topology). Many agents on the same
// gateway share the underlying mounts. What we want is per-agent
// CURATION: an agent only "knows about" the KBs explicitly listed in
// its own spec.knowledgeBaseRefs, with the AW-specific role hint
// telling the agent HOW to use each KB. KBS.md is the materialized
// per-agent index.
//
// Validation: every ref's KnowledgeBase must (a) exist in the
// agent's namespace and (b) have spec.gatewayRef.name matching the
// agent's gateway. Cross-gateway refs are refused with a status
// condition `KnowledgeBaseRefsResolved=False, reason=GatewayMismatch`
// — the Backstage plugin should grey-out incompatible KBs in the
// drag-drop UI based on the same compatibility check.
//
// Idempotent — KBS.md is rewritten on every reconcile pass.
func (r *AgentWorkstationReconciler) reconcileKnowledgeBaseRefs(
	ctx context.Context,
	aw *agentofficev1alpha1.AgentWorkstation,
	gw *agentofficev1alpha1.AgentGateway,
	gwPod *corev1.Pod,
) error {
	log := logf.FromContext(ctx)

	// Build the resolved entries, deduplicating by name.
	seen := map[string]struct{}{}
	type resolved struct {
		Ref agentofficev1alpha1.KnowledgeBaseRef
		KB  *agentofficev1alpha1.KnowledgeBase
	}
	entries := make([]resolved, 0, len(aw.Spec.KnowledgeBaseRefs))
	var mismatchErrs []string
	var notFoundErrs []string
	for _, ref := range aw.Spec.KnowledgeBaseRefs {
		if _, dup := seen[ref.Name]; dup {
			continue
		}
		seen[ref.Name] = struct{}{}
		var kb agentofficev1alpha1.KnowledgeBase
		if err := r.Get(ctx, client.ObjectKey{Namespace: aw.Namespace, Name: ref.Name}, &kb); err != nil {
			notFoundErrs = append(notFoundErrs, ref.Name)
			continue
		}
		if kb.Spec.GatewayRef.Name != gw.Name {
			mismatchErrs = append(mismatchErrs, fmt.Sprintf("%s (on %s, agent on %s)",
				ref.Name, kb.Spec.GatewayRef.Name, gw.Name))
			continue
		}
		entries = append(entries, resolved{Ref: ref, KB: &kb})
	}

	// Render KBS.md content. The format is markdown the agent can
	// parse: one section per KB with name, role, path, displayName,
	// description.
	var sb strings.Builder
	sb.WriteString("# Knowledge Bases attached to this agent\n\n")
	sb.WriteString("You have access to the following Knowledge Bases. Each\n")
	sb.WriteString("entry says what the KB is and HOW you should use it for\n")
	sb.WriteString("your role.\n\n")
	sb.WriteString("Role vocabulary:\n")
	sb.WriteString("  - planning-reference  : read at start of every conversation\n")
	sb.WriteString("  - knowledge-pool      : search on-demand (wiki-search Skill)\n")
	sb.WriteString("  - experiment-history  : read when working on the related CR\n")
	sb.WriteString("  - runbook             : read when triggered operationally\n")
	sb.WriteString("  - style-guide         : read when producing the relevant output\n\n")

	if len(entries) == 0 && len(notFoundErrs) == 0 && len(mismatchErrs) == 0 {
		sb.WriteString("_(no KnowledgeBases attached yet)_\n")
	}
	for _, e := range entries {
		role := e.Ref.Role
		if role == "" {
			role = "knowledge-pool"
		}
		displayName := e.KB.Spec.DisplayName
		if displayName == "" {
			displayName = e.KB.Name
		}
		desc := strings.TrimSpace(e.KB.Spec.Description)
		mount := fmt.Sprintf("/home/node/.openclaw/wiki/%s/", e.KB.Name)
		sb.WriteString(fmt.Sprintf("## %s — role: %s\n\n", e.KB.Name, role))
		sb.WriteString(fmt.Sprintf("**Display name**: %s\n\n", displayName))
		sb.WriteString(fmt.Sprintf("**Mount path on this gateway**: `%s`\n\n", mount))
		if desc != "" {
			sb.WriteString("**Description**:\n\n")
			for _, line := range strings.Split(desc, "\n") {
				sb.WriteString("> " + line + "\n")
			}
			sb.WriteString("\n")
		}
	}
	if len(mismatchErrs) > 0 {
		sb.WriteString("## Refs refused (gateway mismatch)\n\n")
		for _, m := range mismatchErrs {
			sb.WriteString("- " + m + "\n")
		}
		sb.WriteString("\n")
	}
	if len(notFoundErrs) > 0 {
		sb.WriteString("## Refs refused (KnowledgeBase not found)\n\n")
		for _, n := range notFoundErrs {
			sb.WriteString("- " + n + "\n")
		}
		sb.WriteString("\n")
	}
	content := sb.String()

	// Write to the agent's workspace dir. Path follows the same
	// convention as agents.list[].workspace.
	workspacePath := fmt.Sprintf("/home/node/.openclaw/workspaces/%s", aw.Name)
	target := workspacePath + "/KBS.md"

	// Build a small heredoc-via-shell script so we don't need to
	// stream the file via stdin. tee writes atomically enough for
	// our purposes (overwrite-on-rewrite).
	script := fmt.Sprintf(`set -eu
mkdir -p %s
cat > %s <<'AGENTOFFICE_KBS_EOF'
%sAGENTOFFICE_KBS_EOF
echo "wrote $(wc -c < %s) bytes to %s"`,
		shellEscape(workspacePath),
		shellEscape(target),
		content,
		shellEscape(target),
		shellEscape(target),
	)
	out, err := r.execInPod(ctx, gwPod, []string{"/bin/sh", "-c", script})
	if err != nil {
		return fmt.Errorf("write KBS.md for %q: %w (out=%s)",
			aw.Name, err, strings.TrimSpace(out))
	}
	log.Info("rendered KBS.md",
		"agent", aw.Name,
		"target", target,
		"attached", len(entries),
		"mismatches", len(mismatchErrs),
		"notFound", len(notFoundErrs))
	if len(mismatchErrs) > 0 || len(notFoundErrs) > 0 {
		return fmt.Errorf("partial KnowledgeBaseRefs resolution: mismatches=%d notFound=%d",
			len(mismatchErrs), len(notFoundErrs))
	}
	return nil
}

// shellEscape wraps a string in single quotes for safe shell use,
// escaping any embedded single quotes.
func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
