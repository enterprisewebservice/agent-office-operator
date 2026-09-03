/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// tenantPrincipal is who a tenant workspace acts for, read from the
// namespace's agentoffice.ai/user and agentoffice.ai/groups annotations.
type tenantPrincipal struct {
	User   string
	Groups []string
}

// tenantOf returns the workspace's principal. ok=false means a platform
// namespace: no offers are projected and no connection is gated.
func tenantOf(ns *corev1.Namespace) (tenantPrincipal, bool) {
	if ns == nil {
		return tenantPrincipal{}, false
	}
	ann := ns.Annotations
	_, hasUser := ann[agentofficev1alpha1.TenantUserAnnotation]
	_, hasGroups := ann[agentofficev1alpha1.TenantGroupsAnnotation]
	if !hasUser && !hasGroups {
		return tenantPrincipal{}, false
	}
	p := tenantPrincipal{User: strings.TrimSpace(ann[agentofficev1alpha1.TenantUserAnnotation])}
	for _, g := range strings.Split(ann[agentofficev1alpha1.TenantGroupsAnnotation], ",") {
		if g = strings.TrimSpace(g); g != "" {
			p.Groups = append(p.Groups, g)
		}
	}
	return p, true
}

// connectionOfferedTo mirrors the Developer Hub menu rule (canSee in
// AgentGenesisField.tsx): a connection with an empty access list is
// offered to nobody; users match by name and groups by name, both
// case-insensitively, and a group reference such as
// group:default/attendees matches on its last segment. Returns what
// matched ("user:<name>" / "group:<name>") for the offer to record.
func connectionOfferedTo(conn *agentofficev1alpha1.ModelConnection, p tenantPrincipal) (bool, string) {
	if conn == nil {
		return false, ""
	}
	a := conn.Spec.Access
	if a == nil || (len(a.Groups) == 0 && len(a.Users) == 0) {
		return false, ""
	}
	if lcUser := strings.ToLower(strings.TrimSpace(p.User)); lcUser != "" {
		for _, u := range a.Users {
			if strings.ToLower(strings.TrimSpace(u)) == lcUser {
				return true, "user:" + strings.TrimSpace(u)
			}
		}
	}
	mine := map[string]bool{}
	for _, g := range p.Groups {
		lc := strings.ToLower(strings.TrimSpace(g))
		if lc == "" {
			continue
		}
		mine[lc] = true
		if i := strings.LastIndex(lc, "/"); i >= 0 {
			mine[lc[i+1:]] = true
		}
	}
	for _, g := range a.Groups {
		lg := strings.ToLower(strings.TrimSpace(g))
		if lg == "" {
			continue
		}
		short := lg
		if i := strings.LastIndex(lg, "/"); i >= 0 {
			short = lg[i+1:]
		}
		if mine[lg] || mine[short] {
			return true, "group:" + strings.TrimSpace(g)
		}
	}
	return false, ""
}

// offerSpecFor is the non-secret projection of a connection.
func offerSpecFor(conn *agentofficev1alpha1.ModelConnection, offeredTo string) agentofficev1alpha1.ModelConnectionOfferSpec {
	spec := agentofficev1alpha1.ModelConnectionOfferSpec{
		ConnectionRef: conn.Name,
		DisplayName:   conn.Spec.DisplayName,
		Description:   conn.Spec.Description,
		Kind:          conn.Spec.Kind,
		Provider:      conn.Spec.Provider,
		OfferedTo:     offeredTo,
	}
	if len(conn.Spec.Models) > 0 {
		spec.Models = append([]agentofficev1alpha1.ModelCatalogEntry(nil), conn.Spec.Models...)
	}
	if a := conn.Spec.Access; a != nil {
		spec.Access = &agentofficev1alpha1.ModelConnectionAccess{
			Groups: append([]string(nil), a.Groups...),
			Users:  append([]string(nil), a.Users...),
		}
	}
	return spec
}

// noteModelConnectionOffer records on a workstation in a tenant
// workspace whether its connectionRef is offered there. Platform
// namespaces and workstations without a connectionRef carry no
// condition. The gateway enforces the same rule when it renders.
func (r *AgentWorkstationReconciler) noteModelConnectionOffer(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) {
	const condType = "ModelConnectionOffered"
	ref := aw.Spec.Model.ConnectionRef
	if ref == "" {
		meta.RemoveStatusCondition(&aw.Status.Conditions, condType)
		return
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: aw.Namespace}, &ns); err != nil {
		return
	}
	p, gated := tenantOf(&ns)
	if !gated {
		meta.RemoveStatusCondition(&aw.Status.Conditions, condType)
		return
	}
	cond := metav1.Condition{Type: condType, Status: metav1.ConditionTrue, Reason: "Offered"}
	var conn agentofficev1alpha1.ModelConnection
	if err := r.Get(ctx, client.ObjectKey{Name: ref}, &conn); err != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "ConnectionNotFound"
		cond.Message = fmt.Sprintf("ModelConnection %q does not exist", ref)
	} else if offered, by := connectionOfferedTo(&conn, p); offered {
		cond.Message = fmt.Sprintf("%s is offered to this workspace (%s)", ref, by)
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NotOffered"
		cond.Message = fmt.Sprintf("ModelConnection %q is not offered to this workspace (user %q, groups %v); the gateway does not render it", ref, p.User, p.Groups)
		logf.FromContext(ctx).Info("model connection not offered to workspace", "aw", aw.Name, "connection", ref)
	}
	meta.SetStatusCondition(&aw.Status.Conditions, cond)
}
