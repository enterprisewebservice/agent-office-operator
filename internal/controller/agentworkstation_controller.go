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
	"errors"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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

// awSharedGatewayFinalizer guards shared-runtime AgentWorkstations.
// A shared agent is registered into the gateway's openclaw.json via
// exec (agents.list[], browser allowProfiles[], bindings) plus a seeded
// workspace dir — NONE of which are owned resources, so ownerReference
// cascade does not clean them up on delete. The finalizer de-registers
// the agent before the CR is removed (the bug the integration suite
// caught). Dedicated-runtime AWs don't carry it — their Pod/PVC/Service
// children cascade via ownerReferences.
const awSharedGatewayFinalizer = "agentoffice.ai/shared-gateway-deregister"

// errGatewayUnavailable signals the gateway (or a ready pod) is already
// gone, so there's nothing left to de-register — the finalizer can be
// dropped without blocking deletion forever.
var errGatewayUnavailable = errors.New("shared gateway unavailable — nothing to de-register")

// Reconcile registers the agent as a persona on its
// runtime.shared.gatewayRef AgentGateway (reconcileSharedFull). On
// delete, a finalizer de-registers it from that gateway — the same for
// every agent, since a gateway can outlive any one agent it hosts (see
// awSharedGatewayFinalizer). The gateway itself is a declarative
// resource the operator never creates or owns.
func (r *AgentWorkstationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var aw agentofficev1alpha1.AgentWorkstation
	if err := r.Get(ctx, req.NamespacedName, &aw); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// --- Deletion: run the de-register finalizer for shared agents. ---
	if !aw.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&aw, awSharedGatewayFinalizer) {
			if err := r.deregisterSharedAgent(ctx, &aw); err != nil {
				if errors.Is(err, errGatewayUnavailable) {
					log.Info("gateway gone on delete; nothing to de-register", "aw", aw.Name)
				} else {
					log.Error(err, "de-register from shared gateway failed; will retry", "aw", aw.Name)
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
			}
			controllerutil.RemoveFinalizer(&aw, awSharedGatewayFinalizer)
			if err := r.Update(ctx, &aw); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// --- Ensure the de-register finalizer on EVERY AW. ---
	// All agents register a persona on a gateway, so all need to
	// de-register on delete (the gateway may outlive the agent — a
	// shared gateway, or a dedicated one others have joined).
	if !controllerutil.ContainsFinalizer(&aw, awSharedGatewayFinalizer) {
		controllerutil.AddFinalizer(&aw, awSharedGatewayFinalizer)
		if err := r.Update(ctx, &aw); err != nil {
			return ctrl.Result{}, err
		}
		// The Update re-enqueues; fall through and reconcile this pass too.
	}

	// Runtime — UNIFIED, ONE PATH. Every agent is a logical openclaw
	// persona registered on the AgentGateway named by its
	// runtime.shared.gatewayRef. The operator never mints or owns a
	// gateway: a "dedicated" agent simply has gatewayRef pointing at an
	// AgentGateway that its OWN gitops repo also declares (see the
	// scaffolder template), so there is no shared-vs-dedicated branch
	// here at all. reconcileSharedFull surfaces a clear status if the
	// referenced gateway isn't present yet and requeues.
	return r.reconcileShared(ctx, &aw)
}

// reconcileShared dispatches to the full implementation in
// agentworkstation_shared.go. Kept as a small wrapper to keep the
// dispatcher in this file readable.
func (r *AgentWorkstationReconciler) reconcileShared(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) (ctrl.Result, error) {
	return r.reconcileSharedFull(ctx, aw)
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

	// KnowledgeBase fan-out: when a KB attached to a gateway
	// changes, every shared-runtime AW in that gateway needs a
	// fresh WIKI.md render. Find AWs by their
	// runtime.shared.gatewayRef matching this KB's gatewayRef.
	mapKB := func(ctx context.Context, obj client.Object) []reconcile.Request {
		kb, ok := obj.(*agentofficev1alpha1.KnowledgeBase)
		if !ok || kb.Spec.GatewayRef.Name == "" {
			return nil
		}
		var aws agentofficev1alpha1.AgentWorkstationList
		if err := r.List(ctx, &aws, client.InNamespace(kb.Namespace)); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0)
		for _, aw := range aws.Items {
			if effectiveGatewayRef(&aw) == kb.Spec.GatewayRef.Name {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKey{
					Namespace: aw.Namespace, Name: aw.Name,
				}})
			}
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
		Watches(&agentofficev1alpha1.KnowledgeBase{}, handler.EnqueueRequestsFromMapFunc(mapKB)).
		Named("agentworkstation").
		Complete(r)
}
