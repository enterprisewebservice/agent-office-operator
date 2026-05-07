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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// KnowledgeBaseReconciler owns the wiki PVC and tracks which
// AgentGateway is currently mounting it. The actual mount on the
// gateway pod is handled by the AgentGatewayReconciler — this
// controller's job is provisioning storage and surfacing status
// (so users can see "is my wiki ready?" via `oc get kb`).
type KnowledgeBaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentoffice.ai,resources=knowledgebases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentoffice.ai,resources=knowledgebases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=knowledgebases/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentgateways,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile provisions the wiki PVC and updates status. It does
// NOT modify the gateway Deployment — the AgentGatewayReconciler
// watches KnowledgeBase events and adds the volume/mount on its
// own reconcile pass. Splitting it this way keeps each
// controller's surface narrow and makes the cascade easier to
// reason about.
func (r *KnowledgeBaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var kb agentofficev1alpha1.KnowledgeBase
	if err := r.Get(ctx, req.NamespacedName, &kb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1. Provision PVC. Idempotent via CreateOrUpdate.
	pvcName := kbPVCName(kb.Name)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: kb.Namespace,
			Name:      pvcName,
		},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		size := defaultKBSize()
		if kb.Spec.Storage.Size != nil {
			size = *kb.Spec.Storage.Size
		}
		modes := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		if len(kb.Spec.Storage.AccessModes) > 0 {
			modes = kb.Spec.Storage.AccessModes
		}
		// Spec is mostly immutable post-bind; only set fields on
		// initial create. Updating an existing PVC's Spec
		// triggers an immutable-field error from the API server,
		// which CreateOrUpdate would surface as a non-fatal
		// patch failure — we'd rather just no-op.
		if pvc.CreationTimestamp.IsZero() {
			pvc.Spec.AccessModes = modes
			pvc.Spec.Resources = corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: size,
				},
			}
			if kb.Spec.Storage.StorageClassName != "" {
				sc := kb.Spec.Storage.StorageClassName
				pvc.Spec.StorageClassName = &sc
			}
		}
		// Labels are mutable and worth keeping fresh so the UI
		// can group resources by KB.
		if pvc.Labels == nil {
			pvc.Labels = map[string]string{}
		}
		pvc.Labels["agentoffice.ai/knowledgebase"] = kb.Name
		pvc.Labels["app.kubernetes.io/managed-by"] = "agent-office-operator"
		// Owner ref so PVC is GC'd when the KB is deleted.
		return controllerutil.SetControllerReference(&kb, pvc, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure PVC %s/%s: %w", kb.Namespace, pvcName, err)
	}
	if op != controllerutil.OperationResultNone {
		log.Info("KB PVC reconciled", "op", string(op), "pvc", pvcName)
	}

	// 1b. If GitMirror is configured, reconcile the per-KB
	//     ConfigMap holding the sync script + bootstrap
	//     templates. The gateway controller mounts this CM
	//     into the git-sync sidecar container it adds to the
	//     gateway pod. Idempotent — no-op when the CM matches.
	if err := r.reconcileGitSyncConfigMap(ctx, &kb); err != nil {
		log.Info("git-sync CM reconcile failed", "err", err)
		// non-fatal — KB still works without mirroring; status
		// will surface the gap on the next gateway-side reconcile
	}

	// 2. Resolve gateway binding (informational — the gateway
	//    controller does the actual mount). We surface the
	//    bound gateway and the count of logical agents in it
	//    so users can see "this KB is shared with N agents".
	boundGateway := ""
	agentCount := int32(0)
	if kb.Spec.GatewayRef.Name != "" {
		var gw agentofficev1alpha1.AgentGateway
		gwErr := r.Get(ctx, types.NamespacedName{
			Namespace: kb.Namespace, Name: kb.Spec.GatewayRef.Name,
		}, &gw)
		if gwErr == nil {
			boundGateway = gw.Name
			agentCount = gw.Status.AgentCount
		}
	}

	// 3. Status patch. Always set deterministic fields
	//    (PVCName, MountPath) even when the gateway isn't
	//    found yet — the binding is "best effort" and the KB
	//    PVC is useful on its own.
	now := metav1.Now()
	pvcCond := agentofficev1alpha1.KnowledgeBaseCondition{
		Type:               "PVCReady",
		Status:             "True",
		Reason:             "Provisioned",
		Message:            fmt.Sprintf("PVC %s ready", pvcName),
		LastTransitionTime: now,
	}
	gwCond := agentofficev1alpha1.KnowledgeBaseCondition{
		Type:               "GatewayMounted",
		Status:             "Unknown",
		Reason:             "AwaitingGatewayReconcile",
		Message:            "AgentGateway controller will mount the wiki PVC on next reconcile",
		LastTransitionTime: now,
	}
	if boundGateway != "" {
		gwCond.Status = "True"
		gwCond.Reason = "Bound"
		gwCond.Message = fmt.Sprintf("KB attached to AgentGateway %q (%d agents)", boundGateway, agentCount)
	} else if kb.Spec.GatewayRef.Name != "" {
		gwCond.Status = "False"
		gwCond.Reason = "GatewayNotFound"
		gwCond.Message = fmt.Sprintf("AgentGateway %q not found in namespace %s", kb.Spec.GatewayRef.Name, kb.Namespace)
	}
	readyCond := agentofficev1alpha1.KnowledgeBaseCondition{
		Type:               "Ready",
		Status:             pvcCond.Status,
		Reason:             pvcCond.Reason,
		Message:            pvcCond.Message,
		LastTransitionTime: now,
	}

	kb.Status.PVCName = pvcName
	kb.Status.MountPath = kbMountPath(kb.Name)
	kb.Status.BoundGateway = boundGateway
	kb.Status.AgentCount = agentCount
	kb.Status.Conditions = mergeKBConditions(kb.Status.Conditions, pvcCond, gwCond, readyCond)
	if err := r.Status().Update(ctx, &kb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// kbPVCName is the PVC naming convention: "<kb-name>-wiki".
// Exported (package-level) so the gateway controller uses the
// same name when adding the volume.
func kbPVCName(kbName string) string {
	return kbName + "-wiki"
}

// kbMountPath is where the KB lands inside the gateway pod.
// All agents in the gateway see the wiki at this path.
func kbMountPath(kbName string) string {
	return "/home/node/.openclaw/wiki/" + kbName
}

// kbVolumeName is the Deployment-pod-spec volume name. Stable
// across reconciles so we can find + update it idempotently.
func kbVolumeName(kbName string) string {
	return "kb-" + kbName
}

func defaultKBSize() resource.Quantity {
	q := resource.MustParse("5Gi")
	return q
}

// mergeKBConditions mirrors mergeConditions for the KB-typed list.
func mergeKBConditions(existing []agentofficev1alpha1.KnowledgeBaseCondition, updates ...agentofficev1alpha1.KnowledgeBaseCondition) []agentofficev1alpha1.KnowledgeBaseCondition {
	byType := make(map[string]agentofficev1alpha1.KnowledgeBaseCondition, len(existing)+len(updates))
	for _, c := range existing {
		byType[c.Type] = c
	}
	for _, c := range updates {
		if old, ok := byType[c.Type]; ok && old.Status == c.Status {
			c.LastTransitionTime = old.LastTransitionTime
		}
		byType[c.Type] = c
	}
	out := make([]agentofficev1alpha1.KnowledgeBaseCondition, 0, len(byType))
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

// SetupWithManager wires the controller. Watches:
//   - KnowledgeBase (primary)
//   - AgentGateway (re-reconcile any KB whose gatewayRef matches
//     when the gateway changes — agentCount affects KB status)
func (r *KnowledgeBaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapGW := func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list agentofficev1alpha1.KnowledgeBaseList
		if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for _, kb := range list.Items {
			if kb.Spec.GatewayRef.Name == obj.GetName() {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&kb)})
			}
		}
		return out
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.KnowledgeBase{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&agentofficev1alpha1.AgentGateway{}, handler.EnqueueRequestsFromMapFunc(mapGW)).
		Named("knowledgebase").
		Complete(r)
}
