/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// ----------------------------------------------------------------------
// Unified runtime model — there is NO "dedicated" code path.
//
// Every agent is a logical openclaw persona registered on an
// AgentGateway. The operator has exactly ONE behaviour: register the
// agent on the gateway named by runtime.shared.gatewayRef (see
// reconcileSharedFull). It never mints or owns a gateway.
//
// "Dedicated" is purely a packaging choice made in the scaffolder
// template, not a runtime difference the operator sees:
//   - shared    → gatewayRef points at a long-lived gateway someone
//                 else declared (e.g. research-gateway in the cluster repo).
//   - dedicated → the agent's OWN gitops repo also declares an
//                 AgentGateway/<name>, and its gatewayRef points at it.
//
// A gateway is therefore always a first-class, declarative resource;
// nobody owns it via ownerReferences, and its lifecycle is git/ArgoCD.
// Deleting a dedicated agent prunes its repo → both the AgentWorkstation
// and its AgentGateway go (the gateway cascades its own pod/PVC/etc.);
// deleting a shared agent just de-registers it (the finalizer) and the
// gateway lives on for the others. Adding more agents to any gateway is
// the same operation either way: point another AW's gatewayRef at it.
// ----------------------------------------------------------------------

// effectiveGatewayRef returns the AgentGateway this agent runs on. Empty
// only if the AW is misconfigured (no runtime.shared.gatewayRef) — the
// caller surfaces that in status and requeues.
func effectiveGatewayRef(aw *agentofficev1alpha1.AgentWorkstation) string {
	if aw.Spec.Runtime != nil && aw.Spec.Runtime.Shared != nil {
		return aw.Spec.Runtime.Shared.GatewayRef
	}
	return ""
}

// effectiveProfile returns the agent's Chromium browser-profile name,
// defaulting to the agent name when the spec doesn't pin one.
func effectiveProfile(aw *agentofficev1alpha1.AgentWorkstation) string {
	if aw.Spec.Runtime != nil && aw.Spec.Runtime.Shared != nil &&
		aw.Spec.Runtime.Shared.BrowserProfile != "" {
		return aw.Spec.Runtime.Shared.BrowserProfile
	}
	return aw.Name
}
