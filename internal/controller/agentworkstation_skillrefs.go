//go:build ignore
// +build ignore

// NOTE: build-tagged out (2026-05-28) because this WIP file
// references AgentWorkstationSpec.SkillRefs which doesn't exist
// yet. Part of task #51 (v1.6.0 spec.skillRefs normalization).
// Remove the build tag when the spec field lands. Until then,
// keeping the WIP visible (rather than deleting it) so the design
// notes aren't lost.

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// reconcileSkillRefs (v1.6.0) handles spec.skillRefs on
// AgentWorkstation by maintaining one operator-managed SkillBinding
// CR per ref:
//
//   - Each managed SkillBinding is named
//     `aw-binding-<aw-name>-<skill-name>` (truncated + hashed if it
//     would exceed K8s' 253-char metadata.name limit).
//
//   - Each is owner-ref'd to the AW so deletion of the AW
//     cascade-GCs the bindings.
//
//   - Each is labeled
//     `agentoffice.ai/managed-by-aw: <aw-name>` so subsequent
//     reconciles can list them efficiently and detect-delete drift.
//
//   - spec.subjects on the binding is exactly `[{Kind:
//     AgentWorkstation, Name: <aw-name>}]` — direct binding, no
//     selector. The SkillBindingReconciler resolves subjects on its
//     own pass and surfaces the resolved set in status.appliedTo.
//
//   - spec.skillRef matches the AW's ref verbatim (Name + Version).
//
// This is the symmetric-with-KnowledgeBaseRefs path. Hooks into
// AgentWorkstationReconciler.Reconcile BEFORE the shared/dedicated
// dispatch so it runs for BOTH runtime models (Skills aren't
// gateway-scoped — they're applied to AWs regardless of where the
// AW runs).
//
// User-created SkillBindings (without the managed-by-aw label) are
// untouched by this reconciler. Two paths coexist.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

const (
	// awSkillBindingManagedByLabel marks SkillBindings as
	// operator-managed-via-AW.spec.skillRefs (vs user-created).
	awSkillBindingManagedByLabel = "agentoffice.ai/managed-by-aw"
)

// +kubebuilder:rbac:groups=agentoffice.ai,resources=skillbindings,verbs=get;list;watch;create;update;patch;delete

// reconcileSkillRefs syncs the operator-managed SkillBindings for
// an AW with its spec.skillRefs. Idempotent.
func (r *AgentWorkstationReconciler) reconcileSkillRefs(
	ctx context.Context,
	aw *agentofficev1alpha1.AgentWorkstation,
) error {
	log := logf.FromContext(ctx)

	desired := map[string]agentofficev1alpha1.SkillRef{}
	for _, ref := range aw.Spec.SkillRefs {
		// Dedup by skill name — last entry wins if the user lists
		// the same skill twice (silently). Matches the same
		// dedup-by-name approach used in reconcileKnowledgeBaseRefs.
		desired[ref.Name] = ref
	}

	// List existing operator-managed SkillBindings for this AW.
	var existing agentofficev1alpha1.SkillBindingList
	if err := r.List(ctx, &existing,
		client.InNamespace(aw.Namespace),
		client.MatchingLabels{awSkillBindingManagedByLabel: aw.Name}); err != nil {
		return fmt.Errorf("list managed SkillBindings for aw %s: %w", aw.Name, err)
	}

	existingByName := map[string]*agentofficev1alpha1.SkillBinding{}
	for i := range existing.Items {
		sb := &existing.Items[i]
		existingByName[sb.Name] = sb
	}

	// 1. Create or update one SkillBinding per desired ref.
	for skillName, ref := range desired {
		sbName := managedSkillBindingName(aw.Name, skillName)
		sb := &agentofficev1alpha1.SkillBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sbName,
				Namespace: aw.Namespace,
			},
		}
		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, sb, func() error {
			if sb.Labels == nil {
				sb.Labels = map[string]string{}
			}
			sb.Labels[awSkillBindingManagedByLabel] = aw.Name
			sb.Spec.SkillRef = ref
			sb.Spec.Subjects = []agentofficev1alpha1.SkillBindingSubject{
				{Kind: "AgentWorkstation", Name: aw.Name},
			}
			// Owner-ref so the binding GCs when the AW is deleted.
			return controllerutil.SetControllerReference(aw, sb, r.Scheme)
		})
		if err != nil {
			return fmt.Errorf("upsert managed SkillBinding %s: %w", sbName, err)
		}
		if op != controllerutil.OperationResultNone {
			log.Info("managed SkillBinding reconciled",
				"aw", aw.Name, "skill", skillName, "binding", sbName, "op", op)
		}
		// Remove from existingByName so anything left at the end is drift.
		delete(existingByName, sbName)
	}

	// 2. Anything still in existingByName is a binding for a skill
	//    the user has REMOVED from spec.skillRefs. Delete it.
	for sbName, sb := range existingByName {
		if err := r.Delete(ctx, sb); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale managed SkillBinding %s: %w", sbName, err)
		}
		log.Info("managed SkillBinding deleted (no longer in spec.skillRefs)",
			"aw", aw.Name, "binding", sbName)
	}
	return nil
}

// managedSkillBindingName returns a deterministic name for the
// operator-managed SkillBinding that grants `skillName` to `awName`.
//
// Plain concatenation `aw-binding-<aw>-<skill>` is normally fine
// (well under K8s' 253-char metadata.name limit for typical names).
// For pathological long names we fall back to
// `aw-binding-<aw-prefix>-<sha8-of-skillName>` to stay valid.
func managedSkillBindingName(awName, skillName string) string {
	const prefix = "aw-binding-"
	plain := prefix + awName + "-" + skillName
	// K8s name limit is 253 chars and most components prefer well
	// under that. Cap at 200 to leave headroom for finalizer-related
	// status fields.
	if len(plain) <= 200 {
		return plain
	}
	sum := sha256.Sum256([]byte(skillName))
	short := hex.EncodeToString(sum[:4]) // 8-char hex
	// Trim awName so the total stays well-bounded.
	maxAw := 200 - len(prefix) - 1 - 8
	if maxAw < 1 {
		maxAw = 1
	}
	trimmed := awName
	if len(trimmed) > maxAw {
		trimmed = trimmed[:maxAw]
	}
	return prefix + trimmed + "-" + short
}
