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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The openclaw.json converge logic is JavaScript embedded in a Go raw
// string and executed with `node -e` inside the gateway pod. Nothing
// compiles or type-checks it, so a syntax error or a logic slip ships
// silently and breaks EVERY reconcile — the failure mode is "the
// operator quietly stops converging", which is exactly the class of
// bug this feature exists to fix.
//
// These tests extract the REAL script out of the Go source and run it
// against fixture configs, so the thing under test is the code that
// actually ships.

// extractConvergeScript pulls the embedded converge script out of
// agentgateway_controller.go by locating the backtick-delimited raw
// string that carries the script body.
func extractConvergeScript(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("agentgateway_controller.go")
	if err != nil {
		t.Fatalf("read controller source: %v", err)
	}
	segments := strings.Split(string(src), "`")
	var found []string
	for _, seg := range segments {
		// The script body — not the Go code that inspects its output.
		if strings.Contains(seg, "RECONCILED_MODEL_AUTH") && strings.Contains(seg, "const fs = require") {
			found = append(found, seg)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 converge script literal, found %d "+
			"(the extraction heuristic needs updating if the script moved)", len(found))
	}
	return found[0]
}

// runConverge executes the real script against a temp openclaw.json.
// Returns the updated config and the script's stdout marker.
func runConverge(t *testing.T, script string, cfg map[string]interface{}, desired desiredOpenAIConfig) (map[string]interface{}, string) {
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

	// The shipped script hardcodes the in-pod config path; point it at
	// the fixture instead. Everything else runs verbatim.
	runnable := strings.Replace(script,
		`const p = "/home/node/.openclaw/openclaw.json";`,
		`const p = process.env.TEST_CFG_PATH;`, 1)
	if runnable == script {
		t.Fatal("could not redirect the config path; the script's `const p = ...` line changed")
	}
	// The ModelConnection bookkeeping sidecar is hardcoded too; point it
	// at the fixture dir so extraProviders cases can run outside a pod.
	runnable = strings.Replace(runnable,
		`const managedPath = "/home/node/.openclaw/.operator-managed-providers.json";`,
		`const managedPath = process.env.TEST_MANAGED_PATH;`, 1)

	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		t.Fatalf("marshal desired: %v", err)
	}

	cmd := exec.Command("node", "-e", runnable, string(desiredJSON))
	cmd.Env = append(os.Environ(), "TEST_CFG_PATH="+cfgPath,
		"TEST_MANAGED_PATH="+filepath.Join(dir, ".operator-managed-providers.json"), "OPENCLAW_GATEWAY_TOKEN=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run converge script: %v\noutput:\n%s", err, out)
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

// subscriptionDesired mirrors what reconcileModelProvidersAndAuth
// builds for a subscription gateway.
func subscriptionDesired() desiredOpenAIConfig {
	return desiredOpenAIConfig{
		Provider: map[string]interface{}{
			"baseUrl": "https://chatgpt.com/backend-api",
			"api":     "openai-chatgpt-responses",
			"models": []map[string]interface{}{
				{"id": "gpt-5.6-sol", "name": "GPT-5.6 Sol", "api": "openai-chatgpt-responses"},
			},
		},
		Profiles: map[string]map[string]string{
			"openai:me@example.com": {"provider": "openai", "mode": "oauth"},
		},
		Order:           map[string][]string{"openai": {"openai:me@example.com"}},
		AcceptableOrder: map[string][]string{"openai": {"openai:default", "openai:me@example.com"}},
	}
}

func mustNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping embedded-script test")
	}
}

func authOrder(t *testing.T, cfg map[string]interface{}) []string {
	t.Helper()
	auth, ok := cfg["auth"].(map[string]interface{})
	if !ok {
		return nil
	}
	order, ok := auth["order"].(map[string]interface{})
	if !ok {
		return nil
	}
	raw, ok := order["openai"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

func providers(t *testing.T, cfg map[string]interface{}) map[string]interface{} {
	t.Helper()
	models, ok := cfg["models"].(map[string]interface{})
	if !ok {
		t.Fatal("cfg has no models block")
	}
	p, ok := models["providers"].(map[string]interface{})
	if !ok {
		t.Fatal("cfg has no models.providers block")
	}
	return p
}

// TestConvergeScriptFixesTheBrokenGateway reproduces the exact broken
// config found on the live gateway — canonical `openai` pointing at the
// metered endpoint with an apiKey, subscription-only models stranded
// under the legacy `openai-codex` id, and no auth.order at all — and
// asserts the script repairs all three.
func TestConvergeScriptFixesTheBrokenGateway(t *testing.T) {
	mustNode(t)
	script := extractConvergeScript(t)

	broken := map[string]interface{}{
		"models": map[string]interface{}{
			"providers": map[string]interface{}{
				"openai": map[string]interface{}{
					"baseUrl": "https://api.openai.com/v1",
					"api":     "openai-completions",
					"apiKey":  "${OPENAI_API_KEY}",
				},
				"openai-codex": map[string]interface{}{
					"baseUrl": "https://chatgpt.com/backend-api",
					"api":     "openai-chatgpt-responses",
				},
			},
		},
	}

	updated, out := runConverge(t, script, broken, subscriptionDesired())
	if !strings.Contains(out, "RECONCILED_MODEL_AUTH") {
		t.Fatalf("expected a change to be reported, got: %s", out)
	}

	provs := providers(t, updated)
	if _, stillThere := provs["openai-codex"]; stillThere {
		t.Error("legacy openai-codex provider block was not deleted")
	}
	openai := provs["openai"].(map[string]interface{})
	if got := openai["baseUrl"]; got != "https://chatgpt.com/backend-api" {
		t.Errorf("baseUrl = %v, want the subscription endpoint", got)
	}
	if _, hasKey := openai["apiKey"]; hasKey {
		t.Error("apiKey survived into a subscription config; metered billing is still reachable")
	}
	if got := authOrder(t, updated); len(got) != 1 || got[0] != "openai:me@example.com" {
		t.Errorf("auth.order.openai = %v, want the pinned OAuth profile", got)
	}
}

// TestConvergeScriptIsIdempotent is the guard against a restart loop:
// the operator restarts the gateway whenever this script reports a
// change, so a converged config MUST report NO_CHANGE.
func TestConvergeScriptIsIdempotent(t *testing.T) {
	mustNode(t)
	script := extractConvergeScript(t)

	broken := map[string]interface{}{
		"models": map[string]interface{}{
			"providers": map[string]interface{}{
				"openai":       map[string]interface{}{"baseUrl": "https://api.openai.com/v1", "apiKey": "${OPENAI_API_KEY}"},
				"openai-codex": map[string]interface{}{"baseUrl": "https://chatgpt.com/backend-api"},
			},
		},
	}

	once, _ := runConverge(t, script, broken, subscriptionDesired())
	twice, out := runConverge(t, script, once, subscriptionDesired())
	if !strings.Contains(out, "NO_CHANGE") {
		t.Fatalf("second converge reported a change — this restart-loops the gateway. out=%s", out)
	}
	a, _ := json.Marshal(once)
	b, _ := json.Marshal(twice)
	if string(a) != string(b) {
		t.Errorf("config drifted on a no-op reconcile:\n once=%s\ntwice=%s", a, b)
	}
}

// TestConvergeScriptKeepsAnAlreadyValidPin covers the stickiness rule.
// OpenClaw can expose two OAuth profiles at once, and which are visible
// shifts over time. If the config already pins an acceptable one,
// rewriting it to our preferred pick would restart the gateway for no
// benefit — every reconcile, forever.
func TestConvergeScriptKeepsAnAlreadyValidPin(t *testing.T) {
	mustNode(t)
	script := extractConvergeScript(t)

	desired := subscriptionDesired()
	converged, _ := runConverge(t, script, map[string]interface{}{}, desired)

	// Simulate discovery having previously pinned the OTHER acceptable
	// profile id.
	auth := converged["auth"].(map[string]interface{})
	auth["order"].(map[string]interface{})["openai"] = []interface{}{"openai:default"}

	updated, out := runConverge(t, script, converged, desired)
	if got := authOrder(t, updated); len(got) != 1 || got[0] != "openai:default" {
		t.Errorf("auth.order.openai = %v, want the existing valid pin openai:default to be kept", got)
	}
	if strings.Contains(out, "RECONCILED_MODEL_AUTH") {
		t.Errorf("rewrote an already-valid pin, which restarts the gateway. out=%s", out)
	}
}

// TestConvergeScriptReplacesAnInvalidPin is the other half of
// stickiness: a pin that no longer satisfies the mode (say the profile
// was revoked) must be corrected, or the gateway stays broken forever.
func TestConvergeScriptReplacesAnInvalidPin(t *testing.T) {
	mustNode(t)
	script := extractConvergeScript(t)

	desired := subscriptionDesired()
	converged, _ := runConverge(t, script, map[string]interface{}{}, desired)
	auth := converged["auth"].(map[string]interface{})
	auth["order"].(map[string]interface{})["openai"] = []interface{}{"openai:revoked-profile"}

	updated, out := runConverge(t, script, converged, desired)
	if got := authOrder(t, updated); len(got) != 1 || got[0] != "openai:me@example.com" {
		t.Errorf("auth.order.openai = %v, want it corrected to the discovered profile", got)
	}
	if !strings.Contains(out, "RECONCILED_MODEL_AUTH") {
		t.Errorf("expected a change when the pin was invalid, out=%s", out)
	}
}

// keylessDesired mirrors reconcileModelProvidersAndAuth for a gateway
// that declares NO openai credential and whose agents ride a
// ModelConnection (v1.7.66): a keyless canonical block, the connection
// as an extra provider, and the default model pointed at it.
func keylessDesired() desiredOpenAIConfig {
	return desiredOpenAIConfig{
		Provider: map[string]interface{}{
			"baseUrl": "https://api.openai.com/v1",
			"api":     "openai-completions",
			"models":  []map[string]interface{}{{"id": "gpt-4o-mini", "name": "gpt-4o-mini"}},
		},
		Profiles:        map[string]map[string]string{},
		Order:           map[string][]string{},
		AcceptableOrder: map[string][]string{},
		ProfileMode:     "api_key",
		ExtraProviders: map[string]map[string]interface{}{
			"claude-work": {
				"baseUrl": "http://model-desk.model-desk.svc.cluster.local:4000/v1",
				"api":     "openai-completions",
				"apiKey":  "${MODELCONN_CLAUDE_WORK_API_KEY}",
				"models":  []map[string]interface{}{{"id": "claude-sonnet-5", "name": "Claude Sonnet 5"}},
			},
		},
		DefaultModel: "claude-work/claude-sonnet-5",
	}
}

// TestConvergeScriptKeylessGatewayRidesTheConnection is the fresh-seat
// regression: with no openai credential the canonical block must carry
// no ${OPENAI_API_KEY} (openclaw refuses to boot on an unresolvable
// secret ref), the connection block must land, and the seeded
// openai/* default must move to the connection.
func TestConvergeScriptKeylessGatewayRidesTheConnection(t *testing.T) {
	mustNode(t)
	script := extractConvergeScript(t)

	seeded := map[string]interface{}{
		"models": map[string]interface{}{
			"providers": map[string]interface{}{
				"openai": map[string]interface{}{
					"baseUrl": "https://chatgpt.com/backend-api",
					"api":     "openai-chatgpt-responses",
				},
			},
		},
		"agents": map[string]interface{}{
			"defaults": map[string]interface{}{"model": map[string]interface{}{"primary": "openai/gpt-5.6-sol"}},
		},
	}

	updated, out := runConverge(t, script, seeded, keylessDesired())
	if !strings.Contains(out, "RECONCILED_MODEL_AUTH") {
		t.Fatalf("expected a change to be reported, got: %s", out)
	}
	provs := providers(t, updated)
	openai := provs["openai"].(map[string]interface{})
	if _, hasKey := openai["apiKey"]; hasKey {
		t.Error("keyless gateway got an apiKey reference; openclaw would refuse to boot")
	}
	if _, ok := provs["claude-work"]; !ok {
		t.Error("connection provider block did not land")
	}
	primary := updated["agents"].(map[string]interface{})["defaults"].(map[string]interface{})["model"].(map[string]interface{})["primary"]
	if primary != "claude-work/claude-sonnet-5" {
		t.Errorf("defaults.model.primary = %v, want the connection", primary)
	}

	// An admin-chosen default on another provider is left alone.
	updated["agents"].(map[string]interface{})["defaults"].(map[string]interface{})["model"].(map[string]interface{})["primary"] = "litemaas/qwen"
	again, _ := runConverge(t, script, updated, keylessDesired())
	if got := again["agents"].(map[string]interface{})["defaults"].(map[string]interface{})["model"].(map[string]interface{})["primary"]; got != "litemaas/qwen" {
		t.Errorf("non-openai default was overridden to %v", got)
	}
}

func TestFirstConnectionModel(t *testing.T) {
	got := firstConnectionModel(map[string]map[string]interface{}{
		"zeta":        {"models": []map[string]interface{}{{"id": "z1"}}},
		"claude-work": {"models": []map[string]interface{}{{"id": "claude-sonnet-5"}, {"id": "claude-opus-4-6"}}},
		"empty":       {"models": []map[string]interface{}{}},
	})
	if got != "claude-work/claude-sonnet-5" {
		t.Errorf("firstConnectionModel = %q, want claude-work/claude-sonnet-5", got)
	}
	if got := firstConnectionModel(nil); got != "" {
		t.Errorf("firstConnectionModel(nil) = %q, want empty", got)
	}
}
