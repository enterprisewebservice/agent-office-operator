/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeHostRef references the KubeVirt VM (or a similar node-host
// resource) that the gateway pairs with. A single OpenClaw gateway
// pairs with exactly one node-host (per OpenClaw's doc'd model). The
// node-host hosts the real Chromium and is configured with one
// `browser.profiles[*]` entry per AgentWorkstation that uses this
// gateway, so each agent gets its own isolated user-data-dir.
type NodeHostRef struct {
	// Name of the node-host resource (today: KubeVirt VirtualMachine).
	Name string `json:"name"`
	// Namespace where the node-host lives. Defaults to the
	// AgentGateway's own namespace if omitted.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// AgentGatewaySpec defines the desired state of an AgentGateway —
// the heavy "runtime" Pod that hosts the OpenClaw gateway process.
// Multiple AgentWorkstations with `spec.runtime.shared.gatewayRef:
// <this-name>` slot in as logical openclaw agents inside this
// gateway's runtime, sharing the paired node-host's browser.
type AgentGatewaySpec struct {
	// DisplayName is the human-readable name (Console / RHDH).
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Description of what this gateway hosts (e.g. "Karpathy
	// auto-research crew").
	// +optional
	Description string `json:"description,omitempty"`

	// Image is the container image for the gateway runtime.
	// +kubebuilder:default="quay-quay-quay-test.apps.salamander.aimlworkbench.com/deanpeterson/openclaw:latest"
	// +optional
	Image string `json:"image,omitempty"`

	// SkillsImage overrides the OCI skills-catalog image mounted into
	// this gateway's pods — the image whose /skills/<name>/SKILL.md
	// folders are seeded into every agent's workspace. Bump this to roll
	// out a NEW VERSION of the skill catalog (rebuild + push the skills
	// image under a new tag, then set it here) without an operator
	// release: the operator re-creates the gateway pod, which pulls the
	// new tag and re-seeds the agents. Empty ⇒ the operator's built-in
	// default.
	// +optional
	SkillsImage string `json:"skillsImage,omitempty"`

	// NodeHostRef points at the paired node-host (KubeVirt VM today).
	// The node-host hosts the real browser; the gateway routes
	// browser tool calls through to it.
	// +optional
	NodeHostRef *NodeHostRef `json:"nodeHostRef,omitempty"`

	// SharedTokenSecretRef is the name of a Secret in the same
	// namespace that holds OPENCLAW_GATEWAY_TOKEN. Both the gateway
	// pod and the node-host process consume this — the operator
	// creates it on first reconcile if missing, then references it
	// from both ends so the pairing token never has to be
	// hand-synced again.
	// +optional
	SharedTokenSecretRef string `json:"sharedTokenSecretRef,omitempty"`

	// AutoApproveNodeHost makes the operator approve the configured
	// nodeHostRef's pairing request without human intervention.
	// OpenClaw v2026.4.x has no auto-approve config that works in a
	// k8s setting — the operator does the approval by writing
	// directly to the gateway PVC's ~/.openclaw/devices/paired.json
	// (the same shape `openclaw nodes approve` would produce) then
	// bouncing the gateway pod once so it reloads.
	//
	// Trade-off: any node-host that connects with a matching
	// displayName gets paired automatically. Safe in this cluster
	// because the gateway only listens on the in-cluster Service —
	// reachable by the trusted node-host VM, not the public
	// internet.
	// +kubebuilder:default=true
	// +optional
	AutoApproveNodeHost *bool `json:"autoApproveNodeHost,omitempty"`

	// Resources are the gateway pod's container resource
	// requests/limits.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// EnvFromSecretRef is the name of a Secret in the same namespace
	// whose keys are exposed as env vars on the gateway pod
	// (envFrom). The gateway's openclaw.json references those vars
	// (e.g. ${OPENAI_API_KEY}) so model-provider auth flows through
	// without per-agent auth-profiles.json. Typically points at a
	// Secret with OPENAI_API_KEY / ANTHROPIC_API_KEY entries.
	// +optional
	EnvFromSecretRef string `json:"envFromSecretRef,omitempty"`

	// CodexCredentialsSecretRef is the name of a Secret in the same
	// namespace whose `auth.json` key holds the ChatGPT/Codex
	// subscription OAuth tokens (the contents of ~/.codex/auth.json
	// on a developer's laptop after `codex login`). When set, the
	// operator mounts that file at /home/node/.codex/auth.json
	// inside the gateway pod. OpenClaw natively reads this file at
	// agent startup (pi-ai readCodexCliCredentials) and syncs it
	// into its auth-profiles store as the "openai-codex" provider —
	// no extra config needed. Use this instead of EnvFromSecretRef
	// when you want agents to consume your ChatGPT Pro/Team plan
	// quota via OAuth instead of the rate-limited pay-per-request
	// API tier (set the agent's spec.model.provider to
	// "openai-codex").
	// +optional
	CodexCredentialsSecretRef string `json:"codexCredentialsSecretRef,omitempty"`

	// ModelAuth pins WHICH stored credential OpenClaw uses for the
	// canonical `openai` provider — i.e. whether agent turns are
	// billed against your ChatGPT/Codex subscription (OAuth) or
	// against a pay-per-request API key.
	//
	// This exists because OpenClaw 2026.7.x collapsed `openai-codex`
	// into the canonical `openai` provider: one gateway config can
	// only describe ONE OpenAI route (one baseUrl + one credential
	// order), so the billing route is a gateway-level property, not a
	// per-agent one. Leaving it unset and relying on OpenClaw's
	// implicit precedence is what silently fell back to API-key
	// billing.
	//
	// Defaults to `subscription` when CodexCredentialsSecretRef is
	// set, otherwise `apiKey`.
	// +optional
	ModelAuth *ModelAuthSpec `json:"modelAuth,omitempty"`

	// AllowedUsers pre-approves channel senders so they skip the
	// "OpenClaw: access not configured" pairing prompt on first
	// contact. The operator merges these into the gateway's
	// `<channel>-<accountId>-allowFrom.json` (e.g.
	// `discord-default-allowFrom.json`). Idempotent — re-running a
	// reconcile with the same list is a no-op.
	//
	// Example: allow a Discord user to DM the bot directly:
	//
	//   allowedUsers:
	//     - channel: discord
	//       id: "745444500335231169"
	// +optional
	AllowedUsers []AllowedUser `json:"allowedUsers,omitempty"`

	// Hooks declares OpenClaw's webhook ingress (`hooks.*` in
	// openclaw.json): `POST <path>/wake` and `POST <path>/agent` on the
	// gateway's HTTP port, authenticated with a DEDICATED bearer token.
	// It is how an external system asks one specific agent for a
	// targeted turn — a newsroom's "Check now" button running the wire
	// reporter against a single source — without holding the gateway's
	// shared WebSocket token.
	//
	// The token never enters the rendered config. The referenced Secret
	// key is exposed to the gateway pod as the env var
	// OPENCLAW_HOOKS_TOKEN and openclaw.json carries the reference
	// "${OPENCLAW_HOOKS_TOKEN}", which OpenClaw substitutes at config
	// load — so the literal is not in the ConfigMap, not on the PVC,
	// not in exec arguments, and not in API audit logs.
	//
	// The operator owns exactly the keys this block declares (enabled,
	// token, path, allowedAgentIds, allowRequestSessionKey) and leaves
	// every other `hooks.*` key (mappings, presets, gmail, …) alone.
	// Unset ⇒ the operator does not touch the `hooks` block at all.
	// +optional
	Hooks *HooksSpec `json:"hooks,omitempty"`

	// HTTP exposes optional gateway HTTP surfaces beyond the core
	// WebSocket protocol.
	// +optional
	HTTP *GatewayHTTPSpec `json:"http,omitempty"`
}

// ModelAuthMode selects which credential the canonical `openai`
// provider is wired to.
//
// +kubebuilder:validation:Enum=subscription;apiKey
type ModelAuthMode string

const (
	// ModelAuthModeSubscription routes agent turns through your
	// ChatGPT/Codex plan: baseUrl https://chatgpt.com/backend-api,
	// api `openai-chatgpt-responses`, NO apiKey in config, and
	// auth.order pinned to the OAuth profile synced from
	// ~/.codex/auth.json. Flat-rate — no per-token charges.
	ModelAuthModeSubscription ModelAuthMode = "subscription"
	// ModelAuthModeAPIKey routes agent turns through the metered
	// OpenAI API: baseUrl https://api.openai.com/v1, api
	// `openai-completions`, apiKey ${OPENAI_API_KEY}. Billed per
	// token.
	ModelAuthModeAPIKey ModelAuthMode = "apiKey"
)

// ModelCatalogEntry is one model advertised to agents on the
// canonical `openai` provider.
type ModelCatalogEntry struct {
	// ID is the provider-side model id (e.g. "gpt-5.6-sol").
	ID string `json:"id"`
	// Name is the human-readable label. Defaults to ID.
	// +optional
	Name string `json:"name,omitempty"`
}

// ModelAuthSpec declares the gateway's model billing route and,
// optionally, an exact credential pin and model catalog.
type ModelAuthSpec struct {
	// Mode selects the billing route. Empty ⇒ `subscription` when
	// CodexCredentialsSecretRef is set, else `apiKey`.
	// +optional
	Mode ModelAuthMode `json:"mode,omitempty"`

	// ProfileID pins an exact OpenClaw auth-profile id (e.g.
	// "openai:you@example.com"). Empty ⇒ the operator discovers the
	// profile at reconcile time by asking the running gateway
	// (`openclaw models auth list --provider openai --json`) and
	// picking the first one whose type matches Mode. Discovery is
	// preferred: OAuth profile ids are derived from the account
	// email, so they differ per user and change if the account does.
	// +optional
	ProfileID string `json:"profileId,omitempty"`

	// Order is a full escape hatch: provider id ⇒ ordered auth
	// profile ids, written verbatim to openclaw.json `auth.order`.
	// Overrides Mode/ProfileID for any provider it names. Note
	// OpenClaw treats an explicit order as EXCLUSIVE — profiles not
	// listed are dropped from consideration entirely.
	// +optional
	Order map[string][]string `json:"order,omitempty"`

	// Models overrides the model catalog seeded onto the canonical
	// `openai` provider. Set this to advertise a newly released
	// model without waiting for an operator release (same escape
	// hatch as SkillsImage). Empty ⇒ the operator's built-in
	// defaults for the selected Mode.
	// +optional
	Models []ModelCatalogEntry `json:"models,omitempty"`
}

// AllowedUser is one entry in AgentGatewaySpec.AllowedUsers — a
// channel sender ID that's pre-approved to talk to the gateway.
type AllowedUser struct {
	// Channel is the OpenClaw channel plugin id (e.g. "discord",
	// "slack"). Defaults to "discord".
	// +kubebuilder:default=discord
	// +optional
	Channel string `json:"channel,omitempty"`

	// AccountID is the channel account scope. For Discord this is
	// the bot account; OpenClaw uses "default" when there's only
	// one. Defaults to "default".
	// +kubebuilder:default=default
	// +optional
	AccountID string `json:"accountId,omitempty"`

	// ID is the channel sender ID (e.g. a Discord user ID
	// snowflake). Required.
	ID string `json:"id"`
}

// HooksTokenSecretRef points at the Secret key holding the hook bearer
// token. The Secret must live in the AgentGateway's namespace.
type HooksTokenSecretRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key inside the Secret whose value is the token. Defaults to
	// "token".
	// +kubebuilder:default=token
	// +optional
	Key string `json:"key,omitempty"`
}

// GatewayHTTPSpec selects which optional HTTP surfaces the gateway
// serves on its port.
type GatewayHTTPSpec struct {
	// ChatCompletions serves the runtime's OpenAI-compatible surface
	// (/v1/chat/completions, /v1/models, /v1/responses) on the gateway
	// port, authenticated by the gateway token. A request runs the
	// same governed agent codepath as any other turn — routing,
	// permissions, and audit match the gateway. Off by default: the
	// token is an operator-grade credential, so enable this only for
	// gateways whose namespace RBAC already guards that Secret (for
	// example, in-namespace pipeline callers).
	// +optional
	ChatCompletions bool `json:"chatCompletions,omitempty"`
}

// HooksSpec declares the gateway's OpenClaw webhook ingress. See
// AgentGatewaySpec.Hooks for how the token reaches the pod.
//
// +kubebuilder:validation:XValidation:rule="!self.enabled || has(self.tokenSecretRef)",message="hooks.tokenSecretRef is required when hooks.enabled is true"
type HooksSpec struct {
	// Enabled turns the hook endpoints on. When false the operator
	// writes hooks.enabled=false and drops its own token reference, so
	// a gateway is never left pointing at an env var it no longer has
	// (OpenClaw refuses to load a config with an unresolvable
	// "${VAR}").
	Enabled bool `json:"enabled"`

	// TokenSecretRef names the Secret (same namespace) and key holding
	// the dedicated hook token. Required when Enabled. Use a value
	// distinct from OPENCLAW_GATEWAY_TOKEN — OpenClaw flags reuse as a
	// critical audit finding. If the Secret or key is missing or
	// empty, hooks are rendered DISABLED and the HooksReady condition
	// says why, rather than shipping a config the gateway cannot boot.
	// +optional
	TokenSecretRef *HooksTokenSecretRef `json:"tokenSecretRef,omitempty"`

	// Path is the URL prefix the hook endpoints hang off. Must be a
	// dedicated subpath — OpenClaw rejects "/". Defaults to "/hooks".
	// +kubebuilder:default="/hooks"
	// +kubebuilder:validation:Pattern=`^/.+`
	// +optional
	Path string `json:"path,omitempty"`

	// AllowedAgentIds restricts which agents a hook call may target,
	// including the default agent when the caller omits agentId.
	// Omitted ⇒ the operator leaves any existing hooks.allowedAgentIds
	// in the gateway config alone (OpenClaw's own default is
	// unrestricted). Set ["*"] to declare "unrestricted" explicitly.
	// +optional
	AllowedAgentIDs []string `json:"allowedAgentIds,omitempty"`

	// AllowRequestSessionKey lets /hooks/agent callers choose the
	// session key. Always written; false (the safe default) unless
	// set. If you enable it, also constrain
	// hooks.allowedSessionKeyPrefixes in the gateway config.
	// +optional
	AllowRequestSessionKey bool `json:"allowRequestSessionKey,omitempty"`
}

// AgentGatewayConditionHooksReady reports whether spec.hooks is in
// effect on the gateway: True once the token Secret resolved and the
// hooks block is rendered; False with a reason (Disabled,
// SecretNotFound, SecretKeyMissing, TokenSecretRefMissing) otherwise.
// Absent when spec.hooks is unset.
const AgentGatewayConditionHooksReady = "HooksReady"

// AgentGatewayPhase is the high-level lifecycle state.
//
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Error
type AgentGatewayPhase string

const (
	AgentGatewayPhasePending      AgentGatewayPhase = "Pending"
	AgentGatewayPhaseProvisioning AgentGatewayPhase = "Provisioning"
	AgentGatewayPhaseReady        AgentGatewayPhase = "Ready"
	AgentGatewayPhaseError        AgentGatewayPhase = "Error"
)

// AgentGatewayStatus reflects observed state.
type AgentGatewayStatus struct {
	// +optional
	Phase AgentGatewayPhase `json:"phase,omitempty"`

	// GatewayEndpoint is the public URL the gateway listens on
	// (Route URL).
	// +optional
	GatewayEndpoint string `json:"gatewayEndpoint,omitempty"`

	// NodeHostPaired reports whether the configured node-host has
	// successfully paired with the gateway.
	// +optional
	NodeHostPaired bool `json:"nodeHostPaired,omitempty"`

	// AgentCount is the number of AgentWorkstations currently
	// referencing this gateway via spec.runtime.shared.gatewayRef.
	// +optional
	AgentCount int32 `json:"agentCount,omitempty"`

	// Message is a human-readable detail for the current Phase.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions reflect the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag,categories=agent-office
// +kubebuilder:printcolumn:name="Display Name",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Node-Host",type="string",JSONPath=".spec.nodeHostRef.name"
// +kubebuilder:printcolumn:name="Paired",type="boolean",JSONPath=".status.nodeHostPaired"
// +kubebuilder:printcolumn:name="Agents",type="integer",JSONPath=".status.agentCount"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentGateway is a single OpenClaw gateway runtime that hosts one or
// more AgentWorkstations as logical openclaw agents. Multiple AWs
// with `spec.runtime.shared.gatewayRef: <this>` share this gateway's
// pod, paired node-host, and browser profiles. Per OpenClaw's
// doc'd multi-agent model.
type AgentGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentGatewaySpec   `json:"spec,omitempty"`
	Status AgentGatewayStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentGatewayList contains a list of AgentGateway.
type AgentGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentGateway `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentGateway{}, &AgentGatewayList{})
}
