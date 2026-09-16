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
	"reflect"
	"strings"
	"testing"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

const (
	testGovToken  = "gho_rotatedTokenValue0123456789abcdef"
	testNewsToken = "nwsTokenValue-0123456789"
)

// runMCPDrift executes the shipped mcpDriftScript against a fixture
// openclaw.json (raw bytes, so corrupt files can be tested) with the
// given pod env, and returns the raw stdout.
func runMCPDrift(t *testing.T, cfg []byte, desired map[string]map[string]interface{}, env map[string]string) string {
	t.Helper()
	mustNode(t)
	cfgPath := filepath.Join(t.TempDir(), "openclaw.json")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	script := strings.Replace(mcpDriftScript,
		`const CONFIG_PATH = "/home/node/.openclaw/openclaw.json";`,
		`const CONFIG_PATH = process.env.TEST_CFG_PATH;`, 1)
	if script == mcpDriftScript {
		t.Fatal("could not redirect the config path; the script's CONFIG_PATH line changed")
	}
	in, err := json.Marshal(desired)
	if err != nil {
		t.Fatalf("marshal desired: %v", err)
	}
	cmd := exec.Command("node", "-e", script)
	cmd.Env = []string{"TEST_CFG_PATH=" + cfgPath}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = strings.NewReader(string(in))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run drift script: %v\n%s", err, out)
	}
	return string(out)
}

func liveConfig(t *testing.T, servers map[string]interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{
		"meta":    map[string]interface{}{"lastTouchedAt": "2026-09-16T15:43:06.000Z"},
		"gateway": map[string]interface{}{"mode": "local"},
		"mcp":     map[string]interface{}{"servers": servers},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newsroomDesired() map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		"governed": renderMCPServerConfig(agentofficev1alpha1.MCPServerSpec{
			Name: "governed", URL: "http://mcp-gateway.agent-office.svc.cluster.local/mcp", Type: "http",
			EnvFromSecret: "github-mcp-installation-token",
			Headers:       map[string]string{"Authorization": "Bearer ${api_key}"},
		}, map[string][]byte{"api_key": []byte(testGovToken)}),
		"newsroom": renderMCPServerConfig(agentofficev1alpha1.MCPServerSpec{
			Name: "newsroom", URL: "http://newsroom-mcp.upstreambeat.svc.cluster.local/mcp",
			EnvFromSecret: "upstreambeat-newsroom-secrets",
			Headers:       map[string]string{"Authorization": "Bearer ${NEWSROOM_TOKEN}"},
		}, map[string][]byte{"NEWSROOM_TOKEN": []byte(testNewsToken)}),
	}
}

// liveFromDesired is the entry `openclaw mcp set` stores for a rendered
// config (the JSON round trip of what the operator sends).
func liveFromDesired(t *testing.T, d map[string]map[string]interface{}) map[string]interface{} {
	t.Helper()
	b, _ := json.Marshal(d)
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustDrift(t *testing.T, out string) map[string][]string {
	t.Helper()
	for _, secret := range []string{testGovToken, testNewsToken, "rotated-away-literal", "SECRET-IN-FILE"} {
		if strings.Contains(out, secret) {
			t.Fatalf("drift output leaked a credential: %q", out)
		}
	}
	d, err := parseMCPDrift(out)
	if err != nil {
		t.Fatalf("parse drift %q: %v", out, err)
	}
	return d
}

func TestRenderMCPServerConfig(t *testing.T) {
	got := renderMCPServerConfig(agentofficev1alpha1.MCPServerSpec{
		Name: "g", URL: "http://x/mcp", Type: "http", EnvFromSecret: "s",
		Headers: map[string]string{"Authorization": "Bearer ${api_key}", "X-Other": "${NOT_IN_SECRET}"},
	}, map[string][]byte{"api_key": []byte("tok-123456")})
	want := map[string]interface{}{
		"url": "http://x/mcp", "transport": "streamable-http",
		"headers": map[string]string{"Authorization": "Bearer tok-123456", "X-Other": "${NOT_IN_SECRET}"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("render:\n got %#v\nwant %#v", got, want)
	}
	got = renderMCPServerConfig(agentofficev1alpha1.MCPServerSpec{Name: "s", URL: "http://y/sse", Type: "sse"}, nil)
	if got["transport"] != "sse" || got["headers"] != nil {
		t.Fatalf("sse without headers: %#v", got)
	}
	// Unreadable Secret: refs stay for openclaw's env expansion.
	got = renderMCPServerConfig(agentofficev1alpha1.MCPServerSpec{Name: "g", URL: "http://x/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${api_key}"}}, nil)
	if h := got["headers"].(map[string]string); h["Authorization"] != "Bearer ${api_key}" || got["transport"] != "streamable-http" {
		t.Fatalf("unresolved refs must pass through: %#v", got)
	}
}

// The steady state that used to rewrite openclaw.json every few seconds:
// the live entries are exactly what the operator renders. Nothing to set.
func TestMCPDriftIdenticalIsNoOp(t *testing.T) {
	desired := newsroomDesired()
	out := runMCPDrift(t, liveConfig(t, liveFromDesired(t, desired)), desired, nil)
	if d := mustDrift(t, out); len(d) != 0 {
		t.Fatalf("identical entries must not drift, got %v", d)
	}
	// Key order in the file is irrelevant.
	reordered := map[string]interface{}{
		"newsroom": map[string]interface{}{"headers": map[string]interface{}{"Authorization": "Bearer " + testNewsToken},
			"transport": "streamable-http", "url": "http://newsroom-mcp.upstreambeat.svc.cluster.local/mcp"},
		"governed": map[string]interface{}{"transport": "streamable-http",
			"headers": map[string]interface{}{"Authorization": "Bearer " + testGovToken},
			"url":     "http://mcp-gateway.agent-office.svc.cluster.local/mcp"},
	}
	if d := mustDrift(t, runMCPDrift(t, liveConfig(t, reordered), desired, nil)); len(d) != 0 {
		t.Fatalf("key order must not matter, got %v", d)
	}
}

// A rotated credential is the change that MUST still land.
func TestMCPDriftRotatedCredentialIsSet(t *testing.T) {
	desired := newsroomDesired()
	live := liveFromDesired(t, desired)
	live["governed"].(map[string]interface{})["headers"] = map[string]interface{}{"Authorization": "Bearer rotated-away-literal"}
	d := mustDrift(t, runMCPDrift(t, liveConfig(t, live), desired, nil))
	if !reflect.DeepEqual(d, map[string][]string{"governed": {"headers"}}) {
		t.Fatalf("rotation must drift only governed/headers, got %v", d)
	}
}

// OpenClaw keeps an authored ${VAR} when the pod env resolves it to the
// incoming literal, so that entry is already what a set would store.
func TestMCPDriftEnvRefResolvingToLiteralIsCurrent(t *testing.T) {
	desired := newsroomDesired()
	live := liveFromDesired(t, desired)
	live["newsroom"].(map[string]interface{})["headers"] = map[string]interface{}{"Authorization": "Bearer ${NEWSROOM_TOKEN}"}
	cfg := liveConfig(t, live)

	if d := mustDrift(t, runMCPDrift(t, cfg, desired, map[string]string{"NEWSROOM_TOKEN": testNewsToken})); len(d) != 0 {
		t.Fatalf("a ref the env resolves to the desired literal is current, got %v", d)
	}
	// The pod started with an older token: a set is needed (and stores the literal).
	d := mustDrift(t, runMCPDrift(t, cfg, desired, map[string]string{"NEWSROOM_TOKEN": "rotated-away-literal"}))
	if !reflect.DeepEqual(d, map[string][]string{"newsroom": {"headers"}}) {
		t.Fatalf("a stale env value must drift, got %v", d)
	}
	// Unresolvable ref: only an identical desired ref matches it.
	d = mustDrift(t, runMCPDrift(t, cfg, desired, nil))
	if !reflect.DeepEqual(d, map[string][]string{"newsroom": {"headers"}}) {
		t.Fatalf("an unresolvable ref must drift against a literal, got %v", d)
	}
	unresolvedDesired := map[string]map[string]interface{}{"newsroom": renderMCPServerConfig(agentofficev1alpha1.MCPServerSpec{
		Name: "newsroom", URL: "http://newsroom-mcp.upstreambeat.svc.cluster.local/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${NEWSROOM_TOKEN}"}}, nil)}
	if d := mustDrift(t, runMCPDrift(t, cfg, unresolvedDesired, nil)); len(d) != 0 {
		t.Fatalf("an identical ref is current even when unresolvable, got %v", d)
	}
}

func TestMCPDriftStructuralChanges(t *testing.T) {
	desired := newsroomDesired()
	cases := map[string]struct {
		mutate func(live map[string]interface{})
		want   map[string][]string
	}{
		"missing server": {func(l map[string]interface{}) { delete(l, "newsroom") }, map[string][]string{"newsroom": {"missing"}}},
		"url changed": {func(l map[string]interface{}) {
			l["governed"].(map[string]interface{})["url"] = "http://old/mcp"
		}, map[string][]string{"governed": {"url"}}},
		"transport changed": {func(l map[string]interface{}) {
			l["governed"].(map[string]interface{})["transport"] = "sse"
		}, map[string][]string{"governed": {"transport"}}},
		"legacy type key left behind": {func(l map[string]interface{}) {
			l["governed"].(map[string]interface{})["type"] = "http"
		}, map[string][]string{"governed": {"type"}}},
		"headers dropped live": {func(l map[string]interface{}) {
			delete(l["newsroom"].(map[string]interface{}), "headers")
		}, map[string][]string{"newsroom": {"headers"}}},
		"extra live header": {func(l map[string]interface{}) {
			l["newsroom"].(map[string]interface{})["headers"].(map[string]interface{})["X-Extra"] = "SECRET-IN-FILE"
		}, map[string][]string{"newsroom": {"headers"}}},
		"entry is not an object": {func(l map[string]interface{}) { l["governed"] = "SECRET-IN-FILE" }, map[string][]string{"governed": {"missing"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			live := liveFromDesired(t, desired)
			tc.mutate(live)
			if d := mustDrift(t, runMCPDrift(t, liveConfig(t, live), desired, nil)); !reflect.DeepEqual(d, tc.want) {
				t.Fatalf("got %v, want %v", d, tc.want)
			}
		})
	}
	// Desired without headers against a live entry that has them.
	plain := map[string]map[string]interface{}{"governed": {"url": "http://mcp-gateway.agent-office.svc.cluster.local/mcp", "transport": "streamable-http"}}
	if d := mustDrift(t, runMCPDrift(t, liveConfig(t, liveFromDesired(t, desired)), plain, nil)); !reflect.DeepEqual(d, map[string][]string{"governed": {"headers"}}) {
		t.Fatalf("live-only headers must drift, got %v", d)
	}
	// A config with no mcp section at all: everything is missing.
	d := mustDrift(t, runMCPDrift(t, []byte(`{"gateway":{"mode":"local"}}`), desired, nil))
	if len(d) != 2 || d["governed"][0] != "missing" || d["newsroom"][0] != "missing" {
		t.Fatalf("no mcp section: %v", d)
	}
}

// The 2026-09-15 failure: openclaw.json left as NUL bytes. The compare
// must refuse (no blind set) and must not quote the file.
func TestMCPDriftUnreadableConfigRefusesWithoutQuoting(t *testing.T) {
	desired := newsroomDesired()
	for name, cfg := range map[string][]byte{
		"nul bytes": make([]byte, 5043),
		"truncated": []byte(`{"mcp":{"servers":{"governed":{"headers":{"Authorization":"Bearer SECRET-IN-FILE`),
	} {
		t.Run(name, func(t *testing.T) {
			out := runMCPDrift(t, cfg, desired, nil)
			if strings.Contains(out, "SECRET-IN-FILE") || strings.Contains(out, testGovToken) {
				t.Fatalf("error output quoted the config: %q", out)
			}
			if _, err := parseMCPDrift(out); err == nil || !strings.Contains(err.Error(), "SyntaxError") {
				t.Fatalf("want a SyntaxError refusal, got %v (out %q)", err, out)
			}
		})
	}
	out := runMCPDrift(t, liveConfig(t, nil), desired, nil) // mcp.servers null
	if d := mustDrift(t, out); len(d) != 2 {
		t.Fatalf("null servers: %v", d)
	}
}

func TestParseMCPDrift(t *testing.T) {
	d, err := parseMCPDrift("some banner\n{\"ok\":true,\"drift\":{\"a\":[\"url\"]}}\n")
	if err != nil || !reflect.DeepEqual(d, map[string][]string{"a": {"url"}}) {
		t.Fatalf("last line: %v %v", d, err)
	}
	if _, err := parseMCPDrift(`{"ok":false,"error":"Error:ENOENT"}`); err == nil || !strings.Contains(err.Error(), "ENOENT") {
		t.Fatalf("refusal: %v", err)
	}
	_, err = parseMCPDrift("Bearer SECRET-IN-FILE")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("garbage must fail without echoing: %v", err)
	}
}

func TestScrubHeaderValues(t *testing.T) {
	cfg := map[string]interface{}{"headers": map[string]string{"Authorization": "Bearer " + testGovToken}}
	got := scrubHeaderValues("invalid: Bearer "+testGovToken+" and again "+testGovToken, cfg)
	if strings.Contains(got, testGovToken) {
		t.Fatalf("token survived: %q", got)
	}
	if scrubHeaderValues("Saved MCP server", map[string]interface{}{}) != "Saved MCP server" {
		t.Fatal("no headers must be a no-op")
	}
}
