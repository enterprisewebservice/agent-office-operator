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

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// SkillReconciler resolves a Skill's source content, computes a
// SHA-256 hash, and indexes which AgentWorkstations are bound to
// it via SkillBindings.
type SkillReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentoffice.ai,resources=skills,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentoffice.ai,resources=skills/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=skills/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentoffice.ai,resources=skillbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile resolves the skill's source content and updates status:
//   - status.contentSha256: SHA-256 of the resolved SKILL.md.
//   - status.referencedBy: AgentWorkstation names currently bound
//     to this skill via any SkillBinding in this namespace.
//   - status.conditions: ContentResolved + Ready.
func (r *SkillReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var skill agentofficev1alpha1.Skill
	if err := r.Get(ctx, req.NamespacedName, &skill); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1. Resolve content (configMapRef takes precedence over inline).
	content, contentErr := r.resolveSkillContent(ctx, &skill)

	// 2. Compute hash. Empty content still hashes; the
	//    ContentResolved condition tracks resolution success.
	hash := ""
	if contentErr == nil {
		sum := sha256.Sum256([]byte(content))
		hash = hex.EncodeToString(sum[:])
	}

	// 3. Compute referencedBy by resolving all SkillBindings in
	//    this namespace and unioning the AWs each binding applies
	//    to (when it grants this skill).
	refs, err := r.resolveReferencedBy(ctx, &skill)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving referencedBy: %w", err)
	}

	// 4. Build status patch.
	now := metav1.Now()
	resolvedCond := agentofficev1alpha1.SkillCondition{
		Type:               "ContentResolved",
		Status:             "True",
		Reason:             "Resolved",
		Message:            fmt.Sprintf("Resolved %d bytes", len(content)),
		LastTransitionTime: now,
	}
	if contentErr != nil {
		resolvedCond.Status = "False"
		resolvedCond.Reason = "ResolveError"
		resolvedCond.Message = contentErr.Error()
	}
	readyCond := agentofficev1alpha1.SkillCondition{
		Type:               "Ready",
		Status:             resolvedCond.Status,
		Reason:             resolvedCond.Reason,
		Message:            resolvedCond.Message,
		LastTransitionTime: now,
	}

	skill.Status.ContentSHA256 = hash
	skill.Status.ReferencedBy = refs
	skill.Status.Conditions = mergeSkillConditions(skill.Status.Conditions, resolvedCond, readyCond)

	if err := r.Status().Update(ctx, &skill); err != nil {
		// Conflicts here are expected under churn; controller-runtime
		// requeues automatically.
		return ctrl.Result{}, err
	}

	if contentErr != nil {
		log.Info("skill content unresolved", "skill", skill.Name, "error", contentErr)
	}
	return ctrl.Result{}, nil
}

// resolveSkillContent reads the skill's source. configMapRef wins
// over inline. If neither is set, returns an error so the
// condition reflects the gap.
func (r *SkillReconciler) resolveSkillContent(ctx context.Context, skill *agentofficev1alpha1.Skill) (string, error) {
	src := skill.Spec.Source
	if src.ConfigMapRef != nil {
		var cm corev1.ConfigMap
		key := types.NamespacedName{Namespace: skill.Namespace, Name: src.ConfigMapRef.Name}
		if err := r.Get(ctx, key, &cm); err != nil {
			return "", fmt.Errorf("ConfigMap %s/%s: %w", key.Namespace, key.Name, err)
		}
		val, ok := cm.Data[src.ConfigMapRef.Key]
		if !ok {
			return "", fmt.Errorf("ConfigMap %s/%s missing key %q", key.Namespace, key.Name, src.ConfigMapRef.Key)
		}
		return val, nil
	}
	if src.Inline != "" {
		return src.Inline, nil
	}
	return "", fmt.Errorf("source has neither configMapRef nor inline content")
}

// resolveReferencedBy lists all SkillBindings in the namespace,
// keeps the ones whose skillRef.name matches this skill, and
// unions their AppliedTo AW lists. When a binding's status hasn't
// been populated yet (transient: created but not yet reconciled),
// we resolve subjects on the fly so the indication is correct on
// first reconcile.
func (r *SkillReconciler) resolveReferencedBy(ctx context.Context, skill *agentofficev1alpha1.Skill) ([]string, error) {
	var bindings agentofficev1alpha1.SkillBindingList
	if err := r.List(ctx, &bindings, client.InNamespace(skill.Namespace)); err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	for _, sb := range bindings.Items {
		if sb.Spec.SkillRef.Name != skill.Name {
			continue
		}
		// Prefer the binding's own resolved status when populated.
		if len(sb.Status.AppliedTo) > 0 {
			for _, n := range sb.Status.AppliedTo {
				seen[n] = struct{}{}
			}
			continue
		}
		// Fallback: resolve on the fly. Same logic the
		// SkillBinding controller uses; duplicated here so the
		// Skill controller doesn't depend on binding status
		// having converged.
		applied, err := resolveSkillBindingSubjects(ctx, r.Client, &sb)
		if err != nil {
			continue
		}
		for _, n := range applied {
			seen[n] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// mergeSkillConditions is the Skill-typed mirror of
// mergeConditions in the MemoryModule controller. Replaces
// existing conditions of the same type with the new entries;
// otherwise appends. Stable sort keeps reconcile diffs small.
func mergeSkillConditions(existing []agentofficev1alpha1.SkillCondition, updates ...agentofficev1alpha1.SkillCondition) []agentofficev1alpha1.SkillCondition {
	byType := make(map[string]agentofficev1alpha1.SkillCondition, len(existing)+len(updates))
	for _, c := range existing {
		byType[c.Type] = c
	}
	for _, c := range updates {
		if old, ok := byType[c.Type]; ok && old.Status == c.Status {
			c.LastTransitionTime = old.LastTransitionTime
		}
		byType[c.Type] = c
	}
	out := make([]agentofficev1alpha1.SkillCondition, 0, len(byType))
	keys := make([]string, 0, len(byType))
	for k := range byType {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, byType[k])
	}
	return out
}

// SetupWithManager wires up the controller. Watches:
//   - Skill (primary)
//   - ConfigMap (re-reconcile when the referenced CM changes;
//     reverse-lookup via field index on spec.source.configMapRef.name)
//   - SkillBinding (re-reconcile when bindings granting this
//     skill change — referencedBy gets stale otherwise)
func (r *SkillReconciler) SetupWithManager(mgr ctrl.Manager) error {
	const cmRefField = ".spec.source.configMapRef.name"
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &agentofficev1alpha1.Skill{}, cmRefField, func(obj client.Object) []string {
		s := obj.(*agentofficev1alpha1.Skill)
		if s.Spec.Source.ConfigMapRef != nil {
			return []string{s.Spec.Source.ConfigMapRef.Name}
		}
		return nil
	}); err != nil {
		return err
	}

	mapCM := func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list agentofficev1alpha1.SkillList
		if err := r.List(ctx, &list, &client.ListOptions{
			Namespace:     obj.GetNamespace(),
			FieldSelector: fields.OneTermEqualSelector(cmRefField, obj.GetName()),
		}); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for _, s := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
		}
		return out
	}

	// When a SkillBinding changes, re-reconcile the Skill it
	// references so its referencedBy stays accurate.
	mapSB := func(ctx context.Context, obj client.Object) []reconcile.Request {
		sb, ok := obj.(*agentofficev1alpha1.SkillBinding)
		if !ok || sb.Spec.SkillRef.Name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Namespace: sb.Namespace, Name: sb.Spec.SkillRef.Name,
		}}}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.Skill{}).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(mapCM)).
		Watches(&agentofficev1alpha1.SkillBinding{}, handler.EnqueueRequestsFromMapFunc(mapSB)).
		Named("skill").
		Complete(r)
}
