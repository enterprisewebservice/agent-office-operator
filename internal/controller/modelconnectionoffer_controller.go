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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// ModelConnectionOfferReconciler projects every published ModelConnection
// into the tenant workspaces its access list admits, as
// ModelConnectionOffers (one per connection per workspace). A workspace's
// offer list is therefore the same menu the Developer Hub shows its
// principal, readable with a namespaced `oc get`, while the
// cluster-scoped ModelConnection list stays an administrator's view.
//
// Offers follow three inputs: the connection (spec, access list,
// deletion), the tenant namespaces (annotations added, changed or
// removed) and the offers themselves (a deleted offer is re-projected).
type ModelConnectionOfferReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentoffice.ai,resources=modelconnectionoffers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentoffice.ai,resources=modelconnectionoffers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=modelconnections,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile converges the offers of one ModelConnection across all
// tenant workspaces.
func (r *ModelConnectionOfferReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("connection", req.Name)

	var conn agentofficev1alpha1.ModelConnection
	live := true
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, &conn); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		live = false
	}
	if live && !conn.DeletionTimestamp.IsZero() {
		live = false
	}

	// Where the connection should be offered.
	wanted := map[string]string{} // namespace -> what matched
	if live {
		var nsList corev1.NamespaceList
		if err := r.List(ctx, &nsList); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing namespaces: %w", err)
		}
		for i := range nsList.Items {
			ns := &nsList.Items[i]
			if !ns.DeletionTimestamp.IsZero() {
				continue
			}
			p, ok := tenantOf(ns)
			if !ok {
				continue
			}
			if offered, by := connectionOfferedTo(&conn, p); offered {
				wanted[ns.Name] = by
			}
		}
	}

	// Withdraw offers that are no longer wanted (access narrowed,
	// namespace no longer a tenant or no longer admitted, connection
	// gone — garbage collection also covers the last case).
	var existing agentofficev1alpha1.ModelConnectionOfferList
	if err := r.List(ctx, &existing, client.MatchingLabels{agentofficev1alpha1.OfferConnectionLabel: req.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing offers: %w", err)
	}
	for i := range existing.Items {
		o := &existing.Items[i]
		if _, keep := wanted[o.Namespace]; keep {
			continue
		}
		if err := r.Delete(ctx, o); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("withdrawing offer %s/%s: %w", o.Namespace, o.Name, err)
		}
		log.Info("model connection offer withdrawn", "namespace", o.Namespace)
	}

	// Project or refresh the wanted offers.
	for nsName, by := range wanted {
		offer := &agentofficev1alpha1.ModelConnectionOffer{
			ObjectMeta: metav1.ObjectMeta{Name: conn.Name, Namespace: nsName},
		}
		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, offer, func() error {
			if offer.Labels == nil {
				offer.Labels = map[string]string{}
			}
			offer.Labels[agentofficev1alpha1.OfferConnectionLabel] = conn.Name
			offer.Labels["app.kubernetes.io/managed-by"] = "agent-office-operator"
			offer.Spec = offerSpecFor(&conn, by)
			return controllerutil.SetControllerReference(&conn, offer, r.Scheme)
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("projecting offer into %s: %w", nsName, err)
		}
		if op != controllerutil.OperationResultNone {
			log.Info("model connection offer "+string(op), "namespace", nsName, "offeredTo", by)
		}
		if offer.Status.ConnectionGeneration != conn.Generation {
			offer.Status.ConnectionGeneration = conn.Generation
			if err := r.Status().Update(ctx, offer); err != nil && !apierrors.IsNotFound(err) {
				log.Info("offer status not updated", "namespace", nsName, "err", err)
			}
		}
	}
	return ctrl.Result{}, nil
}

// isTenantNamespace reports whether a namespace object carries the
// tenant annotations.
func isTenantNamespace(obj client.Object) bool {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return false
	}
	_, t := tenantOf(ns)
	return t
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModelConnectionOfferReconciler) SetupWithManager(mgr ctrl.Manager) error {
	allConnections := func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list agentofficev1alpha1.ModelConnectionList
		if err := r.List(ctx, &list); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for _, c := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name}})
		}
		return out
	}
	// Only namespaces that are, or were, tenant workspaces matter: a
	// removed annotation must withdraw offers, so Update looks at both.
	tenantNamespaces := predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return isTenantNamespace(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return isTenantNamespace(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return isTenantNamespace(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return isTenantNamespace(e.ObjectOld) || isTenantNamespace(e.ObjectNew)
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.ModelConnection{}).
		Owns(&agentofficev1alpha1.ModelConnectionOffer{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(allConnections), builder.WithPredicates(tenantNamespaces)).
		Named("modelconnectionoffer").
		Complete(r)
}
