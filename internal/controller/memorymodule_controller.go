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

// MemoryModuleReconciler resolves a MemoryModule's source content, computes
// a SHA-256 hash, and indexes which AgentWorkstations consume it.
type MemoryModuleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentoffice.ai,resources=memorymodules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentoffice.ai,resources=memorymodules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=memorymodules/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile resolves the module's source content and updates status:
//   - status.contentSha256: SHA-256 of the resolved .md content (so the UI
//     can show "shared with N agents" via reverse-index across modules
//     that hash to the same value).
//   - status.referencedBy: AgentWorkstation names (in this namespace) whose
//     spec.memory.modules contains this module.
//   - status.conditions: ContentResolved + Ready, standard k8s shape.
func (r *MemoryModuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var mod agentofficev1alpha1.MemoryModule
	if err := r.Get(ctx, req.NamespacedName, &mod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1. Resolve content (configMapRef takes precedence over inline).
	content, contentErr := r.resolveContent(ctx, &mod)

	// 2. Compute hash. Empty content still produces a hash; the
	//    ContentResolved condition tracks whether the resolution succeeded.
	hash := ""
	if contentErr == nil {
		sum := sha256.Sum256([]byte(content))
		hash = hex.EncodeToString(sum[:])
	}

	// 3. Index AgentWorkstations whose spec.memory.modules references this
	//    module by name. Same-namespace only — modules are per-namespace.
	var awList agentofficev1alpha1.AgentWorkstationList
	if err := r.List(ctx, &awList, client.InNamespace(mod.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing AgentWorkstations: %w", err)
	}
	refs := make([]string, 0)
	for _, aw := range awList.Items {
		if aw.Spec.Memory == nil {
			continue
		}
		for _, ref := range aw.Spec.Memory.Modules {
			if ref.Name == mod.Name {
				refs = append(refs, aw.Name)
				break
			}
		}
	}
	sort.Strings(refs)

	// 4. Build status patch.
	now := metav1.Now()
	resolvedCond := agentofficev1alpha1.MemoryModuleCondition{
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
	readyCond := agentofficev1alpha1.MemoryModuleCondition{
		Type:               "Ready",
		Status:             resolvedCond.Status,
		Reason:             resolvedCond.Reason,
		Message:            resolvedCond.Message,
		LastTransitionTime: now,
	}

	mod.Status.ContentSHA256 = hash
	mod.Status.ReferencedBy = refs
	mod.Status.Conditions = mergeConditions(mod.Status.Conditions, resolvedCond, readyCond)

	if err := r.Status().Update(ctx, &mod); err != nil {
		// Conflicts here are expected under churn; controller-runtime
		// requeues automatically.
		return ctrl.Result{}, err
	}

	if contentErr != nil {
		log.Info("module content unresolved", "module", mod.Name, "error", contentErr)
	}
	return ctrl.Result{}, nil
}

// resolveContent reads the module's source. configMapRef wins over inline.
// If neither is set, returns an error so the condition reflects the gap.
func (r *MemoryModuleReconciler) resolveContent(ctx context.Context, mod *agentofficev1alpha1.MemoryModule) (string, error) {
	src := mod.Spec.Source
	if src.ConfigMapRef != nil {
		var cm corev1.ConfigMap
		key := types.NamespacedName{Namespace: mod.Namespace, Name: src.ConfigMapRef.Name}
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

// mergeConditions replaces existing conditions of the same type with the
// new entries; otherwise appends. Stable order keeps reconcile diffs small.
func mergeConditions(existing []agentofficev1alpha1.MemoryModuleCondition, updates ...agentofficev1alpha1.MemoryModuleCondition) []agentofficev1alpha1.MemoryModuleCondition {
	byType := make(map[string]agentofficev1alpha1.MemoryModuleCondition, len(existing)+len(updates))
	for _, c := range existing {
		byType[c.Type] = c
	}
	for _, c := range updates {
		// Preserve LastTransitionTime if Status didn't change.
		if old, ok := byType[c.Type]; ok && old.Status == c.Status {
			c.LastTransitionTime = old.LastTransitionTime
		}
		byType[c.Type] = c
	}
	out := make([]agentofficev1alpha1.MemoryModuleCondition, 0, len(byType))
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
//   - MemoryModule (primary)
//   - ConfigMap (re-reconcile when the referenced CM changes; reverse-lookup
//     uses an index on spec.source.configMapRef.name)
//   - AgentWorkstation (re-reconcile when an agent's spec.memory.modules
//     changes; reverse-lookup uses an index on spec.memory.modules[].name)
func (r *MemoryModuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	const cmRefField = ".spec.source.configMapRef.name"
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &agentofficev1alpha1.MemoryModule{}, cmRefField, func(obj client.Object) []string {
		mod := obj.(*agentofficev1alpha1.MemoryModule)
		if mod.Spec.Source.ConfigMapRef != nil {
			return []string{mod.Spec.Source.ConfigMapRef.Name}
		}
		return nil
	}); err != nil {
		return err
	}

	mapCM := func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list agentofficev1alpha1.MemoryModuleList
		if err := r.List(ctx, &list, &client.ListOptions{
			Namespace:     obj.GetNamespace(),
			FieldSelector: fields.OneTermEqualSelector(cmRefField, obj.GetName()),
		}); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for _, m := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&m)})
		}
		return out
	}

	mapAW := func(ctx context.Context, obj client.Object) []reconcile.Request {
		aw, ok := obj.(*agentofficev1alpha1.AgentWorkstation)
		if !ok || aw.Spec.Memory == nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(aw.Spec.Memory.Modules))
		for _, ref := range aw.Spec.Memory.Modules {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: aw.Namespace,
				Name:      ref.Name,
			}})
		}
		return out
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.MemoryModule{}).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(mapCM)).
		Watches(&agentofficev1alpha1.AgentWorkstation{}, handler.EnqueueRequestsFromMapFunc(mapAW)).
		Named("memorymodule").
		Complete(r)
}
