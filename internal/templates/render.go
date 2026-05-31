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
	"fmt"
	"text/template"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

//go:embed agentgateway-openclaw.json.tmpl
var templateFS embed.FS

// OpenClawConfigData holds the data for rendering the gateway's base
// openclaw.json.
type OpenClawConfigData struct {
	GatewayToken string
	// AllowedOrigins is the list of hostnames the OpenClaw Control UI
	// accepts cross-origin requests from. Computed by the reconciler
	// from the gateway's Route hostname so the "Open agent gateway"
	// button (loaded from a different host) works.
	AllowedOrigins []string
}

// RenderAgentGatewayConfig renders the base openclaw.json for an
// AgentGateway runtime. agents.list and bindings start empty —
// per-AgentWorkstation reconcile appends to them via
// `openclaw config set`.
func RenderAgentGatewayConfig(gw *agentofficev1alpha1.AgentGateway, gatewayToken, appsDomain string) (string, error) {
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
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing agentgateway-openclaw.json template: %w", err)
	}
	return buf.String(), nil
}
