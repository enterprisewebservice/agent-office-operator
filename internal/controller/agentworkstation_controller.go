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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// AgentWorkstationReconciler observes the dependent Deployment + Route
// (currently created by agent-office-server's pseudo-controller) and
// keeps status.phase / status.gatewayEndpoint accurate.
//
// Slice 3 scope: read-only observation. Lifecycle ownership of
// ConfigMap/Secret/PVC/Deployment/Service/Route still lives in
// agent-office-server's create-flow — that migration is slice 4 once
// the operator's reconcile is proven stable here.
type AgentWorkstationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentworkstations/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch

// dependentName is the convention agent-office-server uses for the
// per-agent Deployment, Service, and Route names: prefix the AW name
// with `agent-`.
func dependentName(awName string) string { return "agent-" + awName }

// Reconcile observes the Deployment + Route and patches status.
//   - phase=Running when Deployment.status.readyReplicas >= 1
//   - phase=Creating when Deployment exists but isn't ready yet
//   - phase=Pending when no Deployment yet
//   - gatewayEndpoint=https://<route.spec.host> when the Route exists
func (r *AgentWorkstationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var aw agentofficev1alpha1.AgentWorkstation
	if err := r.Get(ctx, req.NamespacedName, &aw); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	depName := dependentName(aw.Name)

	// Observe Deployment.
	var dep appsv1.Deployment
	depErr := r.Get(ctx, types.NamespacedName{Namespace: aw.Namespace, Name: depName}, &dep)

	phase := agentofficev1alpha1.AgentWorkstationPhasePending
	message := "no Deployment yet"
	switch {
	case depErr == nil && dep.Status.ReadyReplicas >= 1:
		phase = agentofficev1alpha1.AgentWorkstationPhaseRunning
		message = fmt.Sprintf("Deployment %s ready (%d/%d)", depName, dep.Status.ReadyReplicas, dep.Status.Replicas)
	case depErr == nil:
		phase = agentofficev1alpha1.AgentWorkstationPhaseCreating
		message = fmt.Sprintf("Deployment %s present but not ready (%d/%d)", depName, dep.Status.ReadyReplicas, dep.Status.Replicas)
	case apierrors.IsNotFound(depErr):
		// keep Pending
	default:
		return ctrl.Result{}, fmt.Errorf("get Deployment %s/%s: %w", aw.Namespace, depName, depErr)
	}

	// Observe Route via unstructured so we don't take a hard dep on the
	// route.openshift.io types (the operator should still build cleanly on
	// vanilla Kubernetes for unit tests / dev).
	endpoint := ""
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	})
	routeErr := r.Get(ctx, types.NamespacedName{Namespace: aw.Namespace, Name: depName}, route)
	if routeErr == nil {
		host, _, _ := unstructured.NestedString(route.Object, "spec", "host")
		if host != "" {
			endpoint = "https://" + host
		}
	} else if !apierrors.IsNotFound(routeErr) {
		log.Info("get Route", "name", depName, "error", routeErr)
	}

	// Patch only if anything changed; status writes are cheap but log spam
	// is annoying.
	if aw.Status.Phase == phase && aw.Status.Message == message && aw.Status.GatewayEndpoint == endpoint {
		return ctrl.Result{}, nil
	}
	aw.Status.Phase = phase
	aw.Status.Message = message
	aw.Status.GatewayEndpoint = endpoint
	if err := r.Status().Update(ctx, &aw); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("status updated", "name", aw.Name, "phase", phase, "endpoint", endpoint)
	return ctrl.Result{}, nil
}

// SetupWithManager wires the controller. Watches:
//   - AgentWorkstation (primary)
//   - Deployment (re-reconcile when readyReplicas flips). Owns() doesn't
//     work here yet because agent-office-server creates the Deployment
//     without an ownerReference back to the AW (that gets fixed when the
//     operator takes ownership in slice 4). For now map by the agent-*
//     name convention.
func (r *AgentWorkstationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapDep := func(_ context.Context, obj client.Object) []reconcile.Request {
		name := obj.GetName()
		if !strings.HasPrefix(name, "agent-") {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      strings.TrimPrefix(name, "agent-"),
		}}}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.AgentWorkstation{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(mapDep)).
		Named("agentworkstation").
		Complete(r)
}
