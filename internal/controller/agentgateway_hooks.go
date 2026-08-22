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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
	"github.com/enterprisewebservice/agent-office-operator/internal/templates"
)

// spec.hooks.tokenSecretRef names a Secret the operator did not create.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// defaultHooksTokenKey is the Secret key read when
// spec.hooks.tokenSecretRef.key is empty.
const defaultHooksTokenKey = "token"

// hooksState is what one reconcile decided about spec.hooks. It is
// computed once, up front, and threaded through the ConfigMap render,
// the Deployment (env var + Reloader annotation) and the in-pod
// converge, so the three cannot disagree within a pass — a config that
// references an env var the pod does not carry is a gateway that
// refuses to boot.
type hooksState struct {
	// render is the operator-owned hooks block; nil ⇒ spec.hooks is
	// unset and the operator keeps its hands off `hooks` entirely.
	render *templates.HooksRender
	// secretKey is the Secret key the token env var is wired to; nil
	// when hooks are disabled or the Secret did not resolve.
	secretKey *corev1.SecretKeySelector
}

// resolveHooks maps spec.hooks onto hooksState and records the
// HooksReady condition on the (in-memory) status.
//
// The token Secret is the one thing that can be wrong at runtime, and
// OpenClaw's answer to an unresolvable "${VAR}" is to not start. So
// when the Secret or key is missing or empty, hooks are resolved as
// DISABLED — the gateway keeps running, the condition says exactly what
// to fix — instead of the spec being rendered as written. Only a
// transient API error is returned as an error: turning hooks off on a
// blip would restart the gateway for nothing.
//
// The token's VALUE is never read beyond "is it non-empty", never
// logged, and never compared.
func (r *AgentGatewayReconciler) resolveHooks(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) (hooksState, error) {
	spec := gw.Spec.Hooks
	if spec == nil {
		meta.RemoveStatusCondition(&gw.Status.Conditions, agentofficev1alpha1.AgentGatewayConditionHooksReady)
		return hooksState{}, nil
	}
	st := hooksState{render: templates.HooksFromSpec(spec)}
	setCond := func(status metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               agentofficev1alpha1.AgentGatewayConditionHooksReady,
			Status:             status,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: gw.Generation,
		})
	}

	if !spec.Enabled {
		setCond(metav1.ConditionFalse, "Disabled", "spec.hooks.enabled is false")
		return st, nil
	}
	if spec.TokenSecretRef == nil || spec.TokenSecretRef.Name == "" {
		st.render.Enabled = false
		setCond(metav1.ConditionFalse, "TokenSecretRefMissing",
			"spec.hooks.tokenSecretRef is required when hooks are enabled; hooks stay disabled")
		return st, nil
	}
	name := spec.TokenSecretRef.Name
	key := spec.TokenSecretRef.Key
	if key == "" {
		key = defaultHooksTokenKey
	}

	var sec corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: name}, &sec)
	switch {
	case apierrors.IsNotFound(err):
		st.render.Enabled = false
		setCond(metav1.ConditionFalse, "SecretNotFound",
			fmt.Sprintf("Secret %q not found in namespace %s; hooks stay disabled until it exists", name, gw.Namespace))
		return st, nil
	case err != nil:
		return hooksState{}, fmt.Errorf("get hooks token Secret %s/%s: %w", gw.Namespace, name, err)
	}
	if len(sec.Data[key]) == 0 {
		st.render.Enabled = false
		setCond(metav1.ConditionFalse, "SecretKeyMissing",
			fmt.Sprintf("Secret %q has no non-empty key %q; hooks stay disabled", name, key))
		return st, nil
	}

	st.secretKey = &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
		Key:                  key,
	}
	agents := "any agent"
	if len(st.render.AllowedAgentIDs) > 0 {
		agents = "agents [" + strings.Join(st.render.AllowedAgentIDs, ", ") + "]"
	}
	setCond(metav1.ConditionTrue, "Enabled",
		fmt.Sprintf("hooks served at %s for %s; token from Secret %q key %q via $%s",
			st.render.Path, agents, name, key, templates.HooksTokenEnvVar))
	return st, nil
}

// hooksConvergeScript merges the operator-owned hooks.* keys into the
// gateway's live openclaw.json. It runs inside the pod with `node -e`
// and the desired templates.HooksRender as JSON in argv[1].
//
// Why a converge and not just the template: the init container is
// seed-only, so an existing PVC never sees a re-rendered ConfigMap.
// The same rule as model auth applies — read, mutate in memory,
// compare, write only on change — because the caller restarts the
// gateway when this reports a change, and an unconditional write would
// restart it every reconcile.
//
// Tested in agentgateway_hooks_test.go by running this exact constant
// under node; nothing else type-checks it.
const hooksConvergeScript = `
const fs = require("fs");
const p = "/home/node/.openclaw/openclaw.json";
const cfg = JSON.parse(fs.readFileSync(p, "utf8"));
const desired = JSON.parse(process.argv[1]);
const ref = "${" + desired.tokenEnvVar + "}";
const before = JSON.stringify(cfg);

// Merge, never replace: every hooks.* key the operator does not own
// (mappings, presets, gmail, defaultSessionKey, ...) survives as is.
const hooks = (cfg.hooks && typeof cfg.hooks === "object" && !Array.isArray(cfg.hooks)) ? cfg.hooks : {};

if (desired.enabled) {
  // Only write an env reference into a pod that can resolve it. The
  // Deployment carries the env var from this same reconcile, but this
  // script may be running in the OLD pod while the rollout is still in
  // flight, and OpenClaw refuses to load a config with an unresolvable
  // "${VAR}". The next pass, on the new pod, does the write.
  if (!process.env[desired.tokenEnvVar]) {
    console.log("SKIP_NO_ENV " + desired.tokenEnvVar);
    process.exit(0);
  }
  hooks.enabled = true;
  hooks.token = ref;
  hooks.path = desired.path;
  if (Array.isArray(desired.allowedAgentIds)) hooks.allowedAgentIds = desired.allowedAgentIds;
  hooks.allowRequestSessionKey = !!desired.allowRequestSessionKey;
} else {
  if (hooks.enabled) hooks.enabled = false;
  // Drop OUR reference so a pod without the env var can still boot. A
  // hand-set literal token is not ours to remove; with enabled=false it
  // is inert anyway.
  if (hooks.token === ref) delete hooks.token;
}
if (cfg.hooks !== undefined || Object.keys(hooks).length) cfg.hooks = hooks;

const after = JSON.stringify(cfg);
if (after === before) {
  console.log("NO_CHANGE");
} else {
  fs.writeFileSync(p, JSON.stringify(cfg, null, 2));
  console.log("RECONCILED_HOOKS");
}
`

// reconcileHooksConfig applies hooksState to the running gateway's
// openclaw.json. Returns true when the file changed — the caller must
// restart the gateway, since OpenClaw reads its config (and resolves
// "${OPENCLAW_HOOKS_TOKEN}") at process start.
func (r *AgentGatewayReconciler) reconcileHooksConfig(ctx context.Context, gw *agentofficev1alpha1.AgentGateway, st hooksState) (bool, error) {
	if st.render == nil {
		return false, nil
	}
	if r.RestConfig == nil {
		return false, fmt.Errorf("RestConfig not set; cannot exec")
	}
	pod, err := r.findReadyGatewayPod(ctx, gw)
	if err != nil {
		return false, err
	}
	desiredJSON, err := json.Marshal(st.render)
	if err != nil {
		return false, fmt.Errorf("marshal desired hooks config: %w", err)
	}
	// The desired document carries the env var NAME, never the token.
	out, err := r.execInGatewayPod(ctx, pod, []string{"node", "-e", hooksConvergeScript, string(desiredJSON)})
	if err != nil {
		return false, fmt.Errorf("reconcile hooks config: %w (out=%s)", err, out)
	}
	result := strings.TrimSpace(out)
	logf.FromContext(ctx).V(1).Info("hooks config reconcile",
		"enabled", st.render.Enabled, "path", st.render.Path, "result", result)
	return strings.Contains(result, "RECONCILED_HOOKS"), nil
}

// mapHooksSecretToGateways re-reconciles every AgentGateway in the
// Secret's namespace whose spec.hooks.tokenSecretRef names it, so a
// token Secret created after the CR (or recreated after a mistake)
// takes effect on the next pass rather than the next resync. The list
// is served from the cache, so this stays cheap even though Secrets
// churn (ESO refreshes every few minutes).
func (r *AgentGatewayReconciler) mapHooksSecretToGateways(ctx context.Context, obj client.Object) []reconcile.Request {
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	var gws agentofficev1alpha1.AgentGatewayList
	if err := r.List(ctx, &gws, client.InNamespace(sec.Namespace)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, gw := range gws.Items {
		h := gw.Spec.Hooks
		if h == nil || h.TokenSecretRef == nil || h.TokenSecretRef.Name != sec.Name {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: gw.Namespace, Name: gw.Name,
		}})
	}
	return out
}
