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

// DedicatedRuntime is the default runtime mode: the operator
// reconciles a per-AgentWorkstation Pod, ConfigMap, Secret, PVC,
// Service, and Route (slice-4 behavior). Suitable for agents that
// need a unique image, security context, or full isolation.
type DedicatedRuntime struct {
	// (no fields today; reserved for future per-AW dedicated-runtime
	// tunables like sidecar config, network policies, etc.)
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

// RuntimeSpec selects HOW this AgentWorkstation is materialized.
// Exactly one of `dedicated` or `shared` should be set. If both are
// omitted, the operator defaults to `dedicated{}` — preserving
// slice-4 behavior for AWs that pre-date this field.
type RuntimeSpec struct {
	// Dedicated runtime: own Pod, own PVC, own Service, own Route.
	// +optional
	Dedicated *DedicatedRuntime `json:"dedicated,omitempty"`

	// Shared runtime: logical openclaw agent inside the referenced
	// AgentGateway's pod, sharing the gateway's paired node-host
	// browser via a per-agent profile.
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

	// SystemPrompt defines the agent's behavior.
	SystemPrompt string `json:"systemPrompt"`

	// Model selects the LLM backend.
	Model ModelSpec `json:"model"`

	// Description is a brief description of the agent's purpose.
	// +optional
	Description string `json:"description,omitempty"`

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
