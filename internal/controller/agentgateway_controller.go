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
	"regexp"
	"sort"
	"strings"
	"time"

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

// Pod delete is required by restartGatewayPods: openclaw.json is read
// at process start, so a converged config change is inert until the
// gateway process restarts. Without this verb the operator writes the
// right config and then silently fails to apply it —
// `cannot delete resource "pods"` — leaving the gateway on its old
// credential route until something else happens to bounce the pod.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete

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

	// 3. Converge the gateway's openclaw.json onto one canonical
	//    `openai` provider pinned to the credential spec.modelAuth
	//    asks for (subscription vs metered API key). The init
	//    container is seed-only, so existing PVCs need this in-place.
	//    Idempotent: a converged config reports NO_CHANGE and we
	//    leave the pod alone.
	if changed, err := r.reconcileModelProvidersAndAuth(ctx, &gw); err != nil {
		log.Info("model provider/auth reconcile skipped", "err", err)
	} else if changed {
		// openclaw.json is read at process start — the write above is
		// inert until the gateway restarts.
		log.Info("model auth config changed; restarting gateway pods")
		if err := r.restartGatewayPods(ctx, &gw); err != nil {
			log.Error(err, "restart gateway pods after model auth change")
		}
		// Come back once the new pod is Ready to verify convergence.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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

// defaultSubscriptionModels is the catalog advertised when the
// gateway is on the ChatGPT/Codex subscription route. These resolve
// against chatgpt.com/backend-api and are only reachable with an
// OAuth credential — gpt-5.6-sol in particular does NOT exist on the
// metered API endpoint, so declaring it under an api-key provider
// yields a silent lane rather than an error.
var defaultSubscriptionModels = []agentofficev1alpha1.ModelCatalogEntry{
	{ID: "gpt-5.6-sol", Name: "GPT-5.6 Sol"},
	{ID: "gpt-5.6", Name: "GPT-5.6"},
	{ID: "gpt-5.5", Name: "GPT-5.5"},
	{ID: "gpt-5.4", Name: "GPT-5.4"},
}

// defaultAPIKeyModels is the catalog advertised on the metered API
// route.
var defaultAPIKeyModels = []agentofficev1alpha1.ModelCatalogEntry{
	{ID: "gpt-5.5", Name: "GPT-5.5"},
	{ID: "gpt-5.4", Name: "GPT-5.4"},
	{ID: "gpt-4o-mini", Name: "gpt-4o-mini"},
	{ID: "gpt-4o", Name: "gpt-4o"},
}

// resolveModelAuthMode returns the gateway's effective billing route.
// Explicit spec wins; otherwise the presence of codex subscription
// credentials implies the subscription route.
func resolveModelAuthMode(gw *agentofficev1alpha1.AgentGateway) agentofficev1alpha1.ModelAuthMode {
	if gw.Spec.ModelAuth != nil && gw.Spec.ModelAuth.Mode != "" {
		return gw.Spec.ModelAuth.Mode
	}
	if gw.Spec.CodexCredentialsSecretRef != "" {
		return agentofficev1alpha1.ModelAuthModeSubscription
	}
	return agentofficev1alpha1.ModelAuthModeAPIKey
}

// ansiEscape matches the SGR color sequences the openclaw CLI emits
// even under --json.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// extractJSONDocument pulls the first well-formed JSON document out of
// noisy CLI output.
//
// It exists because extractJSON (which returns from the first '[' or
// '{') cannot handle this command: `openclaw models auth list --json`
// prefixes its output with a colorized `[state-migrations]` banner, so
// the first '[' in the stream sits INSIDE the ANSI escape `\x1b[32m` —
// several characters before any JSON. Naive scanning yields
// "[32m[state-migrations]..." and fails to parse every time.
//
// Strategy: strip ANSI, then try each '{'/'[' as a start offset and
// return the first substring that actually unmarshals. Returns "" when
// no candidate parses, so callers can report a real error rather than
// silently treating garbage as an empty result.
func extractJSONDocument(s string) string {
	clean := ansiEscape.ReplaceAllString(s, "")
	for i, c := range clean {
		if c != '{' && c != '[' {
			continue
		}
		candidate := clean[i:]
		var probe json.RawMessage
		if json.Unmarshal([]byte(candidate), &probe) == nil {
			return candidate
		}
	}
	return ""
}

// authProfileRow is one entry from `openclaw models auth list --json`.
type authProfileRow struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Type     string `json:"type"`
	Email    string `json:"email"`
}

// authProfileListing is the envelope that command returns.
type authProfileListing struct {
	Profiles []authProfileRow `json:"profiles"`
}

// discoverOpenAIAuthProfile asks the running gateway which credentials
// it actually holds for provider `openai` and returns the profile id
// matching the desired mode.
//
// Discovery (rather than a hardcoded id) is required because OpenClaw
// derives OAuth profile ids from the account email —
// `openai:you@example.com` — so the id is per-user and changes if the
// account does. The credential itself arrives via
// spec.codexCredentialsSecretRef → ~/.codex/auth.json, which OpenClaw
// imports on startup.
// Returns the preferred profile id plus EVERY id that matches the
// mode. The full set matters: the caller pins one, but any of them is
// an acceptable pin, and treating them as interchangeable is what
// keeps the operator from fighting itself (see reconcile stickiness
// below).
func (r *AgentGatewayReconciler) discoverOpenAIAuthProfile(
	ctx context.Context, pod *corev1.Pod, mode agentofficev1alpha1.ModelAuthMode,
) (string, []string, error) {
	out, err := r.execInGatewayPod(ctx, pod, []string{
		"openclaw", "models", "auth", "list", "--provider", "openai", "--json",
	})
	if err != nil {
		return "", nil, fmt.Errorf("models auth list: %w (out=%s)", err, out)
	}
	doc := extractJSONDocument(out)
	if doc == "" {
		return "", nil, fmt.Errorf("no JSON document in auth list output (out=%s)", out)
	}
	var listing authProfileListing
	if err := json.Unmarshal([]byte(doc), &listing); err != nil {
		return "", nil, fmt.Errorf("parse auth list: %w (out=%s)", err, out)
	}
	// OpenClaw types: "oauth" (subscription), "api_key", "token".
	want := "oauth"
	if mode == agentofficev1alpha1.ModelAuthModeAPIKey {
		want = "api_key"
	}
	var matching []string
	for _, p := range listing.Profiles {
		if p.Type == want {
			matching = append(matching, p.ID)
		}
	}
	if len(matching) == 0 {
		return "", nil, fmt.Errorf("no %q auth profile for provider openai (have %d profiles)", want, len(listing.Profiles))
	}
	// Sort so the PREFERRED pick doesn't depend on the CLI's listing
	// order — an unstable choice here would rewrite auth.order (and
	// therefore restart the gateway) on alternating reconciles.
	sort.Strings(matching)
	return matching[0], matching, nil
}

// desiredOpenAIConfig is the JSON contract handed to the in-pod node
// script: the exact provider block + auth wiring the gateway should
// converge on.
type desiredOpenAIConfig struct {
	Provider map[string]interface{}       `json:"provider"`
	Profiles map[string]map[string]string `json:"profiles"`
	Order    map[string][]string          `json:"order"`

	// AcceptableOrder lists, per provider, every profile id that would
	// satisfy the requested mode. When the config already pins one of
	// these, the script KEEPS it instead of rewriting to our preferred
	// pick.
	//
	// This stickiness prevents a restart loop. A config change
	// restarts the gateway, so if discovery ever returned a different
	// (but equally valid) id on alternating reconciles, the operator
	// would bounce the pod forever. OpenClaw can legitimately expose
	// two OAuth profiles at once — the bootstrapOnly `openai:default`
	// imported from ~/.codex/auth.json, and a managed
	// `openai:<email>` — and which are visible shifts as it takes
	// ownership of the credential. Any of them bills the same
	// subscription, so once pinned, leave it pinned.
	//
	// Empty for a provider ⇒ no stickiness; Order is authoritative
	// (that's the spec.modelAuth.order escape hatch).
	AcceptableOrder map[string][]string `json:"acceptableOrder"`
}

// reconcileModelProvidersAndAuth converges the gateway's openclaw.json
// onto exactly ONE canonical `openai` provider wired to the credential
// the spec asks for, and pins auth.order so OpenClaw cannot silently
// pick a different one.
//
// Why this is not a simple "seed if absent" merge any more:
//
//  1. OpenClaw 2026.7.x folded provider `openai-codex` INTO canonical
//     `openai`. Keeping both blocks meant the subscription models
//     (gpt-5.6-sol) were declared under the legacy id while the
//     canonical block supplied baseUrl api.openai.com + an API key —
//     so a ChatGPT OAuth token got sent to the metered endpoint and
//     every agent turn died silently. The legacy block must be
//     DELETED, not merged alongside.
//  2. The api id `openai-codex-responses` was REMOVED in 7.x; the
//     replacement is `openai-chatgpt-responses`.
//  3. With no auth.order, OpenClaw picks a credential implicitly. A
//     stale `token`-type profile or a config-level apiKey can win over
//     the subscription, which is how agent turns ended up on
//     pay-per-token billing without anyone choosing that. An explicit
//     auth.order is EXCLUSIVE in OpenClaw — profiles not listed are
//     dropped — so pinning it makes the billing route deterministic.
//
// Returns true when it changed the config (caller must restart the
// gateway: openclaw.json is read at process start).
func (r *AgentGatewayReconciler) reconcileModelProvidersAndAuth(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) (bool, error) {
	if r.RestConfig == nil {
		return false, fmt.Errorf("RestConfig not set; cannot exec")
	}
	pod, err := r.findReadyGatewayPod(ctx, gw)
	if err != nil {
		return false, err
	}

	mode := resolveModelAuthMode(gw)

	// Model catalog: spec override, else built-in default for the mode.
	catalog := defaultSubscriptionModels
	if mode == agentofficev1alpha1.ModelAuthModeAPIKey {
		catalog = defaultAPIKeyModels
	}
	if gw.Spec.ModelAuth != nil && len(gw.Spec.ModelAuth.Models) > 0 {
		catalog = gw.Spec.ModelAuth.Models
	}

	// Provider block. The two routes differ in baseUrl, api, and
	// whether an apiKey is present at all — a subscription gateway
	// must NOT carry `apiKey`, or OpenClaw has a metered credential
	// available to fall back to.
	provider := map[string]interface{}{}
	models := make([]map[string]interface{}, 0, len(catalog))
	switch mode {
	case agentofficev1alpha1.ModelAuthModeAPIKey:
		provider["baseUrl"] = "https://api.openai.com/v1"
		provider["api"] = "openai-completions"
		provider["apiKey"] = "${OPENAI_API_KEY}"
		for _, m := range catalog {
			models = append(models, map[string]interface{}{"id": m.ID, "name": defaultIfEmpty(m.Name, m.ID)})
		}
	default: // subscription
		provider["baseUrl"] = "https://chatgpt.com/backend-api"
		provider["api"] = "openai-chatgpt-responses"
		for _, m := range catalog {
			models = append(models, map[string]interface{}{
				"id": m.ID, "name": defaultIfEmpty(m.Name, m.ID), "api": "openai-chatgpt-responses",
			})
		}
	}
	provider["models"] = models

	desired := desiredOpenAIConfig{
		Provider:        provider,
		Profiles:        map[string]map[string]string{},
		Order:           map[string][]string{},
		AcceptableOrder: map[string][]string{},
	}

	// Pin the credential. Explicit spec.modelAuth.order wins outright
	// (and is deliberately NOT sticky — the author asked for exactly
	// that order).
	if gw.Spec.ModelAuth != nil && len(gw.Spec.ModelAuth.Order) > 0 {
		for prov, ids := range gw.Spec.ModelAuth.Order {
			desired.Order[prov] = ids
		}
	} else {
		profileID := ""
		var acceptable []string
		if gw.Spec.ModelAuth != nil {
			profileID = gw.Spec.ModelAuth.ProfileID
		}
		if profileID == "" {
			profileID, acceptable, err = r.discoverOpenAIAuthProfile(ctx, pod, mode)
			if err != nil {
				// In apiKey mode a stored profile is optional — the
				// config-level apiKey is credential enough, so don't
				// block the provider write on it.
				if mode != agentofficev1alpha1.ModelAuthModeAPIKey {
					return false, fmt.Errorf("pin subscription credential: %w", err)
				}
				logf.FromContext(ctx).V(1).Info("no stored api_key profile; relying on config apiKey", "err", err)
			}
		}
		if profileID != "" {
			profMode := "oauth"
			if mode == agentofficev1alpha1.ModelAuthModeAPIKey {
				profMode = "api_key"
			}
			desired.Profiles[profileID] = map[string]string{"provider": "openai", "mode": profMode}
			desired.Order["openai"] = []string{profileID}
			desired.AcceptableOrder["openai"] = acceptable
		}
	}

	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return false, fmt.Errorf("marshal desired openai config: %w", err)
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

// Desired state computed by the operator from the AgentGateway spec.
const desired = JSON.parse(process.argv[1]);
const eq = (a, b) => JSON.stringify(a) === JSON.stringify(b);

// ONE canonical openai provider, wired to exactly one credential
// route. Declarative overwrite (not seed-if-absent): a drifted or
// hand-edited block must converge back, and the api-key vs
// subscription shapes are mutually exclusive.
if (!eq(cfg.models.providers.openai, desired.provider)) {
  cfg.models.providers.openai = desired.provider;
  changed = true;
}

// Delete the legacy provider id. OpenClaw 2026.7.x folds
// "openai-codex" into "openai" and rejects it as a legacy id; leaving
// it in place is what let subscription-only models resolve against
// the metered api.openai.com block.
if (cfg.models.providers["openai-codex"]) {
  delete cfg.models.providers["openai-codex"];
  changed = true;
}

// auth.profiles carries routing metadata only (provider + mode) —
// never secret material. Credentials live in the agent auth store,
// seeded from the mounted ~/.codex/auth.json.
cfg.auth = cfg.auth || {};
cfg.auth.profiles = cfg.auth.profiles || {};
cfg.auth.order = cfg.auth.order || {};
for (const [id, prof] of Object.entries(desired.profiles || {})) {
  if (!eq(cfg.auth.profiles[id], prof)) {
    cfg.auth.profiles[id] = prof;
    changed = true;
  }
}
for (const [prov, ids] of Object.entries(desired.order || {})) {
  // Stickiness: if the config already pins profile ids that all still
  // satisfy the requested mode, keep them. Rewriting to an equally
  // valid alternative would change the config, which restarts the
  // gateway — on every reconcile, forever.
  const acceptable = (desired.acceptableOrder || {})[prov] || [];
  const current = cfg.auth.order[prov];
  if (acceptable.length && Array.isArray(current) && current.length &&
      current.every((id) => acceptable.includes(id))) {
    continue;
  }
  if (!eq(current, ids)) {
    cfg.auth.order[prov] = ids;
    changed = true;
  }
}
// An order keyed by the legacy provider id validates but is silently
// ignored for provider "openai" — drop it so it can't mislead.
if (cfg.auth.order["openai-codex"]) {
  delete cfg.auth.order["openai-codex"];
  changed = true;
}

if (changed) {
  fs.writeFileSync(p, JSON.stringify(cfg, null, 2));
  console.log("RECONCILED_MODEL_AUTH");
} else {
  console.log("NO_CHANGE");
}
`
	out, err := r.execInGatewayPod(ctx, pod, []string{"node", "-e", script, string(desiredJSON)})
	if err != nil {
		return false, fmt.Errorf("reconcile model providers/auth: %w (out=%s)", err, out)
	}
	result := strings.TrimSpace(out)
	logf.FromContext(ctx).V(1).Info("model provider/auth reconcile", "mode", mode, "result", result)
	return strings.Contains(result, "RECONCILED_MODEL_AUTH"), nil
}

// restartGatewayPods deletes the gateway's pods so a changed
// openclaw.json takes effect — OpenClaw reads its config at process
// start, so an in-place PVC write is inert until the process restarts.
// Safe to call repeatedly only because its caller gates on an
// idempotent "config actually changed" signal.
func (r *AgentGatewayReconciler) restartGatewayPods(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{"agentoffice.ai/gateway": gw.Name},
	); err != nil {
		return err
	}
	for i := range pods.Items {
		if err := r.Delete(ctx, &pods.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
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
		if !ok {
			return nil
		}
		// Re-reconcile the agent's gateway when the AW changes — the
		// referenced one for shared, the minted per-agent one for
		// dedicated (so e.g. its MCP env-from-secrets get re-collected).
		gwRef := effectiveGatewayRef(aw)
		if gwRef == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Namespace: aw.Namespace, Name: gwRef,
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
