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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// v1.7.77: `openclaw mcp set` only for servers whose live entry differs.
//
// `openclaw mcp set` never compares — it rewrites openclaw.json every
// time (setConfiguredMcpServer → replaceConfigFile, OpenClaw 2026.7.1),
// stamping meta.lastTouchedAt, rotating the .bak files, appending the
// whole argv (credentials included) to logs/config-audit.jsonl, and
// making the gateway re-evaluate its config. Called unconditionally
// from a reconcile that was itself looping, it rewrote the file every
// 3–7 seconds on the newsroom gateway; a node hang on 2026-09-15 caught
// a write mid-flight and left openclaw.json as 5,043 NUL bytes.
//
// So the reconcile first reads the live mcp.servers entries in the pod
// and sets only the ones that differ from the rendered definition.

// mcpDriftScript reads the desired mcp.servers entries as JSON on stdin
// and prints ONE JSON line naming the servers whose live entry differs,
// with the top-level field names that differ — never a value, because
// the headers carry literal credentials. It is a constant (argv holds
// no secret) and it reports only e.name/e.code on failure: a JSON.parse
// message quotes the text it choked on, which could be a token.
//
// A live string equals the desired literal when it is identical, or
// when it holds ${VAR} refs that the pod env resolves to that literal:
// OpenClaw keeps an authored ${VAR} on write in exactly that case
// (restoreEnvVarRefs), so a set would store what is already there.
// Keys present live but not rendered are drift too — a set replaces
// the whole entry, and the operator owns all of it.
const mcpDriftScript = `
const fs = require("fs");
const CONFIG_PATH = "/home/node/.openclaw/openclaw.json";
const emit = (o) => process.stdout.write(JSON.stringify(o) + "\n");
const has = (o, k) => Object.prototype.hasOwnProperty.call(o, k);
const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const REF = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g;
function resolvesTo(have, want) {
  if (typeof have !== "string" || typeof want !== "string" || !have.includes("${")) return false;
  let complete = true;
  const v = have.replace(REF, (m, k) => {
    if (has(process.env, k)) return process.env[k];
    complete = false;
    return m;
  });
  return complete && v === want;
}
function equal(want, have) {
  if (isObj(want)) {
    if (!isObj(have)) return false;
    const wk = Object.keys(want);
    return wk.length === Object.keys(have).length && wk.every((k) => has(have, k) && equal(want[k], have[k]));
  }
  if (Array.isArray(want)) {
    return Array.isArray(have) && have.length === want.length && want.every((v, i) => equal(v, have[i]));
  }
  return want === have || resolvesTo(have, want);
}
try {
  const desired = JSON.parse(fs.readFileSync(0, "utf8"));
  const cfg = JSON.parse(fs.readFileSync(CONFIG_PATH, "utf8"));
  const live = isObj(cfg) && isObj(cfg.mcp) && isObj(cfg.mcp.servers) ? cfg.mcp.servers : {};
  const drift = {};
  for (const [name, want] of Object.entries(desired)) {
    const have = has(live, name) ? live[name] : undefined;
    if (!isObj(have)) { drift[name] = ["missing"]; continue; }
    const fields = [];
    for (const k of Object.keys(want)) if (!has(have, k) || !equal(want[k], have[k])) fields.push(k);
    for (const k of Object.keys(have)) if (!has(want, k)) fields.push(k);
    if (fields.length) drift[name] = fields.sort();
  }
  emit({ ok: true, drift });
} catch (e) {
  emit({ ok: false, error: [e && e.name, e && e.code].filter(Boolean).join(":") || "Error" });
}
`

// renderMCPServerConfig builds the openclaw mcp.servers entry for srv.
// secretData is srv.EnvFromSecret's payload, or nil when there is none
// or it could not be read.
//
// The CRD's "http" means modern Streamable HTTP, but openclaw's "http"
// selects its legacy GET-first SSE client, which cannot talk to a
// Streamable-HTTP gateway (it blocks on the broker's silent GET stream
// or fatals on a 405, and the agent loads ZERO tools). openclaw's name
// for the POST-first transport is "streamable-http", so "http"/"" map
// to it and "sse" passes through. openclaw picks the client from
// "transport", NOT "type" (`else if (config.transport) transport =
// config.transport` in /app/dist) — writing "type" was silently ignored.
//
// v1.7.46: header credentials are CONFIG-delivered. A ${KEY} that
// resolves from secretData is rendered as the LITERAL value (see
// mcp_credentials.go); refs that don't resolve stay literal ${KEY} for
// openclaw's own env expansion (the env-delivery fallback path).
func renderMCPServerConfig(srv agentofficev1alpha1.MCPServerSpec, secretData map[string][]byte) map[string]interface{} {
	transport := "streamable-http"
	if srv.Type == "sse" {
		transport = "sse"
	}
	cfg := map[string]interface{}{
		"url":       srv.URL,
		"transport": transport,
	}
	headers := srv.Headers
	if resolved, n := resolveMCPHeaderCredentials(srv.Headers, secretData); n > 0 {
		headers = resolved
	}
	if len(headers) > 0 {
		cfg["headers"] = headers
	}
	return cfg
}

// mcpServersDrift runs mcpDriftScript in the gateway pod and returns
// server name → differing fields for every desired server that needs a
// set. desired travels on stdin, so its literal credentials never reach
// an argv, the config audit log, or an error string here.
func (r *AgentWorkstationReconciler) mcpServersDrift(
	ctx context.Context, pod *corev1.Pod, desired map[string]map[string]interface{},
) (map[string][]string, error) {
	in, err := json.Marshal(desired)
	if err != nil {
		return nil, fmt.Errorf("marshal desired mcp servers: %w", err)
	}
	out, err := r.execInPodStdin(ctx, pod, []string{"node", "-e", mcpDriftScript}, string(in))
	if err != nil {
		return nil, fmt.Errorf("compare mcp servers in %s: %w (%d bytes of output)", pod.Name, err, len(out))
	}
	return parseMCPDrift(out)
}

// parseMCPDrift reads the script's result line (the last one printed).
func parseMCPDrift(out string) (map[string][]string, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var res struct {
		OK    bool                `json:"ok"`
		Drift map[string][]string `json:"drift"`
		Error string              `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(lines[len(lines)-1])), &res); err != nil {
		return nil, fmt.Errorf("unreadable mcp compare result (%d bytes)", len(out))
	}
	if !res.OK {
		return nil, fmt.Errorf("gateway config unreadable, mcp servers not compared: %s", truncate(res.Error, 80))
	}
	return res.Drift, nil
}

// scrubHeaderValues removes the literal header values of cfg from s —
// whole, and each space-separated part, so a token quoted without its
// "Bearer " scheme goes too — so CLI output quoted in an error or a
// status message cannot carry them.
func scrubHeaderValues(s string, cfg map[string]interface{}) string {
	headers, _ := cfg["headers"].(map[string]string)
	for _, v := range headers {
		for _, part := range append([]string{v}, strings.Fields(v)...) {
			if len(part) >= 8 {
				s = strings.ReplaceAll(s, part, "<redacted>")
			}
		}
	}
	return s
}
