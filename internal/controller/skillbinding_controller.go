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
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// SkillBindingReconciler resolves a SkillBinding's subjects
// (direct + selector) and Skill reference, surfaces the resolved
// state in `status.appliedTo` / `status.resolvedSkillVersion`, and
// flags conflicts with other bindings overlapping on the same
// (AgentWorkstation, Skill) pair so users see overlap they should
// clean up.
type SkillBindingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentoffice.ai,resources=skillbindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentoffice.ai,resources=skillbindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=skillbindings/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentoffice.ai,resources=skills,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations,verbs=get;list;watch

// Reconcile resolves subjects + skill ref and updates status.
// Conflicts don't block — they're informational so users see
// overlap. The AgentWorkstation reconciler deterministically
// picks a winner (lex-sorted by binding name, last-wins) at
// reconcile time.
func (r *SkillBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var sb agentofficev1alpha1.SkillBinding
	if err := r.Get(ctx, req.NamespacedName, &sb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1. Resolve subjects (direct + selector → unioned AW names).
	applied, subjErr := resolveSkillBindingSubjects(ctx, r.Client, &sb)

	// 2. Resolve the Skill ref so we can populate
	//    resolvedSkillVersion and surface a SkillResolved
	//    condition. Missing or version-mismatched Skill is a hard
	//    error — this binding can't be applied — but we still
	//    update status so the UI shows the failure.
	var skill agentofficev1alpha1.Skill
	skillErr := r.Get(ctx, types.NamespacedName{
		Namespace: sb.Namespace, Name: sb.Spec.SkillRef.Name,
	}, &skill)

	resolvedVersion := ""
	if skillErr == nil {
		switch {
		case sb.Spec.SkillRef.Version == "":
			// Floating ref — track whatever the Skill currently declares.
			if skill.Spec.Version != "" {
				resolvedVersion = "floating:" + skill.Spec.Version
			} else {
				resolvedVersion = "floating"
			}
		case sb.Spec.SkillRef.Version == skill.Spec.Version:
			resolvedVersion = sb.Spec.SkillRef.Version
		default:
			// Version pinned but doesn't match the Skill's
			// current spec.version. Treat as a resolution error.
			skillErr = fmt.Errorf("skill version mismatch: pinned %q but Skill %q reports %q",
				sb.Spec.SkillRef.Version, skill.Name, skill.Spec.Version)
		}
	}

	// 3. Detect conflicts: other SkillBindings in this namespace
	//    granting the same skill to any AW in our applied set.
	conflicts, err := detectSkillBindingConflicts(ctx, r.Client, &sb, applied)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("detecting conflicts: %w", err)
	}

	// 4. Build status patch.
	now := metav1.Now()
	subjectsCond := agentofficev1alpha1.SkillBindingCondition{
		Type:               "SubjectsResolved",
		Status:             "True",
		Reason:             "Resolved",
		Message:            fmt.Sprintf("Resolved %d AgentWorkstation(s)", len(applied)),
		LastTransitionTime: now,
	}
	if subjErr != nil {
		subjectsCond.Status = "False"
		subjectsCond.Reason = "ResolveError"
		subjectsCond.Message = subjErr.Error()
	}
	skillCond := agentofficev1alpha1.SkillBindingCondition{
		Type:               "SkillResolved",
		Status:             "True",
		Reason:             "Resolved",
		Message:            fmt.Sprintf("Skill %q resolved (version=%s)", sb.Spec.SkillRef.Name, resolvedVersion),
		LastTransitionTime: now,
	}
	if skillErr != nil {
		skillCond.Status = "False"
		skillCond.Reason = "SkillNotFound"
		skillCond.Message = skillErr.Error()
	}
	readyStatus := "True"
	readyReason := "Resolved"
	readyMsg := fmt.Sprintf("Applied to %d AgentWorkstation(s)", len(applied))
	if subjErr != nil || skillErr != nil {
		readyStatus = "False"
		readyReason = "ResolveError"
		if skillErr != nil {
			readyMsg = skillErr.Error()
		} else {
			readyMsg = subjErr.Error()
		}
	}
	readyCond := agentofficev1alpha1.SkillBindingCondition{
		Type:               "Ready",
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMsg,
		LastTransitionTime: now,
	}

	sb.Status.AppliedTo = applied
	sb.Status.ResolvedSkillVersion = resolvedVersion
	sb.Status.Conflicts = conflicts
	sb.Status.Conditions = mergeSkillBindingConditions(sb.Status.Conditions, subjectsCond, skillCond, readyCond)

	if err := r.Status().Update(ctx, &sb); err != nil {
		return ctrl.Result{}, err
	}

	if subjErr != nil {
		log.Info("subject resolution failed", "binding", sb.Name, "error", subjErr)
	}
	if skillErr != nil {
		log.Info("skill resolution failed", "binding", sb.Name, "skill", sb.Spec.SkillRef.Name, "error", skillErr)
	}
	return ctrl.Result{}, nil
}

// resolveSkillBindingSubjects expands a binding's direct subjects
// + selector into a deduplicated, sorted list of AgentWorkstation
// names in the binding's namespace. Subjects that don't exist as
// AWs in the namespace are filtered out (silent — surfaced via
// the SubjectsResolved count).
//
// Exported (package-level) so the Skill controller and the AW
// reconciler can reuse it without duplicating the logic.
func resolveSkillBindingSubjects(ctx context.Context, c client.Client, sb *agentofficev1alpha1.SkillBinding) ([]string, error) {
	// 1. List existing AWs in the namespace once. We need them
	//    for both selector matching and to filter direct
	//    subjects to actually-present names.
	var aws agentofficev1alpha1.AgentWorkstationList
	if err := c.List(ctx, &aws, client.InNamespace(sb.Namespace)); err != nil {
		return nil, fmt.Errorf("listing AgentWorkstations: %w", err)
	}
	awByName := make(map[string]*agentofficev1alpha1.AgentWorkstation, len(aws.Items))
	for i := range aws.Items {
		awByName[aws.Items[i].Name] = &aws.Items[i]
	}

	matched := map[string]struct{}{}

	// 2. Direct subjects.
	for _, s := range sb.Spec.Subjects {
		if s.Kind != "AgentWorkstation" {
			continue
		}
		if _, ok := awByName[s.Name]; ok {
			matched[s.Name] = struct{}{}
		}
	}

	// 3. Selector. Empty selector matches none (LabelSelector
	//    semantics around nil vs empty are subtle — we treat nil
	//    as "no selector at all"; empty matchLabels would match
	//    everything which is almost certainly a typo at this
	//    scale, so require at least one label / matchExpression).
	if sb.Spec.SubjectSelector != nil {
		ls := sb.Spec.SubjectSelector
		hasCriteria := len(ls.MatchLabels) > 0 || len(ls.MatchExpressions) > 0
		if hasCriteria {
			sel, err := metav1.LabelSelectorAsSelector(ls)
			if err != nil {
				return nil, fmt.Errorf("invalid subjectSelector: %w", err)
			}
			for name, aw := range awByName {
				if sel.Matches(labels.Set(aw.Labels)) {
					matched[name] = struct{}{}
				}
			}
		}
	}

	out := make([]string, 0, len(matched))
	for n := range matched {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// detectSkillBindingConflicts scans other SkillBindings in the
// same namespace for any that grant THIS binding's skill to any
// AW also covered by this binding. Self is excluded.
func detectSkillBindingConflicts(ctx context.Context, c client.Client, sb *agentofficev1alpha1.SkillBinding, applied []string) ([]agentofficev1alpha1.SkillBindingConflict, error) {
	var bindings agentofficev1alpha1.SkillBindingList
	if err := c.List(ctx, &bindings, client.InNamespace(sb.Namespace)); err != nil {
		return nil, err
	}

	appliedSet := make(map[string]struct{}, len(applied))
	for _, n := range applied {
		appliedSet[n] = struct{}{}
	}

	overlapByAW := map[string][]string{}
	for _, other := range bindings.Items {
		if other.Name == sb.Name {
			continue
		}
		if other.Spec.SkillRef.Name != sb.Spec.SkillRef.Name {
			continue
		}
		// Use the other binding's reported AppliedTo; fall back
		// to live resolution if its status hasn't converged yet.
		otherApplied := other.Status.AppliedTo
		if len(otherApplied) == 0 {
			ap, err := resolveSkillBindingSubjects(ctx, c, &other)
			if err != nil {
				continue
			}
			otherApplied = ap
		}
		for _, n := range otherApplied {
			if _, hit := appliedSet[n]; hit {
				overlapByAW[n] = append(overlapByAW[n], other.Name)
			}
		}
	}

	out := make([]agentofficev1alpha1.SkillBindingConflict, 0, len(overlapByAW))
	awNames := make([]string, 0, len(overlapByAW))
	for n := range overlapByAW {
		awNames = append(awNames, n)
	}
	sort.Strings(awNames)
	for _, n := range awNames {
		bs := overlapByAW[n]
		sort.Strings(bs)
		out = append(out, agentofficev1alpha1.SkillBindingConflict{
			AgentWorkstation: n,
			OverlapsWith:     bs,
		})
	}
	return out, nil
}

// mergeSkillBindingConditions mirrors mergeConditions for
// SkillBinding's typed condition list.
func mergeSkillBindingConditions(existing []agentofficev1alpha1.SkillBindingCondition, updates ...agentofficev1alpha1.SkillBindingCondition) []agentofficev1alpha1.SkillBindingCondition {
	byType := make(map[string]agentofficev1alpha1.SkillBindingCondition, len(existing)+len(updates))
	for _, c := range existing {
		byType[c.Type] = c
	}
	for _, c := range updates {
		if old, ok := byType[c.Type]; ok && old.Status == c.Status {
			c.LastTransitionTime = old.LastTransitionTime
		}
		byType[c.Type] = c
	}
	out := make([]agentofficev1alpha1.SkillBindingCondition, 0, len(byType))
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
//   - SkillBinding (primary)
//   - Skill (re-reconcile bindings whose skillRef.name matches)
//   - AgentWorkstation (re-reconcile every binding in the AW's
//     namespace — its labels may have changed, affecting selector
//     matches; cheap because bindings/ns is small)
func (r *SkillBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapSkill := func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list agentofficev1alpha1.SkillBindingList
		if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for _, b := range list.Items {
			if b.Spec.SkillRef.Name == obj.GetName() {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&b)})
			}
		}
		return out
	}

	mapAW := func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list agentofficev1alpha1.SkillBindingList
		if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for _, b := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&b)})
		}
		return out
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.SkillBinding{}).
		Watches(&agentofficev1alpha1.Skill{}, handler.EnqueueRequestsFromMapFunc(mapSkill)).
		Watches(&agentofficev1alpha1.AgentWorkstation{}, handler.EnqueueRequestsFromMapFunc(mapAW)).
		Named("skillbinding").
		Complete(r)
}
