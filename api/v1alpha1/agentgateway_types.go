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
}

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
