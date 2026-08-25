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
	"regexp"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// v1.7.46: MCP header credentials are CONFIG-delivered, not env-delivered.
//
// Before this, a `${VAR}` in spec.tools.mcpServers[].headers rode into the
// pod as env (envFrom of EnvFromSecret) and every rotation of that Secret
// rolled the gateway Deployment via envSecretsHash. Correct — but a pod
// roll per rotation is unusable for self-expiring tokens on a 30-minute
// refresh (GitHub App installation tokens, Gitea OAuth2 access tokens):
// the runtime would restart mid-conversation twice an hour.
//
// The gateway already has the right delivery channel: the operator
// rewrites openclaw.json on every reconcile (`openclaw mcp set`), and the
// OpenClaw runtime's config watcher classifies every `mcp.*` change as
// HOT (see /app/dist config-reload-plan: prefix "mcp" → kind "hot",
// action "dispose-mcp-runtimes" — verified live on openclaw v2026.7.1).
// A changed header value disposes the cached MCP client; the next tool
// call reconnects with the fresh credential. No process restart, no pod
// roll.
//
// So: when a server's headers reference `${KEY}` values that resolve from
// its EnvFromSecret, the operator renders the LITERAL values into the
// `openclaw mcp set` payload, and that Secret is dropped from the gateway
// Deployment's envFrom + envSecretsHash (no roll on rotation — the config
// write IS the delivery). Rotation propagates: Secret watch → AW
// reconcile → mcp set → OpenClaw hot reload.
//
// Fallback stays intact: a Secret whose keys are NOT referenced by the
// declaring server's headers (someone using envFromSecret as a plain env
// injector) keeps the v1.7.45 behavior — envFrom on the pod plus an
// envSecretsHash roll on rotation.

// mcpHeaderVarRef matches ${VAR} references in MCP server header values —
// the same shape OpenClaw itself expands from process env at MCP client
// construction time. Only simple env-var-ish names count; anything else
// is passed through untouched.
var mcpHeaderVarRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// resolveMCPHeaderCredentials returns a copy of headers with every
// `${KEY}` whose KEY exists in data replaced by the literal Secret value,
// plus the number of substitutions performed. References that don't
// resolve stay literal `${KEY}` so OpenClaw's own env expansion still
// applies to them (e.g. vars provided via spec.envFromSecretRef).
// A zero substitution count means the caller should use the original
// headers unchanged.
func resolveMCPHeaderCredentials(headers map[string]string, data map[string][]byte) (map[string]string, int) {
	if len(headers) == 0 || len(data) == 0 {
		return headers, 0
	}
	out := make(map[string]string, len(headers))
	n := 0
	for k, v := range headers {
		out[k] = mcpHeaderVarRef.ReplaceAllStringFunc(v, func(m string) string {
			key := m[2 : len(m)-1]
			if val, ok := data[key]; ok {
				n++
				return string(val)
			}
			return m
		})
	}
	return out, n
}

// mcpServerConsumesSecretViaConfig reports whether srv's declared
// EnvFromSecret is consumed through config rendering: at least one
// `${KEY}` in its headers names a key present in data. When true, the
// gateway reconciler must NOT envFrom that Secret for this server nor
// fold it into envSecretsHash — delivery happens through openclaw.json
// and rotation must not roll the pod.
func mcpServerConsumesSecretViaConfig(srv *agentofficev1alpha1.MCPServerSpec, data map[string][]byte) bool {
	if srv.EnvFromSecret == "" || len(srv.Headers) == 0 || len(data) == 0 {
		return false
	}
	for _, v := range srv.Headers {
		for _, m := range mcpHeaderVarRef.FindAllStringSubmatch(v, -1) {
			if _, ok := data[m[1]]; ok {
				return true
			}
		}
	}
	return false
}
