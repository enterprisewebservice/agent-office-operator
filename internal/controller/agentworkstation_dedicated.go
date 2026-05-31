/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// ----------------------------------------------------------------------
// Unified runtime model.
//
// Shared and dedicated are NOT separate implementations. An agent is
// always a logical openclaw persona registered on an AgentGateway. The
// only difference:
//   - shared    → the AW joins an EXISTING AgentGateway (gatewayRef).
//   - dedicated → the operator mints the AW its OWN AgentGateway here,
//     owned by the AW (so it cascades on delete), then registers the
//     agent on it via the SAME reconcileSharedFull machinery.
//
// This replaced the old divergent per-AW Pod path (reconcileChildren +
// internal/templates/render.go), which was a stale reimplementation of
// the gateway pod that silently lacked Codex auth and the skills image
// volume. By minting an AgentGateway instead, dedicated agents get the
// SAME feature-complete pod shared agents run on — Codex, skills, the
// constrained SCC — for free, and a dedicated gateway is just "a gateway
// with one agent" that more agents can join later.
// ----------------------------------------------------------------------

// dedicatedGatewayName is the AgentGateway minted for a dedicated agent:
// named after the agent itself.
func dedicatedGatewayName(awName string) string { return awName }

// effectiveGatewayRef returns the gateway an AW registers on — the
// referenced one for shared, the minted per-agent one for dedicated.
func effectiveGatewayRef(aw *agentofficev1alpha1.AgentWorkstation) string {
	if aw.Spec.Runtime != nil && aw.Spec.Runtime.Shared != nil {
		return aw.Spec.Runtime.Shared.GatewayRef
	}
	return dedicatedGatewayName(aw.Name)
}

// effectiveProfile returns the agent's Chromium browser-profile name.
func effectiveProfile(aw *agentofficev1alpha1.AgentWorkstation) string {
	if aw.Spec.Runtime != nil && aw.Spec.Runtime.Shared != nil &&
		aw.Spec.Runtime.Shared.BrowserProfile != "" {
		return aw.Spec.Runtime.Shared.BrowserProfile
	}
	return aw.Name
}

// isDedicated reports whether the AW runs on its own minted gateway
// (dedicated, or the default when runtime is unspecified) vs joining an
// existing one (shared).
func isDedicated(aw *agentofficev1alpha1.AgentWorkstation) bool {
	return aw.Spec.Runtime == nil || aw.Spec.Runtime.Shared == nil
}

// reconcileDedicatedGateway mints (or updates) the per-agent AgentGateway
// for a dedicated AW, owned by the AW so it cascades on delete. The spec
// is derived from the AW; the AgentGateway controller then builds the
// feature-complete gateway pod the agent registers onto.
func (r *AgentWorkstationReconciler) reconcileDedicatedGateway(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) error {
	gw := &agentofficev1alpha1.AgentGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dedicatedGatewayName(aw.Name),
			Namespace: aw.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, gw, func() error {
		gw.Spec.DisplayName = defaultIfEmpty(aw.Spec.DisplayName, aw.Name)
		gw.Spec.Description = "Dedicated gateway for agent " + aw.Name
		if aw.Spec.Image != "" {
			gw.Spec.Image = aw.Spec.Image
		}
		// Codex-backed agents read ~/.codex/auth.json — point the gateway
		// at the cluster-managed Codex subscription secret (the same one
		// the shared research-gateway uses). Other providers flow their
		// API key in via the gateway's envFrom (future: derive from spec).
		if aw.Spec.Model.Provider == agentofficev1alpha1.ModelProviderOpenAICodex {
			gw.Spec.CodexCredentialsSecretRef = "codex-subscription-credentials"
		}
		return controllerutil.SetControllerReference(aw, gw, r.Scheme)
	})
	return err
}
