/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Tenant workspace annotations.
//
// A namespace carrying either annotation is a tenant workspace: it acts
// for one signed-in principal (a workshop seat, a self-service hire).
// The operator projects a ModelConnectionOffer into it for every
// published ModelConnection whose access list admits that principal,
// and the gateway renders ONLY offered connections — a connectionRef the
// workspace was never offered gets no provider block and no projected
// key, and the workstation reports ModelConnectionOffered=False.
//
// Namespaces without them are platform namespaces: nothing is projected
// and every connectionRef renders, as before.
//
// The annotations are stamped by whoever provisions the workspace (the
// factory hub provisioner, the deployer's seat manifests); a namespace
// admin cannot edit its own Namespace object, so a seat cannot widen
// its own menu.
const (
	// TenantUserAnnotation names the principal the workspace acts for —
	// the username the identity provider signs in.
	TenantUserAnnotation = "agentoffice.ai/user"
	// TenantGroupsAnnotation lists, comma-separated, the identity
	// provider groups that principal belongs to (e.g. "attendees").
	TenantGroupsAnnotation = "agentoffice.ai/groups"
	// OfferConnectionLabel on a ModelConnectionOffer names the
	// ModelConnection it mirrors.
	OfferConnectionLabel = "agentoffice.ai/connection"
)

// ModelConnectionOfferSpec is the non-secret face of a published
// ModelConnection as offered to one tenant workspace. The operator
// writes it; hand edits are overwritten on the next reconcile, and an
// offer grants nothing by itself — the gateway decides from the
// namespace's principal and the connection's access list.
type ModelConnectionOfferSpec struct {
	// ConnectionRef is the ModelConnection this offer mirrors — the
	// value a workstation puts in spec.model.connectionRef.
	ConnectionRef string `json:"connectionRef"`

	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// +optional
	Description string `json:"description,omitempty"`

	// +optional
	Kind ModelConnectionKind `json:"kind,omitempty"`

	// +optional
	Provider ModelProvider `json:"provider,omitempty"`

	// Models the connection publishes (ids and labels — no endpoint,
	// no credential).
	// +optional
	Models []ModelCatalogEntry `json:"models,omitempty"`

	// Access is the connection's access list, copied so the reader can
	// see why the offer exists.
	// +optional
	Access *ModelConnectionAccess `json:"access,omitempty"`

	// OfferedTo names what matched the access list for this workspace:
	// "user:<name>" or "group:<name>".
	// +optional
	OfferedTo string `json:"offeredTo,omitempty"`
}

// ModelConnectionOfferStatus records which revision of the connection
// the offer mirrors.
type ModelConnectionOfferStatus struct {
	// ConnectionGeneration is the metadata.generation of the
	// ModelConnection this offer was last projected from.
	// +optional
	ConnectionGeneration int64 `json:"connectionGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcoffer
// +kubebuilder:printcolumn:name="Connection",type=string,JSONPath=`.spec.connectionRef`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Display",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Offered-To",type=string,JSONPath=`.spec.offeredTo`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelConnectionOffer is a published brain as seen from one tenant
// workspace: the operator projects one per ModelConnection whose access
// list admits the workspace's principal. Listing them in a workspace is
// exactly the hiring menu the Developer Hub shows that principal.
type ModelConnectionOffer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelConnectionOfferSpec   `json:"spec,omitempty"`
	Status ModelConnectionOfferStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelConnectionOfferList contains a list of ModelConnectionOffer.
type ModelConnectionOfferList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelConnectionOffer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelConnectionOffer{}, &ModelConnectionOfferList{})
}
