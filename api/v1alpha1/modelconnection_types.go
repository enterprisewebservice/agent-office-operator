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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelConnectionKind classifies how a connection reaches its model.
//
// +kubebuilder:validation:Enum=subscription;apiKey;endpoint
type ModelConnectionKind string

const (
	// ModelConnectionKindSubscription is a consumer-subscription auth
	// route (ChatGPT/Codex, Claude). The operator does NOT render these
	// into gateway provider blocks — subscription plumbing stays on
	// AgentGateway.spec.codexCredentialsSecretRef + modelAuth. The
	// connection object exists so the hiring UI can list the route and
	// gate who may pick it.
	ModelConnectionKindSubscription ModelConnectionKind = "subscription"
	// ModelConnectionKindAPIKey is a metered first-party API route
	// (api.openai.com with an API key). Like subscription, rendering
	// rides the existing gateway modelAuth path; the connection is a
	// permissioned menu entry.
	ModelConnectionKindAPIKey ModelConnectionKind = "apiKey"
	// ModelConnectionKindEndpoint is any OpenAI-compatible endpoint —
	// MaaS, LiteLLM, vLLM. This is the kind the operator fully renders:
	// a models.providers.<name> block on every gateway whose agents
	// reference the connection, with the key delivered via a derived
	// per-gateway Secret and env expansion. Proven live 2026-08-31
	// against a LiteLLM front door before this type existed.
	ModelConnectionKindEndpoint ModelConnectionKind = "endpoint"
)

// ConnectionSecretRef points at the Secret key holding the
// connection's API key. ModelConnection is cluster-scoped, so the
// reference names its namespace explicitly; keep these Secrets in the
// platform admin namespace — consumers never read them, the operator
// projects the value into a gateway-owned derived Secret.
type ConnectionSecretRef struct {
	// Name of the Secret.
	Name string `json:"name"`

	// Namespace the Secret lives in.
	// +kubebuilder:default=agent-office
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Key within the Secret.
	// +kubebuilder:default=api-key
	// +optional
	Key string `json:"key,omitempty"`
}

// ModelConnectionAccess declares who may see and pick this connection
// in the hiring UI. Enforced by the Developer Hub plugin against the
// logged-in user's identity (user.entity.spec.memberOf); the operator
// treats it as data. The hard boundary is Secret placement: connection
// credentials live only in the admin namespace and are projected by
// the operator, so a hand-written CR referencing a connection the
// author cannot see yields a provider block only if the operator
// agrees to render it — never direct Secret access.
type ModelConnectionAccess struct {
	// Groups whose members may use this connection (matched against
	// the identity provider's group claims, e.g. `cluster-admins`,
	// `attendees`).
	// +optional
	Groups []string `json:"groups,omitempty"`

	// Users allowed individually, by username.
	// +optional
	Users []string `json:"users,omitempty"`
}

// ModelConnectionSpec is a reusable, admin-published model route:
// "a brain the platform offers", with visibility rules. Workstations
// reference one by name via spec.model.connectionRef.
type ModelConnectionSpec struct {
	// DisplayName is the label the hiring UI shows on the picker.
	DisplayName string `json:"displayName"`

	// Description is the one-liner under the label.
	// +optional
	Description string `json:"description,omitempty"`

	// Kind classifies the route. Only `endpoint` connections are
	// rendered into gateway provider blocks by the operator.
	Kind ModelConnectionKind `json:"kind"`

	// Provider names the legacy provider preset for subscription and
	// apiKey kinds (the hiring flow maps those onto the existing
	// AgentWorkstation provider path). Ignored for endpoint kind.
	// +optional
	Provider ModelProvider `json:"provider,omitempty"`

	// BaseURL of the OpenAI-compatible endpoint (endpoint kind).
	// +optional
	BaseURL string `json:"baseUrl,omitempty"`

	// API is the OpenClaw wire dialect for the endpoint.
	// +kubebuilder:default=openai-completions
	// +optional
	API string `json:"api,omitempty"`

	// APIKeySecretRef locates the endpoint's API key (endpoint kind).
	// +optional
	APIKeySecretRef *ConnectionSecretRef `json:"apiKeySecretRef,omitempty"`

	// Models this connection offers. The first entry is the default
	// when a workstation names no model.
	// +optional
	Models []ModelCatalogEntry `json:"models,omitempty"`

	// KeyStrategy: `shared` uses the referenced key for every
	// consumer; `perSeat` marks the connection for per-consumer key
	// minting (budget/TTL) — recorded now, minted by the seat
	// tooling, not yet by the operator.
	// +kubebuilder:validation:Enum=shared;perSeat
	// +kubebuilder:default=shared
	// +optional
	KeyStrategy string `json:"keyStrategy,omitempty"`

	// Access declares who may pick this connection in the hiring UI.
	// Empty means admins only (the UI hides it from everyone else).
	// +optional
	Access *ModelConnectionAccess `json:"access,omitempty"`
}

// ModelConnectionStatus reports resolution health.
type ModelConnectionStatus struct {
	// Conditions: `Ready` means the referenced Secret resolves (for
	// kinds that need one).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mconn
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Display Name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Base URL",type=string,JSONPath=`.spec.baseUrl`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelConnection is an admin-published, permissioned model route
// agents can be hired onto.
type ModelConnection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelConnectionSpec   `json:"spec,omitempty"`
	Status ModelConnectionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelConnectionList contains a list of ModelConnection.
type ModelConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelConnection `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelConnection{}, &ModelConnectionList{})
}
