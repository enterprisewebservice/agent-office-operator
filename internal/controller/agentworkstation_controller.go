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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// AgentWorkstationReconciler owns the per-agent runtime — ConfigMap,
// gateway-token Secret, PVC, Deployment, Service, Route — and reflects
// observed Deployment + Route state into the AW's status.
//
// Slice 4: full lifecycle. agent-office-server only creates the
// AgentWorkstation CR (and the user-supplied API-key Secret); the
// operator reconciles everything else and ownerReferences cascade
// deletion when the CR is removed.
type AgentWorkstationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// RestConfig is needed for exec into gateway pods (runtime.shared
	// path uses `openclaw config set` against a running gateway).
	// Set from main.go via mgr.GetConfig().
	RestConfig *rest.Config
}

// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete

// Reconcile renders all child resources from the AW spec and patches
// status. Deletion is implicit: ownerReferences cascade when the AW
// CR is removed.
func (r *AgentWorkstationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var aw agentofficev1alpha1.AgentWorkstation
	if err := r.Get(ctx, req.NamespacedName, &aw); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Dispatch on spec.runtime. Default (nil or runtime.dedicated)
	// preserves the slice-4 path: own Pod / PVC / Service / Route /
	// CM / Secret. The runtime.shared path defers to step-3 logic
	// which slots the AW as a logical agent inside a shared
	// AgentGateway runtime.
	if aw.Spec.Runtime != nil && aw.Spec.Runtime.Shared != nil {
		return r.reconcileShared(ctx, &aw)
	}

	// 1. Materialize the per-agent dedicated runtime.
	if err := r.reconcileChildren(ctx, &aw); err != nil {
		log.Error(err, "reconcileChildren")
		return ctrl.Result{}, err
	}

	// 2. Observe Deployment + Route → status.
	if err := r.reconcileStatus(ctx, &aw); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileShared dispatches to the full implementation in
// agentworkstation_shared.go. Kept as a small wrapper to keep the
// dispatcher in this file readable.
func (r *AgentWorkstationReconciler) reconcileShared(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) (ctrl.Result, error) {
	return r.reconcileSharedFull(ctx, aw)
}

// reconcileStatus reads the owned Deployment + Route and patches
// status.phase / status.gatewayEndpoint. Same logic as slice 3, just
// extracted for clarity.
func (r *AgentWorkstationReconciler) reconcileStatus(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) error {
	log := logf.FromContext(ctx)
	depName := deployName(aw.Name)

	var dep appsv1.Deployment
	depErr := r.Get(ctx, types.NamespacedName{Namespace: aw.Namespace, Name: depName}, &dep)

	phase := agentofficev1alpha1.AgentWorkstationPhasePending
	message := "no Deployment yet"
	switch {
	case depErr == nil && dep.Status.ReadyReplicas >= 1:
		phase = agentofficev1alpha1.AgentWorkstationPhaseRunning
		message = fmt.Sprintf("Deployment ready (%d/%d)", dep.Status.ReadyReplicas, dep.Status.Replicas)
	case depErr == nil:
		phase = agentofficev1alpha1.AgentWorkstationPhaseCreating
		message = fmt.Sprintf("Deployment present but not ready (%d/%d)", dep.Status.ReadyReplicas, dep.Status.Replicas)
	case apierrors.IsNotFound(depErr):
		// keep Pending
	default:
		return fmt.Errorf("get Deployment %s/%s: %w", aw.Namespace, depName, depErr)
	}

	endpoint := ""
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "route.openshift.io", Version: "v1", Kind: "Route",
	})
	if err := r.Get(ctx, types.NamespacedName{Namespace: aw.Namespace, Name: routeName(aw.Name)}, route); err == nil {
		host, _, _ := unstructured.NestedString(route.Object, "spec", "host")
		if host != "" {
			endpoint = "https://" + host
		}
	} else if !apierrors.IsNotFound(err) {
		log.Info("get Route", "name", routeName(aw.Name), "error", err)
	}

	if aw.Status.Phase == phase && aw.Status.Message == message && aw.Status.GatewayEndpoint == endpoint {
		return nil
	}
	aw.Status.Phase = phase
	aw.Status.Message = message
	aw.Status.GatewayEndpoint = endpoint
	if err := r.Status().Update(ctx, aw); err != nil {
		return err
	}
	log.Info("status updated", "name", aw.Name, "phase", phase, "endpoint", endpoint)
	return nil
}

// SetupWithManager wires the controller. Owns() handles the cascade —
// re-reconcile whenever an owned child changes. Watches() handles
// the cross-resource fan-out: when a SkillBinding or Skill that
// applies to this AW changes, we need to re-reconcile the AW so
// the workspace SKILL_*.md files stay in sync.
func (r *AgentWorkstationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Route is unstructured; register it as a generic owned source.
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "route.openshift.io", Version: "v1", Kind: "Route",
	})

	// SkillBinding fan-out: enqueue every AW the binding could
	// possibly apply to (direct subjects + selector matches). On
	// binding delete the same names get enqueued so the AW
	// reconciler removes the corresponding SKILL_*.md from the
	// workspace via the GC step.
	mapSkillBinding := func(ctx context.Context, obj client.Object) []reconcile.Request {
		sb, ok := obj.(*agentofficev1alpha1.SkillBinding)
		if !ok {
			return nil
		}
		seen := map[client.ObjectKey]struct{}{}
		for _, s := range sb.Spec.Subjects {
			if s.Kind != "AgentWorkstation" {
				continue
			}
			seen[client.ObjectKey{Namespace: sb.Namespace, Name: s.Name}] = struct{}{}
		}
		if sb.Spec.SubjectSelector != nil {
			ls := sb.Spec.SubjectSelector
			if len(ls.MatchLabels) > 0 || len(ls.MatchExpressions) > 0 {
				sel, err := metav1.LabelSelectorAsSelector(ls)
				if err == nil {
					var aws agentofficev1alpha1.AgentWorkstationList
					if err := r.List(ctx, &aws,
						client.InNamespace(sb.Namespace),
						client.MatchingLabelsSelector{Selector: sel},
					); err == nil {
						for _, aw := range aws.Items {
							seen[client.ObjectKey{Namespace: aw.Namespace, Name: aw.Name}] = struct{}{}
						}
					}
				}
			}
		}
		out := make([]reconcile.Request, 0, len(seen))
		for k := range seen {
			out = append(out, reconcile.Request{NamespacedName: k})
		}
		return out
	}

	// Skill fan-out: when a Skill changes, re-reconcile every AW
	// that's currently bound to it. Cheaper than re-resolving
	// every binding's subject set: just iterate the AWs whose
	// status references this skill. Walk SkillBindings and union
	// their applied lists for the matching skillRef.
	mapSkill := func(ctx context.Context, obj client.Object) []reconcile.Request {
		sk, ok := obj.(*agentofficev1alpha1.Skill)
		if !ok {
			return nil
		}
		var bindings agentofficev1alpha1.SkillBindingList
		if err := r.List(ctx, &bindings, client.InNamespace(sk.Namespace)); err != nil {
			return nil
		}
		seen := map[client.ObjectKey]struct{}{}
		for _, sb := range bindings.Items {
			if sb.Spec.SkillRef.Name != sk.Name {
				continue
			}
			for _, n := range sb.Status.AppliedTo {
				seen[client.ObjectKey{Namespace: sk.Namespace, Name: n}] = struct{}{}
			}
		}
		out := make([]reconcile.Request, 0, len(seen))
		for k := range seen {
			out = append(out, reconcile.Request{NamespacedName: k})
		}
		return out
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.AgentWorkstation{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(route).
		Watches(&agentofficev1alpha1.SkillBinding{}, handler.EnqueueRequestsFromMapFunc(mapSkillBinding)).
		Watches(&agentofficev1alpha1.Skill{}, handler.EnqueueRequestsFromMapFunc(mapSkill)).
		Named("agentworkstation").
		Complete(r)
}
