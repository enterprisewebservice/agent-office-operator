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

// MemoryModuleKind is the canonical filename / role a module's content plays
// inside the rendered per-agent ConfigMap. It maps to the Anthropic Skills
// Open Standard (`skill`) and the AAIF cross-tool conventions (`agents` ->
// AGENTS.md), as well as OpenClaw-internal kinds (`user`, `tools`,
// `identity`, `soul`).
//
// +kubebuilder:validation:Enum=agents;user;tools;identity;soul;skill;custom
type MemoryModuleKind string

const (
	MemoryModuleKindAgents   MemoryModuleKind = "agents"
	MemoryModuleKindUser     MemoryModuleKind = "user"
	MemoryModuleKindTools    MemoryModuleKind = "tools"
	MemoryModuleKindIdentity MemoryModuleKind = "identity"
	MemoryModuleKindSoul     MemoryModuleKind = "soul"
	MemoryModuleKindSkill    MemoryModuleKind = "skill"
	MemoryModuleKindCustom   MemoryModuleKind = "custom"
)

// ConfigMapSource references a ConfigMap whose data[key] holds the module's
// .md content. Typical pattern is a kustomize-generated CM in the same
// namespace as the MemoryModule.
type ConfigMapSource struct {
	// Name of the ConfigMap in the same namespace.
	Name string `json:"name"`

	// Key in the ConfigMap's data map (typically the canonical filename,
	// e.g. AGENTS.md, USER.md, SKILL_<name>.md).
	Key string `json:"key"`
}

// MemoryModuleSource is the location of the module's content. Exactly one of
// `configMapRef` or `inline` must be set.
type MemoryModuleSource struct {
	// Reference to a ConfigMap whose data[key] holds the module's .md
	// content. Use this for content that lives as a real .md file in git
	// (recommended for anything authored by humans).
	// +optional
	ConfigMapRef *ConfigMapSource `json:"configMapRef,omitempty"`

	// Inline .md content. For small or one-off modules where a separate
	// file in git would be overkill.
	// +optional
	Inline string `json:"inline,omitempty"`
}

// MemoryModuleSpec defines the desired state of a MemoryModule.
type MemoryModuleSpec struct {
	// Kind maps to the canonical filename the OpenClaw runtime expects in
	// /workspace/workspace, and to the AAIF / Linux Foundation cross-tool
	// conventions where applicable.
	Kind MemoryModuleKind `json:"kind"`

	// Filename the module's content lands as inside the rendered per-agent
	// ConfigMap. For `kind: agents` use `AGENTS.md` (AAIF universal). For
	// `kind: skill` use `SKILL_<name>.md` (Anthropic Skills Open Standard,
	// unpacked by the OpenClaw init container into skills/<name>/SKILL.md).
	// For OpenClaw-internal kinds (user/tools/identity/soul) use the
	// matching .md filename.
	Filename string `json:"filename"`

	// Semver version of the module content. Optional in v1alpha1; recorded
	// in status when present.
	// +optional
	Version string `json:"version,omitempty"`

	// Human-readable description of when to use this module.
	// +optional
	Description string `json:"description,omitempty"`

	// Source is where the module's content lives. Exactly one of
	// configMapRef or inline must be set.
	Source MemoryModuleSource `json:"source"`
}

// MemoryModuleCondition is a standard Kubernetes condition entry on the
// module's status.
type MemoryModuleCondition struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// MemoryModuleStatus reflects observed state. The operator's controller
// fills these fields on every reconcile.
type MemoryModuleStatus struct {
	// SHA-256 of the resolved content (the .md text the controller last
	// observed). Used by agents to detect "memory drift" and by the UI to
	// show shared vs. unique content across agents.
	// +optional
	ContentSHA256 string `json:"contentSha256,omitempty"`

	// AgentWorkstation names whose spec.memory.modules currently reference
	// this module. Maintained by the controller as agents are created and
	// deleted.
	// +optional
	ReferencedBy []string `json:"referencedBy,omitempty"`

	// Conditions reflect the latest available observations of the module's
	// state. Standard types: Ready, ContentResolved, RBACGranted.
	// +optional
	Conditions []MemoryModuleCondition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mm,categories=agent-office
// +kubebuilder:printcolumn:name="Kind",type="string",JSONPath=".spec.kind"
// +kubebuilder:printcolumn:name="Filename",type="string",JSONPath=".spec.filename"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".spec.version"
// +kubebuilder:printcolumn:name="ReferencedBy",type="integer",JSONPath=".status.referencedBy",description="Number of agents currently referencing this module"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MemoryModule is shared .md content that one or more AgentWorkstations
// pull into their per-agent rendered ConfigMap (AGENTS.md, USER.md,
// SKILL_<name>.md, etc.). Single source of truth for cross-agent memory.
type MemoryModule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MemoryModuleSpec   `json:"spec,omitempty"`
	Status MemoryModuleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MemoryModuleList contains a list of MemoryModule.
type MemoryModuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryModule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MemoryModule{}, &MemoryModuleList{})
}
