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

// SkillRef points to a Skill in the same namespace. Optional
// `version` pins a specific Skill spec.version; if empty, the
// binding floats with whatever the Skill CR currently declares.
//
// Floating refs are convenient in dev (Skill CR rev is the source
// of truth, no churn on every bump). Pinned refs are the right
// posture in prod where you want explicit control over which
// version of a skill an agent picks up — surface the resolved
// version in `status.resolvedSkillVersion` either way.
type SkillRef struct {
	// Name of the Skill CR in the same namespace.
	Name string `json:"name"`

	// Optional version pin. When set, the binding only resolves
	// successfully if the referenced Skill's `spec.version`
	// matches this value (semver string equality).
	// +optional
	Version string `json:"version,omitempty"`
}

// SkillBindingSubject names a single AgentWorkstation that should
// receive the bound skill. `Kind` exists so we can extend to
// AgentGateway-scoped grants later (every logical agent in the
// gateway pod inherits the skill) without breaking the API.
type SkillBindingSubject struct {
	// Kind of the subject. Currently only "AgentWorkstation".
	// +kubebuilder:validation:Enum=AgentWorkstation
	Kind string `json:"kind"`

	// Name of the subject in the same namespace as the binding.
	Name string `json:"name"`
}

// SkillOverrides are layered on top of the Skill's base spec when
// rendering the per-agent `SKILL.md`. Precedence is: skill base
// content < binding overrides < (future) AW per-skill overrides.
//
// Keep these small and append-only — overrides that fundamentally
// change what the skill does belong in a separate Skill, not
// hidden in a binding.
type SkillOverrides struct {
	// Markdown text appended to the bound agent's `SKILL.md`
	// after the base content. Useful for narrowing scope per
	// binding (e.g. "for Red Hat content, prefer redhat.com
	// domains").
	// +optional
	PromptAdditions string `json:"promptAdditions,omitempty"`

	// Free-form key/value parameters surfaced to the skill's
	// invocation template as `{{.BindingOverrides.<key>}}`. Use
	// for per-binding tuning like timeouts or default search
	// limits. Values are strings to keep the schema simple — cast
	// in the template if you need numbers.
	// +optional
	InvocationParams map[string]string `json:"invocationParams,omitempty"`
}

// SkillBindingSpec declares which AgentWorkstations receive the
// referenced Skill. Subjects can be listed directly or matched via
// label selector; both lists are unioned. An empty subjects list
// + nil selector matches no AWs (intentional — a typo shouldn't
// silently grant a skill cluster-wide).
type SkillBindingSpec struct {
	// SkillRef points to the Skill being granted. Required.
	SkillRef SkillRef `json:"skillRef"`

	// Direct subject list. Each entry must reference an
	// AgentWorkstation in the same namespace as this binding.
	// +optional
	Subjects []SkillBindingSubject `json:"subjects,omitempty"`

	// Label selector against AgentWorkstation `metadata.labels`.
	// Unioned with `subjects`. Useful for bulk grants like
	// "every research-role agent gets the web-research skill".
	// +optional
	SubjectSelector *metav1.LabelSelector `json:"subjectSelector,omitempty"`

	// Overrides applied on top of the Skill's base content/params
	// for every AW this binding resolves to.
	// +optional
	Overrides *SkillOverrides `json:"overrides,omitempty"`
}

// SkillBindingConflict records that another SkillBinding in the
// same namespace also grants the same Skill to one of this
// binding's subjects. Conflicts don't block reconciliation — the
// AgentWorkstation reconciler picks a deterministic winner
// (lex-sorted by binding name, last-wins) — but we surface them so
// users can see overlap and clean it up.
type SkillBindingConflict struct {
	// Name of the AgentWorkstation that has multiple bindings
	// granting the same skill.
	AgentWorkstation string `json:"agentWorkstation"`

	// Names of the other bindings overlapping on this AW for the
	// same skill.
	OverlapsWith []string `json:"overlapsWith"`
}

// SkillBindingCondition is a standard Kubernetes condition entry.
type SkillBindingCondition struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// SkillBindingStatus reflects observed state.
type SkillBindingStatus struct {
	// AgentWorkstation names this binding currently applies to,
	// after subject + selector resolution. Maintained by the
	// SkillBinding controller as AWs are created, deleted, or
	// re-labeled.
	// +optional
	AppliedTo []string `json:"appliedTo,omitempty"`

	// Resolved Skill version this binding points at. Either the
	// pinned `spec.skillRef.version`, or "floating: <skill-version>"
	// when the binding doesn't pin.
	// +optional
	ResolvedSkillVersion string `json:"resolvedSkillVersion,omitempty"`

	// Conflicts with other bindings overlapping on the same
	// (AgentWorkstation, Skill) pair.
	// +optional
	Conflicts []SkillBindingConflict `json:"conflicts,omitempty"`

	// Conditions reflect the latest observations. Standard types:
	// Ready, SkillResolved, SubjectsResolved.
	// +optional
	Conditions []SkillBindingCondition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=skb,categories=agent-office
// +kubebuilder:printcolumn:name="Skill",type="string",JSONPath=".spec.skillRef.name"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.resolvedSkillVersion"
// +kubebuilder:printcolumn:name="AppliedTo",type="integer",JSONPath=".status.appliedTo",description="Number of AgentWorkstations this binding applies to"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SkillBinding grants a Skill to one or more AgentWorkstations,
// RBAC-style. The AW does not list its skills inline — instead,
// the AgentWorkstation reconciler queries the SkillBindings in
// its namespace at reconcile time to discover what skills apply.
//
// This decouples skill *authorship* (who writes the Skill CR)
// from skill *grants* (who decides which agents get it). Useful
// for platform setups where a security/platform team curates
// skills and a separate team authors agents.
type SkillBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SkillBindingSpec   `json:"spec,omitempty"`
	Status SkillBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SkillBindingList contains a list of SkillBinding.
type SkillBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SkillBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SkillBinding{}, &SkillBindingList{})
}
