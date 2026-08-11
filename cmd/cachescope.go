/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// Cache scoping.
//
// Installed AllNamespaces, OLM sets WATCH_NAMESPACE="" and
// controller-runtime happily caches EVERY watched type across the whole
// cluster. On this cluster that meant 1928 ConfigMaps — 122 MiB of JSON
// before decoding, of which 2.3 MiB was ours; the rest is OLM heap
// dumps and marketplace catalog blobs. The operator OOMKilled at 512Mi
// during the initial informer LIST, before it served a single request.
//
// The asymmetry is the whole point: this operator's CRs may live in ANY
// namespace (a team gets its own namespace), but the core objects it
// owns — Deployment, Service, ConfigMap, Secret, PVC, Job, Route — are
// only ever created ALONGSIDE one of those CRs. So the CR informers
// stay cluster-wide and cheap, and only the expensive core-type
// informers get pinned to the namespaces that actually hold CRs.
//
// Scoping by label was the obvious alternative and it does not work
// here: the objects predate any consistent labelling (0 of 18
// Deployments and 0 of 30 ConfigMaps in agent-office carry
// managed-by), and the operator legitimately reads Secrets it did not
// create — spec.envFromSecretRef, spec.codexCredentialsSecretRef,
// gitMirror.credentialsSecretRef. A label-filtered cache turns those
// into a silent NotFound.

// coreTypes are the expensive informers. Everything else — the CRDs —
// stays cluster-wide.
func coreTypes() []client.Object {
	unstructuredOf := func(group, version, kind string) client.Object {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
		return u
	}
	return []client.Object{
		&corev1.ConfigMap{},
		&corev1.Secret{},
		&corev1.Service{},
		&corev1.PersistentVolumeClaim{},
		&appsv1.Deployment{},
		&batchv1.Job{},
		// Pods are never Owned, so nothing here declares them — but
		// finding a gateway's Ready pod Lists them through the cached
		// client, which quietly starts a cluster-wide Pod informer on
		// first use: 1259 pods, 48 MiB of JSON, for the handful we
		// actually exec into.
		&corev1.Pod{},
		unstructuredOf("route.openshift.io", "v1", "Route"),
		unstructuredOf("mcp.kuadrant.io", "v1alpha1", "MCPServerRegistration"),
		unstructuredOf("argoproj.io", "v1alpha1", "Workflow"),
	}
}

// operatorNamespace is where this pod runs — always cached, because the
// catalog handler and the operator's own config live there.
func operatorNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "agent-office-operator"
}

// crNamespaces lists every namespace holding an agent-office CR, using a
// direct (uncached) client — this runs before the manager exists.
func crNamespaces(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme) (map[string]struct{}, error) {
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	lists := []client.ObjectList{
		&agentofficev1alpha1.AgentWorkstationList{},
		&agentofficev1alpha1.AgentGatewayList{},
		&agentofficev1alpha1.SkillList{},
		&agentofficev1alpha1.SkillBindingList{},
		&agentofficev1alpha1.KnowledgeBaseList{},
		&agentofficev1alpha1.MemoryModuleList{},
		&agentofficev1alpha1.AutoResearchProjectList{},
	}
	for _, l := range lists {
		if err := c.List(ctx, l); err != nil {
			// A CRD that is not installed yet must not stop startup.
			ctrl.Log.WithName("cache-scope").V(1).Info("skipping list", "err", err.Error())
			continue
		}
		objs, err := extractNamespaces(l)
		if err != nil {
			continue
		}
		for _, ns := range objs {
			out[ns] = struct{}{}
		}
	}
	return out, nil
}

func extractNamespaces(l client.ObjectList) ([]string, error) {
	var out []string
	err := meta.EachListItem(l, func(o runtime.Object) error {
		if co, ok := o.(client.Object); ok {
			if ns := co.GetNamespace(); ns != "" {
				out = append(out, ns)
			}
		}
		return nil
	})
	return out, err
}

// coreCacheNamespaces resolves the namespace set for the core-type
// informers.
//
// Two env vars can pin it explicitly: MANAGED_NAMESPACES, for caching a
// namespace before any CR lands in it, and WATCH_NAMESPACE, which OLM
// already uses to scope the whole cache. Either one means the scope was
// stated deliberately, so discovery is skipped and — critically — the
// watchdog is disabled. Otherwise a deliberately narrow scope with CRs
// outside it would restart the operator forever chasing namespaces it
// was told not to cache. `pinned` reports which case this was.
func coreCacheNamespaces(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme) (nss []string, pinned bool) {
	set := map[string]struct{}{operatorNamespace(): {}}

	addList := func(v string) bool {
		added := false
		for _, ns := range strings.Split(v, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				set[ns] = struct{}{}
				added = true
			}
		}
		return added
	}

	switch {
	case addList(os.Getenv("MANAGED_NAMESPACES")):
		pinned = true
	case addList(os.Getenv("WATCH_NAMESPACE")):
		pinned = true
	default:
		if found, err := crNamespaces(ctx, cfg, scheme); err == nil {
			for ns := range found {
				set[ns] = struct{}{}
			}
		} else {
			ctrl.Log.WithName("cache-scope").Error(err,
				"could not discover CR namespaces; caching operator namespace only")
		}
	}

	out := make([]string, 0, len(set))
	for ns := range set {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out, pinned
}

// coreByObject pins the expensive informers to the given namespaces.
func coreByObject(namespaces []string) map[client.Object]cache.ByObject {
	scoped := map[string]cache.Config{}
	for _, ns := range namespaces {
		scoped[ns] = cache.Config{}
	}
	byObject := map[client.Object]cache.ByObject{}
	for _, obj := range coreTypes() {
		byObject[obj] = cache.ByObject{Namespaces: scoped}
	}
	return byObject
}

// stripManagedFields drops the server-side-apply bookkeeping before an
// object enters the cache. Nothing here reads it and on busy objects it
// is a large fraction of the payload.
//
// Upstream's helper rather than a hand-rolled one: it nil-checks before
// clearing, which sidesteps kubernetes/kubernetes#124337.
func stripManagedFields() toolscache.TransformFunc {
	return cache.TransformStripManagedFields()
}

// namespaceWatchdog restarts the operator when a CR shows up in a
// namespace whose core objects are not cached.
//
// The CR informers are cluster-wide, so the reconcile WOULD fire — but
// every Get for that CR's Deployment or ConfigMap would come back
// NotFound from a cache that never watched the namespace, and the
// operator would loop trying to create objects that already exist.
// Exiting is how a cache scope is widened: the namespace list is fixed
// once the manager starts, so the new set can only take effect on a
// fresh process. Exit 0, kubelet restarts us, boot discovery picks the
// namespace up. This is the same thing OLM does when targetNamespaces
// change.
type namespaceWatchdog struct {
	client client.Client
	cached map[string]struct{}
}

func (w *namespaceWatchdog) NeedLeaderElection() bool { return false }

func (w *namespaceWatchdog) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("cache-scope")
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			var missing []string
			for _, l := range []client.ObjectList{
				&agentofficev1alpha1.AgentWorkstationList{},
				&agentofficev1alpha1.AgentGatewayList{},
				&agentofficev1alpha1.KnowledgeBaseList{},
			} {
				if err := w.client.List(ctx, l); err != nil {
					continue
				}
				nss, err := extractNamespaces(l)
				if err != nil {
					continue
				}
				for _, ns := range nss {
					if _, ok := w.cached[ns]; !ok {
						missing = append(missing, ns)
					}
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				log.Info("agent-office CRs found in namespaces this process does not cache; "+
					"exiting so the cache is rebuilt with them included",
					"namespaces", missing)
				os.Exit(0)
			}
		}
	}
}
