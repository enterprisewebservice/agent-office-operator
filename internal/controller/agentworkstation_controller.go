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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

// reconcileShared is a stub for the runtime.shared path. Step 3
// implements: openclaw agents create against the referenced
// AgentGateway, register browser profile, propagate Discord channel
// + system prompt into the gateway's per-agent workspace. For now
// it just records status so dashboards reflect the chosen mode.
func (r *AgentWorkstationReconciler) reconcileShared(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) (ctrl.Result, error) {
	gwRef := aw.Spec.Runtime.Shared.GatewayRef
	msg := fmt.Sprintf("runtime=shared (gatewayRef=%s) — agent provisioning is implemented in step 3", gwRef)

	if aw.Status.Phase == agentofficev1alpha1.AgentWorkstationPhasePending && aw.Status.Message == msg {
		return ctrl.Result{}, nil
	}
	aw.Status.Phase = agentofficev1alpha1.AgentWorkstationPhasePending
	aw.Status.Message = msg
	aw.Status.GatewayEndpoint = ""
	if err := r.Status().Update(ctx, aw); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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
// re-reconcile whenever an owned child changes.
func (r *AgentWorkstationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Route is unstructured; register it as a generic owned source.
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "route.openshift.io", Version: "v1", Kind: "Route",
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.AgentWorkstation{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(route).
		Named("agentworkstation").
		Complete(r)
}
