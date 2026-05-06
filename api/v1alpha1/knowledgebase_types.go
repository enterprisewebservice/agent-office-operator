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
	"k8s.io/apimachinery/pkg/api/resource"
)

// AgentGatewayRef points at an AgentGateway in the same namespace
// as the KnowledgeBase. The KB's PVC is mounted into the gateway
// pod at `/home/node/.openclaw/wiki/<kb-name>/`, so every logical
// agent running in that gateway sees the same wiki content. This
// matches Karpathy's "team brain" model where all research agents
// pulled by topic share a single evolving knowledge base.
type AgentGatewayRef struct {
	// Name of the AgentGateway in the same namespace.
	Name string `json:"name"`
}

// KnowledgeBaseStorage configures the persistent volume that
// backs the wiki content. The KB controller provisions a PVC
// named `<kb-name>-wiki` in the KB's namespace; the gateway
// reconciler then mounts it into the gateway pod.
//
// Default size is 5Gi. Storage class is left to the cluster
// default unless explicitly set — the platform's default is
// `ocs-storagecluster-ceph-rbd` (see other AW PVCs in
// agent-office namespace) but we don't hard-code it here.
type KnowledgeBaseStorage struct {
	// Size of the PVC. Defaults to 5Gi when unset.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// StorageClassName for the PVC. When unset, the cluster
	// default storage class is used.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// AccessModes for the PVC. Defaults to [ReadWriteOnce] when
	// unset. ReadWriteMany is appropriate when one KB attaches
	// to multiple gateways concurrently.
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
}

// KnowledgeBaseGitMirror configures optional bidirectional sync
// between the wiki PVC and a git repository. Useful for
// portability (open the wiki in Obsidian on a laptop) and for
// audit trails (every "agent files an article" becomes a git
// commit). The mirror is implemented by the KB controller via a
// CronJob that pulls from the remote on `cadence`, then commits
// + pushes any local changes since the last cycle.
//
// Conflict resolution is "last write wins" — agents writing to
// the wiki are the source of truth in shared mode. If you need
// stronger guarantees, use a read-only mirror by setting
// `pushEnabled: false`.
type KnowledgeBaseGitMirror struct {
	// HTTPS URL of the git repository to mirror.
	URL string `json:"url"`

	// Branch to track. Defaults to "main" when unset.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Cadence in cron syntax (e.g. "*/30 * * * *"). Defaults to
	// every 30 minutes when unset.
	// +optional
	Cadence string `json:"cadence,omitempty"`

	// Reference to a Secret in the same namespace holding git
	// credentials. The Secret must contain a `token` key
	// (Personal Access Token / fine-grained token with repo
	// scope) or an `ssh-privatekey` key.
	// +optional
	CredentialsSecretRef string `json:"credentialsSecretRef,omitempty"`

	// PushEnabled controls whether the mirror writes local
	// changes back to the remote. Set false for read-only
	// mirrors (the wiki ingests from the remote but doesn't
	// push). Defaults true when the credentialsSecretRef is set.
	// +optional
	PushEnabled *bool `json:"pushEnabled,omitempty"`
}

// KnowledgeBaseSpec describes a wiki — a structured collection
// of `.md` files and supporting media (the Karpathy KB pattern)
// that one or more shared-runtime agents read at session start
// and incrementally extend through skills.
type KnowledgeBaseSpec struct {
	// Human-friendly name for the wiki. Defaults to
	// metadata.name when unset.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Short description shown in the Console plugin and surfaced
	// to agents in their `WIKI.md` workspace file.
	// +optional
	Description string `json:"description,omitempty"`

	// GatewayRef binds this KB to a single AgentGateway. The
	// gateway reconciler mounts the KB's PVC into the gateway
	// pod at `/home/node/.openclaw/wiki/<kb-name>/`, making the
	// wiki visible to every logical agent in that gateway.
	GatewayRef AgentGatewayRef `json:"gatewayRef"`

	// Storage configures the PVC backing the wiki content.
	// +optional
	Storage KnowledgeBaseStorage `json:"storage,omitempty"`

	// GitMirror optionally syncs the PVC to a git repository on
	// a cron schedule. Use this to open the wiki in Obsidian /
	// VS Code locally, or for audit trails.
	// +optional
	GitMirror *KnowledgeBaseGitMirror `json:"gitMirror,omitempty"`

	// InitFromGit, when set, clones the named git repository
	// into the PVC the first time it's provisioned. After the
	// initial clone, GitMirror takes over (if configured); if
	// GitMirror isn't set, the repo is one-shot copied and not
	// kept in sync.
	// +optional
	InitFromGit *KnowledgeBaseGitMirror `json:"initFromGit,omitempty"`
}

// KnowledgeBaseCondition is a standard Kubernetes condition.
type KnowledgeBaseCondition struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// KnowledgeBaseStatus reflects observed state.
type KnowledgeBaseStatus struct {
	// PVCName is the name of the PersistentVolumeClaim the
	// controller provisioned. Always equals "<kb-name>-wiki".
	// +optional
	PVCName string `json:"pvcName,omitempty"`

	// MountPath inside the gateway pod where the wiki is
	// available. Always
	// `/home/node/.openclaw/wiki/<kb-name>/`.
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// BoundGateway names the AgentGateway this KB is currently
	// mounted into. Set after the gateway reconciler confirms
	// the volume + mount landed on the gateway Deployment.
	// +optional
	BoundGateway string `json:"boundGateway,omitempty"`

	// AgentCount is the number of logical agents in the bound
	// gateway that can see this wiki.
	// +optional
	AgentCount int32 `json:"agentCount,omitempty"`

	// LastSyncTime, when GitMirror is configured, is the last
	// successful pull/push against the remote. Empty if the
	// mirror has never run or isn't configured.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// Conditions reflect the latest observations. Standard
	// types: Ready, PVCReady, GatewayMounted, GitSyncReady.
	// +optional
	Conditions []KnowledgeBaseCondition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=kb,categories=agent-office
// +kubebuilder:printcolumn:name="DisplayName",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Gateway",type="string",JSONPath=".spec.gatewayRef.name"
// +kubebuilder:printcolumn:name="Agents",type="integer",JSONPath=".status.agentCount"
// +kubebuilder:printcolumn:name="MountPath",type="string",JSONPath=".status.mountPath"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// KnowledgeBase is a wiki — a persistent, structured collection
// of `.md` files and supporting media — backed by a PVC and
// mounted into a single AgentGateway pod. All logical agents in
// the bound gateway see the wiki at
// `/home/node/.openclaw/wiki/<kb-name>/` and can read, search,
// and extend it via skills (e.g. `wiki-clip`, `wiki-search`,
// `wiki-write`).
//
// Pattern follows Andrej Karpathy's "LLM Knowledge Base"
// workflow (Sep 2025): raw data ingested, an LLM compiles a
// structured wiki, queries land back as new wiki entries, and
// the wiki gets smarter over time as agents file their findings
// into it. KnowledgeBase is the Kubernetes-native handle for
// that wiki.
type KnowledgeBase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KnowledgeBaseSpec   `json:"spec,omitempty"`
	Status KnowledgeBaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KnowledgeBaseList contains a list of KnowledgeBase.
type KnowledgeBaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBase `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KnowledgeBase{}, &KnowledgeBaseList{})
}
