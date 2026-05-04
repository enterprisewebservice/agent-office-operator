/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
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

// AgentGatewayReconciler owns the gateway runtime: Deployment, PVC,
// Service, Route, ConfigMap, token Secret. Multiple
// AgentWorkstations with `spec.runtime.shared.gatewayRef: <this>`
// slot in as logical openclaw agents inside this gateway's pod.
type AgentGatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentgateways,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=agentgateways/finalizers,verbs=update

func (r *AgentGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var gw agentofficev1alpha1.AgentGateway
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1. Materialize gateway runtime (Pod/PVC/Service/Route/CM/Secret).
	if err := r.reconcileGatewayChildren(ctx, &gw); err != nil {
		log.Error(err, "reconcileGatewayChildren")
		return ctrl.Result{}, err
	}

	// 2. Compute status from observed Deployment + Route + AW count.
	return r.reconcileGatewayStatus(ctx, &gw)
}

// reconcileGatewayStatus reads the owned Deployment + Route and
// counts how many AgentWorkstations reference this gateway. Updates
// status.phase / gatewayEndpoint / agentCount.
func (r *AgentGatewayReconciler) reconcileGatewayStatus(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var dep appsv1.Deployment
	depErr := r.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: gwDeployName(gw.Name)}, &dep)

	phase := agentofficev1alpha1.AgentGatewayPhasePending
	message := "no Deployment yet"
	switch {
	case depErr == nil && dep.Status.ReadyReplicas >= 1:
		phase = agentofficev1alpha1.AgentGatewayPhaseReady
		message = fmt.Sprintf("Deployment ready (%d/%d)", dep.Status.ReadyReplicas, dep.Status.Replicas)
	case depErr == nil:
		phase = agentofficev1alpha1.AgentGatewayPhaseProvisioning
		message = fmt.Sprintf("Deployment present but not ready (%d/%d)", dep.Status.ReadyReplicas, dep.Status.Replicas)
	case apierrors.IsNotFound(depErr):
		// keep Pending
	default:
		return ctrl.Result{}, fmt.Errorf("get Deployment: %w", depErr)
	}

	endpoint := ""
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"})
	if err := r.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: gwRouteName(gw.Name)}, route); err == nil {
		host, _, _ := unstructured.NestedString(route.Object, "spec", "host")
		if host != "" {
			endpoint = "https://" + host
		}
	}

	// Count AWs referencing this gateway.
	var awList agentofficev1alpha1.AgentWorkstationList
	if err := r.List(ctx, &awList, client.InNamespace(gw.Namespace)); err == nil {
		count := int32(0)
		for _, aw := range awList.Items {
			if aw.Spec.Runtime != nil && aw.Spec.Runtime.Shared != nil && aw.Spec.Runtime.Shared.GatewayRef == gw.Name {
				count++
			}
		}
		gw.Status.AgentCount = count
	}

	if gw.Status.Phase == phase && gw.Status.Message == message && gw.Status.GatewayEndpoint == endpoint {
		// only AgentCount may have changed; still update if so
	}
	gw.Status.Phase = phase
	gw.Status.Message = message
	gw.Status.GatewayEndpoint = endpoint
	if err := r.Status().Update(ctx, gw); err != nil {
		return ctrl.Result{}, err
	}
	log.V(1).Info("gateway status", "phase", phase, "endpoint", endpoint, "agents", gw.Status.AgentCount)
	return ctrl.Result{}, nil
}

func (r *AgentGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.AgentGateway{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("agentgateway").
		Complete(r)
}
