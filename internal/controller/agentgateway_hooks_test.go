/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
	"github.com/enterprisewebservice/agent-office-operator/internal/templates"
)

// --- the in-pod converge script, run for real under node ---------------

// runHooksConverge executes hooksConvergeScript against a temp
// openclaw.json. envToken controls whether OPENCLAW_HOOKS_TOKEN is
// present in the script's environment (the pod-has-the-env-var guard).
func runHooksConverge(t *testing.T, cfg map[string]interface{}, desired *templates.HooksRender, envToken string) (map[string]interface{}, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	runnable := strings.Replace(hooksConvergeScript,
		`const p = "/home/node/.openclaw/openclaw.json";`,
		`const p = process.env.TEST_CFG_PATH;`, 1)
	if runnable == hooksConvergeScript {
		t.Fatal("could not redirect the config path; the script's `const p = ...` line changed")
	}
	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		t.Fatalf("marshal desired: %v", err)
	}
	cmd := exec.Command("node", "-e", runnable, string(desiredJSON))
	cmd.Env = append(os.Environ(), "TEST_CFG_PATH="+cfgPath, templates.HooksTokenEnvVar+"="+envToken)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run hooks converge script: %v\noutput:\n%s", err, out)
	}
	updatedBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read updated cfg: %v", err)
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(updatedBytes, &updated); err != nil {
		t.Fatalf("parse updated cfg: %v", err)
	}
	return updated, strings.TrimSpace(string(out))
}

// handConfiguredGateway reproduces the live newsroom gateways as found
// on 2026-08-22: hooks enabled by hand with a LITERAL token on the PVC,
// plus unrelated keys that must survive.
func handConfiguredGateway() map[string]interface{} {
	return map[string]interface{}{
		"gateway": map[string]interface{}{"port": float64(18789), "auth": map[string]interface{}{"token": "gw-literal"}},
		"agents":  map[string]interface{}{"list": []interface{}{map[string]interface{}{"id": "nl2sql-wire"}}},
		"hooks": map[string]interface{}{
			"enabled":                true,
			"token":                  "literal-hook-token-from-config-set",
			"path":                   "/hooks",
			"allowedAgentIds":        []interface{}{"nl2sql-wire"},
			"allowRequestSessionKey": false,
			// Not operator-owned; a hand-added mapping must survive.
			"mappings": []interface{}{map[string]interface{}{"match": map[string]interface{}{"path": "gmail"}, "action": "agent"}},
		},
	}
}

func enabledHooks(ids ...string) *templates.HooksRender {
	return templates.HooksFromSpec(&agentofficev1alpha1.HooksSpec{
		Enabled:         true,
		TokenSecretRef:  &agentofficev1alpha1.HooksTokenSecretRef{Name: "newsroom-hooks"},
		AllowedAgentIDs: ids,
	})
}

func hooksOf(t *testing.T, cfg map[string]interface{}) map[string]interface{} {
	t.Helper()
	h, ok := cfg["hooks"].(map[string]interface{})
	if !ok {
		t.Fatalf("cfg has no hooks block: %v", cfg)
	}
	return h
}

// The migration case: a literal token set by `openclaw config set`
// becomes the env reference, and nothing else in the file moves.
func TestHooksConvergeScript_ReplacesLiteralTokenWithEnvReference(t *testing.T) {
	mustNode(t)
	updated, out := runHooksConverge(t, handConfiguredGateway(), enabledHooks("nl2sql-wire"), "tok")
	if !strings.Contains(out, "RECONCILED_HOOKS") {
		t.Fatalf("expected a change, got: %s", out)
	}
	h := hooksOf(t, updated)
	if h["token"] != templates.HooksTokenRef {
		t.Errorf("hooks.token = %v, want %s", h["token"], templates.HooksTokenRef)
	}
	if h["enabled"] != true || h["path"] != "/hooks" || h["allowRequestSessionKey"] != false {
		t.Errorf("managed keys wrong: %v", h)
	}
	if _, kept := h["mappings"]; !kept {
		t.Error("hooks.mappings (not operator-owned) was clobbered")
	}
	want := handConfiguredGateway()
	delete(want, "hooks")
	got := map[string]interface{}{}
	for k, v := range updated {
		if k != "hooks" {
			got[k] = v
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys outside hooks changed:\n got=%v\nwant=%v", got, want)
	}
	b, _ := json.Marshal(updated)
	if strings.Contains(string(b), "literal-hook-token") {
		t.Error("the literal token survived somewhere in the file")
	}
}

// Restart-loop guard: a converged file must report NO_CHANGE.
func TestHooksConvergeScript_IsIdempotent(t *testing.T) {
	mustNode(t)
	desired := enabledHooks("nl2sql-wire")
	once, _ := runHooksConverge(t, handConfiguredGateway(), desired, "tok")
	twice, out := runHooksConverge(t, once, desired, "tok")
	if !strings.Contains(out, "NO_CHANGE") {
		t.Fatalf("second converge reported a change — this restart-loops the gateway. out=%s", out)
	}
	a, _ := json.Marshal(once)
	b, _ := json.Marshal(twice)
	if string(a) != string(b) {
		t.Errorf("config drifted on a no-op reconcile:\n once=%s\ntwice=%s", a, b)
	}
}

// The old pod during a rollout has no OPENCLAW_HOOKS_TOKEN; writing the
// reference there would hand OpenClaw a config it cannot load.
func TestHooksConvergeScript_RefusesToWriteAnUnresolvableReference(t *testing.T) {
	mustNode(t)
	updated, out := runHooksConverge(t, handConfiguredGateway(), enabledHooks("nl2sql-wire"), "")
	if !strings.HasPrefix(out, "SKIP_NO_ENV") {
		t.Fatalf("expected SKIP_NO_ENV, got: %s", out)
	}
	if hooksOf(t, updated)["token"] != "literal-hook-token-from-config-set" {
		t.Error("file was modified despite the env var being absent")
	}
}

func TestHooksConvergeScript_OmittedAllowedAgentIdsKeepsExisting(t *testing.T) {
	mustNode(t)
	updated, _ := runHooksConverge(t, handConfiguredGateway(), enabledHooks(), "tok")
	if got := hooksOf(t, updated)["allowedAgentIds"]; !reflect.DeepEqual(got, []interface{}{"nl2sql-wire"}) {
		t.Errorf("allowedAgentIds = %v; an omitted list must leave the existing restriction alone", got)
	}
}

func TestHooksConvergeScript_DisableDropsOnlyOurReference(t *testing.T) {
	mustNode(t)
	disabled := templates.HooksFromSpec(&agentofficev1alpha1.HooksSpec{Enabled: false})

	// Our own reference goes away with enabled=false, so a pod without
	// the env var can still boot; the mapping is untouched.
	converged, _ := runHooksConverge(t, handConfiguredGateway(), enabledHooks("nl2sql-wire"), "tok")
	off, out := runHooksConverge(t, converged, disabled, "")
	if !strings.Contains(out, "RECONCILED_HOOKS") {
		t.Fatalf("expected a change when disabling, got: %s", out)
	}
	h := hooksOf(t, off)
	if h["enabled"] != false {
		t.Errorf("hooks.enabled = %v, want false", h["enabled"])
	}
	if _, still := h["token"]; still {
		t.Error("the ${OPENCLAW_HOOKS_TOKEN} reference must be dropped when disabling")
	}
	if _, kept := h["mappings"]; !kept {
		t.Error("hooks.mappings was clobbered by disable")
	}

	// A hand-set literal is not ours to delete.
	off2, _ := runHooksConverge(t, handConfiguredGateway(), disabled, "")
	if hooksOf(t, off2)["token"] != "literal-hook-token-from-config-set" {
		t.Error("a hand-set literal token was removed by disable")
	}
	if hooksOf(t, off2)["enabled"] != false {
		t.Error("disable did not turn hooks off")
	}
}

// A fresh gateway declared with hooks.enabled=false must not be
// restarted just to write `hooks: {enabled: false}`.
func TestHooksConvergeScript_DisabledOnConfigWithoutHooksIsANoop(t *testing.T) {
	mustNode(t)
	cfg := map[string]interface{}{"gateway": map[string]interface{}{"port": float64(18789)}}
	updated, out := runHooksConverge(t, cfg, templates.HooksFromSpec(&agentofficev1alpha1.HooksSpec{Enabled: false}), "")
	if !strings.Contains(out, "NO_CHANGE") {
		t.Fatalf("expected NO_CHANGE, got: %s", out)
	}
	if _, added := updated["hooks"]; added {
		t.Error("a hooks block was added to a config that had none")
	}
}

// --- resolveHooks against a fake API ---------------------------------

func hooksReconciler(t *testing.T, objs ...client.Object) *AgentGatewayReconciler {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := agentofficev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return &AgentGatewayReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		Scheme: s,
	}
}

func hooksGateway(spec *agentofficev1alpha1.HooksSpec) *agentofficev1alpha1.AgentGateway {
	return &agentofficev1alpha1.AgentGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "newsroom-gateway", Namespace: "agent-office", Generation: 7},
		Spec:       agentofficev1alpha1.AgentGatewaySpec{Hooks: spec},
	}
}

func hooksSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-office"},
		Data:       data,
	}
}

func hooksCondition(t *testing.T, gw *agentofficev1alpha1.AgentGateway) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(gw.Status.Conditions, agentofficev1alpha1.AgentGatewayConditionHooksReady)
}

func TestResolveHooks_UnsetSpecIsHandsOff(t *testing.T) {
	gw := hooksGateway(nil)
	gw.Status.Conditions = []metav1.Condition{{Type: agentofficev1alpha1.AgentGatewayConditionHooksReady, Status: metav1.ConditionTrue, Reason: "Stale"}}
	st, err := hooksReconciler(t).resolveHooks(context.Background(), gw)
	if err != nil {
		t.Fatal(err)
	}
	if st.render != nil || st.secretKey != nil {
		t.Errorf("unset spec must resolve to nothing, got %+v", st)
	}
	if hooksCondition(t, gw) != nil {
		t.Error("a stale HooksReady condition must be removed when spec.hooks is unset")
	}
}

func TestResolveHooks_SecretPresentEnablesHooks(t *testing.T) {
	gw := hooksGateway(&agentofficev1alpha1.HooksSpec{
		Enabled:         true,
		TokenSecretRef:  &agentofficev1alpha1.HooksTokenSecretRef{Name: "newsroom-hooks"},
		AllowedAgentIDs: []string{"nl2sql-wire"},
	})
	r := hooksReconciler(t, hooksSecret("newsroom-hooks", map[string][]byte{"token": []byte("s3cret")}))
	st, err := r.resolveHooks(context.Background(), gw)
	if err != nil {
		t.Fatal(err)
	}
	if st.render == nil || !st.render.Enabled {
		t.Fatalf("expected enabled render, got %+v", st.render)
	}
	if st.secretKey == nil || st.secretKey.Name != "newsroom-hooks" || st.secretKey.Key != "token" {
		t.Errorf("secretKey = %+v, want newsroom-hooks/token (default key)", st.secretKey)
	}
	c := hooksCondition(t, gw)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Enabled" || c.ObservedGeneration != 7 {
		t.Errorf("condition = %+v, want True/Enabled@7", c)
	}
	if strings.Contains(c.Message, "s3cret") {
		t.Error("the token value leaked into the status condition")
	}
}

func TestResolveHooks_MissingSecretDisablesWithReason(t *testing.T) {
	gw := hooksGateway(&agentofficev1alpha1.HooksSpec{
		Enabled:        true,
		TokenSecretRef: &agentofficev1alpha1.HooksTokenSecretRef{Name: "does-not-exist"},
	})
	st, err := hooksReconciler(t).resolveHooks(context.Background(), gw)
	if err != nil {
		t.Fatalf("a missing Secret is a condition, not an error: %v", err)
	}
	if st.render == nil || st.render.Enabled {
		t.Errorf("hooks must resolve DISABLED when the Secret is missing, got %+v", st.render)
	}
	if st.secretKey != nil {
		t.Error("no env var must be wired to a Secret that does not exist")
	}
	if c := hooksCondition(t, gw); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "SecretNotFound" {
		t.Errorf("condition = %+v, want False/SecretNotFound", c)
	}
}

func TestResolveHooks_EmptyKeyDisablesWithReason(t *testing.T) {
	gw := hooksGateway(&agentofficev1alpha1.HooksSpec{
		Enabled:        true,
		TokenSecretRef: &agentofficev1alpha1.HooksTokenSecretRef{Name: "newsroom-hooks", Key: "HOOKS_TOKEN"},
	})
	r := hooksReconciler(t, hooksSecret("newsroom-hooks", map[string][]byte{"token": []byte("wrong-key"), "HOOKS_TOKEN": {}}))
	st, err := r.resolveHooks(context.Background(), gw)
	if err != nil {
		t.Fatal(err)
	}
	if st.render.Enabled || st.secretKey != nil {
		t.Errorf("an empty key must disable hooks, got %+v / %+v", st.render, st.secretKey)
	}
	if c := hooksCondition(t, gw); c == nil || c.Reason != "SecretKeyMissing" {
		t.Errorf("condition = %+v, want SecretKeyMissing", c)
	}
}

func TestResolveHooks_ExplicitlyDisabled(t *testing.T) {
	gw := hooksGateway(&agentofficev1alpha1.HooksSpec{Enabled: false})
	st, err := hooksReconciler(t).resolveHooks(context.Background(), gw)
	if err != nil {
		t.Fatal(err)
	}
	if st.render == nil || st.render.Enabled || st.secretKey != nil {
		t.Errorf("explicit disable must still produce a (disabled) render so the converge turns hooks off: %+v", st)
	}
	if c := hooksCondition(t, gw); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Disabled" {
		t.Errorf("condition = %+v, want False/Disabled", c)
	}
}

// --- Deployment wiring ------------------------------------------------

func TestGatewayEnv_HooksTokenComesFromTheSecret(t *testing.T) {
	base := gatewayEnv(nil)
	if !reflect.DeepEqual(base, gatewayRuntimeEnv()) {
		t.Errorf("no hooks must mean the plain runtime env, got %+v", base)
	}
	env := gatewayEnv(&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "newsroom-hooks"}, Key: "token"})
	var found *corev1.EnvVar
	for i := range env {
		if env[i].Name == templates.HooksTokenEnvVar {
			found = &env[i]
		}
	}
	if found == nil {
		t.Fatalf("%s not in env: %+v", templates.HooksTokenEnvVar, env)
	}
	if found.Value != "" || found.ValueFrom == nil || found.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("the token must come from a secretKeyRef, never an inline value: %+v", found)
	}
	ref := found.ValueFrom.SecretKeyRef
	if ref.Name != "newsroom-hooks" || ref.Key != "token" {
		t.Errorf("secretKeyRef = %+v, want newsroom-hooks/token", ref)
	}
	if ref.Optional != nil && *ref.Optional {
		t.Error("the hooks token env var must not be optional; a missing Secret should fail loudly")
	}
}

func TestReloaderSecrets_MergesHooksSecretDeterministically(t *testing.T) {
	hooks := &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "newsroom-hooks"}, Key: "token"}
	if got := reloaderSecrets(nil, nil); got != "" {
		t.Errorf("nothing to watch must yield \"\", got %q", got)
	}
	if got := reloaderSecrets([]string{"zeta-mcp", "alpha-mcp"}, nil); got != "alpha-mcp,zeta-mcp" {
		t.Errorf("got %q, want sorted MCP secrets", got)
	}
	if got := reloaderSecrets([]string{"zeta-mcp", "newsroom-hooks"}, hooks); got != "newsroom-hooks,zeta-mcp" {
		t.Errorf("got %q, want deduplicated + sorted", got)
	}
	if got := reloaderSecrets(nil, hooks); got != "newsroom-hooks" {
		t.Errorf("got %q, want just the hooks Secret", got)
	}
}
