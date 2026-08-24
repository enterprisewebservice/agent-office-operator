// Package templates renders the base openclaw.json for an AgentGateway
// runtime pod. Since the v1.6.4 runtime unify, EVERY agent (shared or
// dedicated) runs as a logical openclaw persona inside a gateway pod,
// so the gateway config is the only thing the operator renders here —
// per-agent identity is appended to agents.list via `openclaw config
// set` during the AgentWorkstation reconcile, not rendered to a CM.
//
// The old per-AW renderers (RenderOpenClawConfig + the workspace .md
// files) were removed with the divergent dedicated Pod path they fed.
package templates

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"text/template"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

//go:embed agentgateway-openclaw.json.tmpl
var templateFS embed.FS

// CodexAuthSyncScript is the node program the codex-auth-sync sidecar
// runs: it mirrors the Secret-projected auth.json into the writable
// .codex/ emptyDir AND re-seeds any agent auth store that lost (or
// never had) a usable openai OAuth profile — the missing hop behind the
// 2026-08-23 upstreambeat outage, where OpenClaw pruned expired
// profiles and nothing put them back. Rendered into the gateway config
// CM (key codex-auth-sync.mjs) so the operator, not the image, owns it.
//
//go:embed codex-auth-sync.mjs
var CodexAuthSyncScript string

// HooksTokenEnvVar is the env var the operator exposes the hook token
// through: the gateway Deployment maps spec.hooks.tokenSecretRef onto
// it, and openclaw.json refers to it as HooksTokenRef. OpenClaw
// substitutes "${VAR}" in config strings at load time, so the literal
// token never lands in the ConfigMap, on the PVC, in exec arguments, or
// in the API server's audit log of those exec calls.
const HooksTokenEnvVar = "OPENCLAW_HOOKS_TOKEN"

// HooksTokenRef is the config value written for hooks.token.
const HooksTokenRef = "${" + HooksTokenEnvVar + "}"

// DefaultHooksPath is the hook endpoint prefix when spec.hooks.path is
// empty. OpenClaw rejects "/" outright, so a subpath is mandatory.
const DefaultHooksPath = "/hooks"

// HooksRender is the operator-owned slice of openclaw.json `hooks`, as
// decided for one reconcile. It is both what the template seeds into a
// NEW gateway and the contract handed to the in-pod converge script for
// an EXISTING one (whose openclaw.json is seed-only), so the two paths
// cannot drift.
type HooksRender struct {
	Enabled bool `json:"enabled"`
	// TokenEnvVar is the env var the token arrives through. The config
	// only ever carries "${<TokenEnvVar>}".
	TokenEnvVar string `json:"tokenEnvVar"`
	Path        string `json:"path"`
	// AllowedAgentIDs nil ⇒ not managed: an existing value in the
	// gateway config is left alone.
	AllowedAgentIDs        []string `json:"allowedAgentIds,omitempty"`
	AllowRequestSessionKey bool     `json:"allowRequestSessionKey"`
}

// HooksFromSpec maps spec.hooks onto the render contract. A nil spec
// yields nil: the operator keeps its hands off the hooks block
// entirely, so a gateway whose hooks were configured by hand keeps
// working until someone declares them.
func HooksFromSpec(h *agentofficev1alpha1.HooksSpec) *HooksRender {
	if h == nil {
		return nil
	}
	out := &HooksRender{
		Enabled:                h.Enabled,
		TokenEnvVar:            HooksTokenEnvVar,
		Path:                   h.Path,
		AllowRequestSessionKey: h.AllowRequestSessionKey,
	}
	if out.Path == "" {
		out.Path = DefaultHooksPath
	}
	if len(h.AllowedAgentIDs) > 0 {
		out.AllowedAgentIDs = append([]string(nil), h.AllowedAgentIDs...)
	}
	return out
}

// ConfigJSON renders the `hooks` object exactly as it appears in
// openclaw.json for an enabled render — token as the env reference,
// never a literal. Returns "" when hooks are not enabled: a fresh
// config with no hooks block IS hooks-disabled, so there is nothing to
// seed (turning hooks OFF on an existing gateway is the converge
// script's job, not the template's).
func (h *HooksRender) ConfigJSON() (string, error) {
	if h == nil || !h.Enabled {
		return "", nil
	}
	obj := struct {
		Enabled                bool     `json:"enabled"`
		Token                  string   `json:"token"`
		Path                   string   `json:"path"`
		AllowedAgentIDs        []string `json:"allowedAgentIds,omitempty"`
		AllowRequestSessionKey bool     `json:"allowRequestSessionKey"`
	}{
		Enabled:                true,
		Token:                  "${" + h.TokenEnvVar + "}",
		Path:                   h.Path,
		AllowedAgentIDs:        h.AllowedAgentIDs,
		AllowRequestSessionKey: h.AllowRequestSessionKey,
	}
	// Prefix matches the template's two-space indent so the block sits
	// flush with its siblings in the rendered file.
	b, err := json.MarshalIndent(obj, "  ", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal hooks block: %w", err)
	}
	return string(b), nil
}

// OpenClawConfigData holds the data for rendering the gateway's base
// openclaw.json.
type OpenClawConfigData struct {
	GatewayToken string
	// AllowedOrigins is the list of hostnames the OpenClaw Control UI
	// accepts cross-origin requests from. Computed by the reconciler
	// from the gateway's Route hostname so the "Open agent gateway"
	// button (loaded from a different host) works.
	AllowedOrigins []string
	// HooksJSON is the pre-rendered `hooks` object, or "" for none.
	HooksJSON string
}

// RenderAgentGatewayConfig renders the base openclaw.json for an
// AgentGateway runtime. agents.list and bindings start empty —
// per-AgentWorkstation reconcile appends to them via
// `openclaw config set`. hooks may be nil (no hooks block).
func RenderAgentGatewayConfig(gw *agentofficev1alpha1.AgentGateway, gatewayToken, appsDomain string, hooks *HooksRender) (string, error) {
	tmpl, err := template.ParseFS(templateFS, "agentgateway-openclaw.json.tmpl")
	if err != nil {
		return "", fmt.Errorf("parsing agentgateway-openclaw.json template: %w", err)
	}
	data := OpenClawConfigData{GatewayToken: gatewayToken}
	if appsDomain != "" {
		host := fmt.Sprintf("%s-%s.%s", gw.Name, gw.Namespace, appsDomain)
		data.AllowedOrigins = []string{
			fmt.Sprintf("https://%s", host),
			fmt.Sprintf("http://%s", host),
			"http://localhost:18789",
			"http://127.0.0.1:18789",
		}
	} else {
		data.AllowedOrigins = []string{
			"http://localhost:18789",
			"http://127.0.0.1:18789",
		}
	}
	if data.HooksJSON, err = hooks.ConfigJSON(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing agentgateway-openclaw.json template: %w", err)
	}
	return buf.String(), nil
}
