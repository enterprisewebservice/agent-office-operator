// Package templates renders OpenClaw config + workspace .md files from
// an AgentWorkstation spec. Ported from agent-office/backend/templates
// in slice 4 — the operator now owns this so a single AW reconcile is
// the only thing that touches the per-agent ConfigMap.
package templates

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

//go:embed openclaw.json.tmpl agents.md.tmpl identity.md.tmpl soul.md.tmpl user.md.tmpl tools.md.tmpl
var templateFS embed.FS

// ModelEntry is a model row in openclaw.json.
type ModelEntry struct {
	ID   string
	Name string
}

// OpenClawConfigData holds the data for rendering openclaw.json.
type OpenClawConfigData struct {
	ProviderName string
	BaseURL      string
	APIKeyRef    string
	Models       []ModelEntry
	DefaultModel string
	GatewayToken string
	// AllowedOrigins is the list of hostnames the OpenClaw Control
	// UI accepts cross-origin requests from. Computed by the
	// reconciler from the agent's Route hostname so the "Open agent
	// gateway" button (loaded from a different host) works.
	AllowedOrigins []string
}

// AgentsMdData holds data for rendering AGENTS.md.
type AgentsMdData struct {
	Name         string
	DisplayName  string
	SystemPrompt string
}

// IdentityMdData holds data for rendering IDENTITY.md.
type IdentityMdData struct {
	DisplayName string
	Emoji       string
}

// SoulMdData holds data for rendering SOUL.md.
type SoulMdData struct {
	DisplayName  string
	SystemPrompt string
}

// ToolsMdData holds data for rendering TOOLS.md.
type ToolsMdData struct {
	Tools []string
}

// RenderOpenClawConfig renders openclaw.json from the AW spec + a
// generated gateway token + the cluster's apps domain (used to
// compute the agent's Route hostname for allowedOrigins).
func RenderOpenClawConfig(aw *agentofficev1alpha1.AgentWorkstation, gatewayToken, appsDomain string) (string, error) {
	tmpl, err := template.ParseFS(templateFS, "openclaw.json.tmpl")
	if err != nil {
		return "", fmt.Errorf("parsing openclaw.json template: %w", err)
	}

	data := OpenClawConfigData{GatewayToken: gatewayToken}
	// Compute the canonical Route hostname for this agent so the
	// Control UI accepts cross-origin loads from there. Console
	// sessions launch the gateway at this host; without it on the
	// allowlist OpenClaw rejects with "origin not allowed".
	if appsDomain != "" {
		host := fmt.Sprintf("agent-%s-%s.%s", aw.Name, aw.Namespace, appsDomain)
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

	switch aw.Spec.Model.Provider {
	case agentofficev1alpha1.ModelProviderSMR:
		routerRef := aw.Spec.Model.ModelRouterRef
		if routerRef == "" {
			routerRef = "default"
		}
		data.ProviderName = "smr"
		data.BaseURL = fmt.Sprintf("http://smr-router-%s.%s.svc.cluster.local", routerRef, aw.Namespace)
		data.APIKeyRef = "${SMR_API_KEY}"
		data.Models = []ModelEntry{{ID: "auto", Name: "auto"}}
		data.DefaultModel = "auto"

	case agentofficev1alpha1.ModelProviderOpenAI:
		data.ProviderName = "openai"
		data.BaseURL = "https://api.openai.com/v1"
		data.APIKeyRef = "${OPENAI_API_KEY}"
		modelName := aw.Spec.Model.ModelName
		if modelName == "" || modelName == "auto" {
			modelName = "gpt-4o"
		}
		data.Models = []ModelEntry{{ID: modelName, Name: modelName}}
		data.DefaultModel = modelName

	case agentofficev1alpha1.ModelProviderAnthropic:
		data.ProviderName = "anthropic"
		data.BaseURL = "https://api.anthropic.com/v1"
		data.APIKeyRef = "${ANTHROPIC_API_KEY}"
		modelName := aw.Spec.Model.ModelName
		if modelName == "" || modelName == "auto" {
			modelName = "claude-sonnet-4-20250514"
		}
		data.Models = []ModelEntry{{ID: modelName, Name: modelName}}
		data.DefaultModel = modelName

	default:
		return "", fmt.Errorf("unknown provider: %s", aw.Spec.Model.Provider)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing openclaw.json template: %w", err)
	}
	return buf.String(), nil
}

// RenderAgentsMd, RenderIdentityMd, RenderSoulMd, RenderUserMd,
// RenderToolsMd render the corresponding workspace files from the AW
// spec. Each is a pure function so reconcile is deterministic.
func RenderAgentsMd(aw *agentofficev1alpha1.AgentWorkstation) (string, error) {
	return renderTemplate("agents.md.tmpl", AgentsMdData{
		Name:         aw.Name,
		DisplayName:  aw.Spec.DisplayName,
		SystemPrompt: aw.Spec.SystemPrompt,
	})
}

func RenderIdentityMd(aw *agentofficev1alpha1.AgentWorkstation) (string, error) {
	return renderTemplate("identity.md.tmpl", IdentityMdData{
		DisplayName: aw.Spec.DisplayName,
		Emoji:       aw.Spec.Emoji,
	})
}

func RenderSoulMd(aw *agentofficev1alpha1.AgentWorkstation) (string, error) {
	return renderTemplate("soul.md.tmpl", SoulMdData{
		DisplayName:  aw.Spec.DisplayName,
		SystemPrompt: aw.Spec.SystemPrompt,
	})
}

func RenderUserMd() (string, error) {
	return renderTemplate("user.md.tmpl", nil)
}

func RenderToolsMd(aw *agentofficev1alpha1.AgentWorkstation) (string, error) {
	tools := []string{}
	if aw.Spec.Tools != nil {
		tools = aw.Spec.Tools.Allow
	}
	return renderTemplate("tools.md.tmpl", ToolsMdData{Tools: tools})
}

func renderTemplate(name string, data interface{}) (string, error) {
	tmpl, err := template.ParseFS(templateFS, name)
	if err != nil {
		return "", fmt.Errorf("parsing %s template: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing %s template: %w", name, err)
	}
	return buf.String(), nil
}
