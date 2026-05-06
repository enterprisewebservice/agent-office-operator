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

// SkillSource is the location of the skill's primary `SKILL.md`
// content (Anthropic Skills Open Standard). Exactly one of
// `configMapRef` or `inline` must be set.
//
// Skills authored as real `.md` files in git should use
// `configMapRef` pointed at a kustomize-generated ConfigMap whose
// data[key] holds the markdown body. `inline` is for small or
// one-off skills where shipping a separate file in git is overkill.
type SkillSource struct {
	// Reference to a ConfigMap whose data[key] holds the skill's
	// `SKILL.md` content.
	// +optional
	ConfigMapRef *ConfigMapSource `json:"configMapRef,omitempty"`

	// Inline `SKILL.md` markdown body.
	// +optional
	Inline string `json:"inline,omitempty"`
}

// SkillInvocation describes how an agent invokes the skill at
// runtime. Both fields are optional and combine: `Tool` declares
// the underlying OpenClaw plugin tool the skill is built on, and
// `PromptTemplate` is rendered into the skill's `SKILL.md` so the
// agent learns when and how to call that tool.
//
// A pure-prompt skill (no `Tool`) is fine — the agent uses
// whatever tools its workstation already has.
type SkillInvocation struct {
	// Name of the OpenClaw plugin tool this skill is built on
	// (e.g. "browser", "web_fetch", "exec", "memory"). Recorded in
	// status and used by the operator to validate the bound
	// AgentGateway has the matching capability before an AW that
	// uses this skill is allowed to go Ready.
	// +optional
	Tool string `json:"tool,omitempty"`

	// Optional Go-template body rendered into the skill's
	// `SKILL.md`. Variables `{{.Skill}}`, `{{.Agent}}`,
	// `{{.BindingOverrides}}` are available. Use this for skills
	// where the shape of the procedure is the same across agents
	// but the parameters differ.
	// +optional
	PromptTemplate string `json:"promptTemplate,omitempty"`
}

// SkillSpec defines the desired state of a Skill — a reusable,
// declarative capability that one or more AgentWorkstations can
// consume via `SkillBinding`.
//
// A Skill is more than a MemoryModule: it has an invocation
// contract (which OpenClaw tool implements it), required
// capabilities (which the operator validates against the bound
// AgentGateway), and a version that bindings can pin against. The
// `.md` content is the procedure documentation the agent reads at
// session start; the rest is the runtime contract.
type SkillSpec struct {
	// Human-friendly name shown in the Console plugin and
	// Backstage tile. Defaults to metadata.name when empty.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Short description of what the skill does. Surfaced in the
	// Console plugin and in `status` messages.
	// +optional
	Description string `json:"description,omitempty"`

	// Semver-ish version string of the skill's content +
	// invocation contract. SkillBindings can pin a specific
	// version via `spec.skillRef.version`; otherwise they float
	// with whatever this Skill currently declares.
	// +optional
	Version string `json:"version,omitempty"`

	// Source is where the skill's `SKILL.md` content lives.
	// Exactly one of configMapRef or inline must be set.
	Source SkillSource `json:"source"`

	// Invocation describes the runtime contract — which OpenClaw
	// plugin tool implements this skill, and an optional prompt
	// template rendered into `SKILL.md`.
	// +optional
	Invocation SkillInvocation `json:"invocation,omitempty"`

	// Requires lists OpenClaw capabilities (`browser`,
	// `web_search`, `web_fetch`, `exec`, `memory`, etc.) the agent
	// needs to use this skill. The operator validates that the
	// AgentWorkstation's bound AgentGateway provides each
	// capability before the AW can go Ready. Missing capabilities
	// surface in `aw.status.message` so users see why their agent
	// is Pending.
	// +optional
	Requires []string `json:"requires,omitempty"`
}

// SkillCondition is a standard Kubernetes condition entry on the
// skill's status.
type SkillCondition struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// SkillStatus reflects observed state. The controller fills these
// fields on every reconcile.
type SkillStatus struct {
	// SHA-256 of the resolved `SKILL.md` content. Used by the UI
	// to detect drift and by agents to know when their cached
	// copy is stale.
	// +optional
	ContentSHA256 string `json:"contentSha256,omitempty"`

	// AgentWorkstation names currently bound to this skill via
	// any SkillBinding in the same namespace. Maintained by the
	// controller as bindings + AWs change.
	// +optional
	ReferencedBy []string `json:"referencedBy,omitempty"`

	// Conditions reflect the latest observations. Standard types:
	// Ready, ContentResolved.
	// +optional
	Conditions []SkillCondition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sk,categories=agent-office
// +kubebuilder:printcolumn:name="DisplayName",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Tool",type="string",JSONPath=".spec.invocation.tool"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".spec.version"
// +kubebuilder:printcolumn:name="ReferencedBy",type="integer",JSONPath=".status.referencedBy",description="Number of agents currently bound to this skill"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Skill is a reusable, declarative agent capability. AgentWorkstations
// gain access to a Skill via a SkillBinding (RBAC-style — direct
// inline references on the AW are not used). The Skill's `SKILL.md`
// content is rendered into each bound agent's workspace per the
// Anthropic Skills Open Standard, and its required capabilities are
// validated against the bound AgentGateway before the AW goes Ready.
type Skill struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SkillSpec   `json:"spec,omitempty"`
	Status SkillStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SkillList contains a list of Skill.
type SkillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Skill `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Skill{}, &SkillList{})
}
