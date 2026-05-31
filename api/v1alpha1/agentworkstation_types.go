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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelProvider identifies which LLM backend an AgentWorkstation uses.
//
// +kubebuilder:validation:Enum=smr;anthropic;openai;openai-codex;custom
type ModelProvider string

const (
	// ModelProviderSMR routes through the in-cluster SmallModelRouter.
	ModelProviderSMR ModelProvider = "smr"
	// ModelProviderAnthropic talks directly to Anthropic's API.
	ModelProviderAnthropic ModelProvider = "anthropic"
	// ModelProviderOpenAI talks to OpenAI's pay-per-request API
	// using an OPENAI_API_KEY (rate-limited).
	ModelProviderOpenAI ModelProvider = "openai"
	// ModelProviderOpenAICodex routes through a ChatGPT/Codex
	// subscription via OAuth — uses your existing Pro/Team plan
	// quota instead of the API tier. Requires the gateway pod to
	// have ~/.codex/auth.json populated; set
	// AgentGateway.spec.codexCredentialsSecretRef so the operator
	// mounts your codex-subscription-credentials secret there.
	// OpenClaw natively syncs the file into its own auth-profiles
	// store on agent startup (see pi-ai
	// readCodexCliCredentials / syncExternalCliCredentialsForProvider).
	ModelProviderOpenAICodex ModelProvider = "openai-codex"
	// ModelProviderCustom is for any other endpoint.
	ModelProviderCustom ModelProvider = "custom"
)

// ModelSpec selects the LLM backend the agent uses.
type ModelSpec struct {
	// Provider type.
	Provider ModelProvider `json:"provider"`

	// ModelName is the specific model name, or "auto" for router-selected.
	// +kubebuilder:default=auto
	// +optional
	ModelName string `json:"modelName,omitempty"`

	// ModelRouterRef references a SmallModelRouter CR (used when provider
	// is smr).
	// +optional
	ModelRouterRef string `json:"modelRouterRef,omitempty"`
}

// OfficePosition is the (x,y) seat layout in the office Map view.
type OfficePosition struct {
	X int32 `json:"x,omitempty"`
	Y int32 `json:"y,omitempty"`
}

// OfficeSpec carries UI-only metadata that the office Map view renders.
type OfficeSpec struct {
	// +optional
	Position *OfficePosition `json:"position,omitempty"`
}

// ToolsSpec is the tool allowlist for an agent.
type ToolsSpec struct {
	// Allow is the list of tool names the agent is allowed to use.
	// +optional
	Allow []string `json:"allow,omitempty"`

	// MCPServers declares Model Context Protocol servers this agent
	// can call. Each entry becomes an `openclaw mcp set` invocation
	// against the agent's gateway pod, and any referenced credentials
	// Secret is envFrom'd onto the openclaw container with a
	// stakater/Reloader annotation so the pod rolls on Secret
	// rotation (e.g. ESO-rotated GitHub App installation tokens).
	//
	// Only meaningful when spec.runtime.shared.gatewayRef is set —
	// dedicated-runtime agents own their own pod and configure MCP
	// directly via their own manifest.
	//
	// Added in v1.4.0 alongside the MCP Gateway integration.
	// +optional
	MCPServers []MCPServerSpec `json:"mcpServers,omitempty"`
}

// MCPServerSpec declares one MCP server the agent talks to. The
// operator translates this into `openclaw mcp set <name> '<json>'`
// against the agent's gateway pod, plus optional envFrom + Reloader
// wiring on the gateway Deployment so rotated credentials are
// available as env-var refs in Headers.
type MCPServerSpec struct {
	// Name is the local identifier openclaw uses for this MCP server.
	// Tools from this server appear in openclaw as `<name>_<toolname>`
	// (matches the upstream MCPServerRegistration.spec.toolPrefix
	// convention used by the Kuadrant mcp-gateway operator).
	Name string `json:"name"`

	// URL is the HTTP/SSE endpoint of the MCP server. Typically a
	// cluster-internal Service DNS pointing at the Kuadrant MCP
	// Gateway broker (which then federates to backend MCP servers).
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`

	// Type selects the MCP transport. "http" is the modern Streamable
	// HTTP transport (mcp/2025-06-18 spec, what github-mcp-server
	// and the Kuadrant MCP Gateway speak). "sse" is the legacy
	// server-sent-events transport. stdio MCP servers (defined as
	// `{"command":...,"args":...}` in openclaw's native shape) are
	// not currently expressible via this CRD — use `openclaw mcp set`
	// directly inside the gateway pod for those.
	// +kubebuilder:default=http
	// +kubebuilder:validation:Enum=http;sse
	// +optional
	Type string `json:"type,omitempty"`

	// Headers is the static request-header map sent on every MCP
	// call. Values support `${ENV_VAR}` interpolation that openclaw
	// resolves at request time from the gateway pod's environment.
	// Typical usage with a rotating GitHub App installation token:
	//   headers:
	//     Authorization: "Bearer ${GITHUB_PERSONAL_ACCESS_TOKEN}"
	// paired with EnvFromSecret pointing at the ESO-rotated Secret.
	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// EnvFromSecret names a Secret in the AgentGateway's namespace
	// whose keys are projected into the openclaw container via
	// envFrom. Required when Headers contains any `${ENV_VAR}`
	// references. The operator also patches the gateway Deployment
	// with a `secret.reloader.stakater.com/reload` annotation
	// naming this Secret, so stakater/Reloader bounces the pod
	// whenever the Secret content changes (kubelet's mounted-secret
	// refresh doesn't cover env-var consumers — openclaw reads
	// ${VAR} at startup, not per-request).
	// +optional
	EnvFromSecret string `json:"envFromSecret,omitempty"`
}

// MemoryModuleRef is a reference to a MemoryModule CR consumed by this
// agent. Resolved by the operator at render time.
type MemoryModuleRef struct {
	// Name of the MemoryModule (cluster-namespaced; same namespace as the
	// AgentWorkstation).
	Name string `json:"name"`
}

// MemorySpec lists the shared MemoryModule CRs this agent pulls into its
// rendered per-agent ConfigMap (AGENTS.md, USER.md, SKILL_<name>.md, etc.)
// plus optional per-agent overrides.
type MemorySpec struct {
	// Modules referenced by this agent. The operator renders the union of
	// the modules' content into the agent's per-agent ConfigMap, ordered
	// by kind (AAIF cascade: agents -> user -> tools -> skill).
	// +optional
	Modules []MemoryModuleRef `json:"modules,omitempty"`

	// Overrides is inline .md content that takes precedence over any
	// referenced module's content for the same kind/filename. Useful for
	// agent-specific tweaks without forking a shared module.
	// +optional
	Overrides map[string]string `json:"overrides,omitempty"`
}

// DiscordChannelSpec configures the Discord channel/server the agent
// posts to. The URL field is the link the Console card's "Open in
// Discord" button uses — Discord channel URL
// (`https://discord.com/channels/<guild>/<channel>`), invite link
// (`https://discord.gg/<code>`), or guild URL all work.
type DiscordChannelSpec struct {
	// URL the "Open in Discord" Console button opens.
	// +optional
	URL string `json:"url,omitempty"`
}

// DedicatedRuntime is DEPRECATED and ignored by the operator as of
// v1.6.6. The old per-AgentWorkstation Pod path it selected was removed
// in the runtime unify: every agent now runs as a persona on an
// AgentGateway named by runtime.shared.gatewayRef, and "dedicated" is a
// packaging choice in the scaffolder template (the agent's gitops repo
// declares its own AgentGateway and points gatewayRef at it). Setting
// runtime.dedicated has no effect — set runtime.shared.gatewayRef
// instead. Kept only so manifests that still carry the field validate.
type DedicatedRuntime struct {
	// (deprecated; no fields)
}

// SharedRuntime slots this AgentWorkstation into an existing
// AgentGateway as a logical openclaw agent. The operator does NOT
// create a Pod/PVC/Service/Route for this AW — instead it calls
// `openclaw agents create <aw.name>` against the referenced gateway,
// and registers `browserProfile` in the gateway's node-host
// `browser.profiles[*]` allowlist so this agent gets its own
// isolated Chromium user-data-dir on the shared browser VM.
//
// This is the OpenClaw-doc'd multi-agent pattern: many openclaw
// agents inside one gateway runtime, one paired node-host, one
// browser-profile per agent.
type SharedRuntime struct {
	// GatewayRef is the name of the AgentGateway in the same
	// namespace that hosts this agent.
	GatewayRef string `json:"gatewayRef"`

	// BrowserProfile is the Chromium profile / user-data-dir name
	// to use for this agent on the shared node-host. Defaults to the
	// AW's own name if omitted.
	// +optional
	BrowserProfile string `json:"browserProfile,omitempty"`
}

// RuntimeSpec selects which AgentGateway this AgentWorkstation runs on.
// Set runtime.shared.gatewayRef — every agent (whether you think of it
// as "shared" or "dedicated") is a logical openclaw persona on a
// gateway; the operator has one path. A "dedicated" agent simply points
// gatewayRef at an AgentGateway its own gitops repo also declares.
type RuntimeSpec struct {
	// Dedicated is DEPRECATED and ignored as of v1.6.6 — see
	// DedicatedRuntime. Use Shared.GatewayRef.
	// +optional
	Dedicated *DedicatedRuntime `json:"dedicated,omitempty"`

	// Shared selects the AgentGateway this agent registers on as a
	// logical openclaw persona (its own per-agent browser profile).
	// +optional
	Shared *SharedRuntime `json:"shared,omitempty"`
}

// ChannelsSpec carries per-agent channel configuration. Today only
// Discord is wired into the Console UI; other channels live in
// openclaw.json on the PVC.
type ChannelsSpec struct {
	// +optional
	Discord *DiscordChannelSpec `json:"discord,omitempty"`
}

// AgentWorkstationSpec defines the desired state of an AgentWorkstation.
type AgentWorkstationSpec struct {
	// DisplayName is the human-readable name for this agent workstation.
	DisplayName string `json:"displayName"`

	// SystemPrompt defines the agent's behavior. Owned by the
	// AgentWorkstation author — the operator never edits this.
	SystemPrompt string `json:"systemPrompt"`

	// SystemPromptAddons is appended to SystemPrompt verbatim in the
	// rendered SOUL.md / AGENTS.md. Intended for operator- or
	// tooling-managed content that augments — but never replaces —
	// the author's prompt. Typical use today (set manually):
	//
	//   spec:
	//     systemPromptAddons: |
	//       # Knowledge Bases attached to this agent are listed in
	//       # ~/.openclaw/workspaces/<your-name>/KBS.md.
	//       # Read that file at the start of every conversation so
	//       # your tool selection cites the right KBs by name.
	//
	// Future operator versions may auto-populate this field when
	// related state changes (e.g. spec.knowledgeBaseRefs is set);
	// the field stays user-editable so manual additions are
	// possible. Empty by default.
	//
	// Added in v1.5.2 to support the AgentWorkstation Bindings
	// plugin: when the plugin attaches KBs/Skills/MemoryModules to
	// an agent, it can also append the matching prompt-awareness
	// snippet here so the agent actually USES the new bindings
	// without the author hand-editing systemPrompt every time.
	// +optional
	SystemPromptAddons string `json:"systemPromptAddons,omitempty"`

	// Model selects the LLM backend.
	Model ModelSpec `json:"model"`

	// Description is a brief description of the agent's purpose.
	// Added value in v1.3.0: the PM Agent reads this when inventorying
	// the available agent pool during project intake — it's the
	// human-readable "what does this agent do" that helps PM decide
	// which existing agent (if any) maps to a task in a new project.
	// Keep it dense and concrete; PM ingests these as one-liners.
	// +optional
	Description string `json:"description,omitempty"`

	// Role identifies the agent's position in the larger
	// orchestration. Open-set string the PM Agent uses to route
	// tasks: "pm" (the orchestrator itself), "doc-ingestor",
	// "rag-indexer", "autoresearch-runner", "data-curator",
	// "code-generator", "web-researcher", etc.
	//
	// The operator does NOT validate or enforce specific values —
	// roles are a discovery axis, not a constraint. PM picks an
	// agent by exact role match (with capabilities as a tiebreaker)
	// and falls back to "no agent for this role" pushback if no
	// match exists. Empty role = "untyped" = PM will ask the human
	// what to do with it rather than auto-assigning.
	//
	// Added in v1.3.0 (PM-as-collaborator design slice 1).
	// +optional
	Role string `json:"role,omitempty"`

	// Capabilities is the open-set list of skills/affordances this
	// agent has beyond what its Role implies. Examples:
	// "pdf-parsing", "ocr-en", "docling", "sdxl-lora-train",
	// "web-fetch", "browser-automation".
	//
	// PM uses this as a tiebreaker when multiple agents declare the
	// same Role, and as a feasibility hint when planning ("this
	// task needs `docling`; do any agents declare it?"). Like Role,
	// capabilities are open-set and unvalidated — vocabulary
	// discipline is a team norm, not enforced by the operator.
	//
	// Added in v1.3.0.
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`

	// Emoji icon for the agent in the office view.
	// +optional
	Emoji string `json:"emoji,omitempty"`

	// Image is the container image for the agent runtime.
	// +kubebuilder:default="quay.io/aicatalyst/openclaw:latest"
	// +optional
	Image string `json:"image,omitempty"`

	// APIKeySecretRef is the name of a Secret containing the API key
	// (key "api-key").
	// +optional
	APIKeySecretRef string `json:"apiKeySecretRef,omitempty"`

	// Tools is the tool allowlist.
	// +optional
	Tools *ToolsSpec `json:"tools,omitempty"`

	// Memory lists shared MemoryModule references and per-agent overrides.
	// +optional
	Memory *MemorySpec `json:"memory,omitempty"`

	// Office carries UI-only metadata for the office Map view.
	// +optional
	Office *OfficeSpec `json:"office,omitempty"`

	// Channels configures the channels the agent communicates on.
	// +optional
	Channels *ChannelsSpec `json:"channels,omitempty"`

	// Runtime selects how the operator materializes this agent. If
	// omitted, defaults to `dedicated` (slice-4 model: own Pod and
	// dependent K8s objects). Set `runtime.shared.gatewayRef` to
	// slot the agent into an existing AgentGateway as a logical
	// openclaw agent (multi-agent-in-one-pod model).
	// +optional
	Runtime *RuntimeSpec `json:"runtime,omitempty"`

	// Resources are the container resource requests/limits.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// KnowledgeBaseRefs attaches one or more KnowledgeBases to this
	// agent. The operator renders a per-agent KBS.md into the agent's
	// workspace describing each attached KB, its role (how the agent
	// should think about it), and the on-pod mount path.
	//
	// Today the KB's PVC mounts at the AgentGateway level (no change
	// to that — saves on volumes, prevents duplicate mounts when
	// multiple agents share a gateway and reference the same KB).
	// What's per-agent is the KBS.md curation: each AW lists ONLY
	// the KBs it actually wants to think about, with its
	// AW-specific role.
	//
	// Each ref must match a KnowledgeBase CR in the same namespace
	// whose spec.gatewayRef.name matches this agent's gateway. The
	// reconciler refuses cross-gateway refs and surfaces a status
	// condition explaining why.
	//
	// Added in v1.5.0 to support the Backstage "drag a KB onto an
	// agent" UX (the plugin writes to this field; the operator
	// handles materialization).
	//
	// Only supported on shared-runtime AWs today. Dedicated-runtime
	// agents (own pod) need a different mount-injection story —
	// deferred to v1.6.0.
	// +optional
	KnowledgeBaseRefs []KnowledgeBaseRef `json:"knowledgeBaseRefs,omitempty"`
}

// KnowledgeBaseRef attaches a KnowledgeBase to an AgentWorkstation
// with an agent-specific role hint.
type KnowledgeBaseRef struct {
	// Name of the KnowledgeBase CR in the same namespace. Must exist
	// and must target the agent's gateway via spec.gatewayRef.name.
	Name string `json:"name"`

	// Role hints to the agent HOW to think about this KB. Open-set
	// string; the operator does NOT enforce specific values. Suggested
	// vocabulary the agent system prompts can teach against:
	//   - planning-reference  : read at start of every conversation
	//   - knowledge-pool      : search on-demand via wiki-search Skill;
	//                           write new findings via wiki-write
	//   - experiment-history  : read when working on the related
	//                           AutoResearchProject or feature
	//   - runbook             : read when triggered on an operational
	//                           task
	//   - style-guide         : read when producing a specific kind of
	//                           output
	// The role is included verbatim in the per-agent KBS.md so the
	// agent can act on it. Empty string defaults to "knowledge-pool".
	// +optional
	Role string `json:"role,omitempty"`
}

// AgentWorkstationPhase is the high-level lifecycle phase the agent is in.
//
// +kubebuilder:validation:Enum=Pending;Creating;Running;Stopped;Error
type AgentWorkstationPhase string

const (
	AgentWorkstationPhasePending  AgentWorkstationPhase = "Pending"
	AgentWorkstationPhaseCreating AgentWorkstationPhase = "Creating"
	AgentWorkstationPhaseRunning  AgentWorkstationPhase = "Running"
	AgentWorkstationPhaseStopped  AgentWorkstationPhase = "Stopped"
	AgentWorkstationPhaseError    AgentWorkstationPhase = "Error"
)

// AgentWorkstationStatus reflects observed state.
type AgentWorkstationStatus struct {
	// +optional
	Phase AgentWorkstationPhase `json:"phase,omitempty"`

	// GatewayEndpoint is the public URL the agent listens on.
	// +optional
	GatewayEndpoint string `json:"gatewayEndpoint,omitempty"`

	// Message is a human-readable detail accompanying Phase.
	// +optional
	Message string `json:"message,omitempty"`

	// LastActivity records the last time the agent processed a request.
	// +optional
	LastActivity *metav1.Time `json:"lastActivity,omitempty"`

	// Conditions reflect the latest available observations of the agent's
	// state. Standard types: Ready, Available, Progressing.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=aw,categories=agent-office
// +kubebuilder:printcolumn:name="Display Name",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Provider",type="string",JSONPath=".spec.model.provider"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentWorkstation is a single governed coding-agent instance — the cluster
// representation of "one seat in the office". The operator owns its
// Deployment, Service, ConfigMap, PVC, and Secret references.
type AgentWorkstation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentWorkstationSpec   `json:"spec,omitempty"`
	Status AgentWorkstationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentWorkstationList contains a list of AgentWorkstation.
type AgentWorkstationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentWorkstation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentWorkstation{}, &AgentWorkstationList{})
}
