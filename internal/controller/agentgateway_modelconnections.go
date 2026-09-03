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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=agentoffice.ai,resources=modelconnections,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=modelconnections/status,verbs=get;update;patch

// ModelConnection rendering — the endpoint half of "the admin
// publishes brains; workstations reference them by name".
//
// A workstation with spec.model.connectionRef pointing at an
// endpoint-kind ModelConnection needs its gateway to carry a matching
// models.providers.<connection-name> block, or the emitted
// `<connection>/<model>` string is a silent dead lane (openclaw
// resolves the model string against a provider block that doesn't
// exist and fails every turn with no Kubernetes-visible symptom).
// The shape of that block — baseUrl + api dialect + apiKey — was
// proven live against a LiteLLM front door before this code existed:
// the whole chain (agent → gateway → provider → LiteLLM → upstream)
// answered, so rendering is the only missing piece.
//
// Credentials: the connection's Secret lives in the platform admin
// namespace. Consumers never mount it. The operator copies exactly
// the referenced keys into a gateway-owned derived Secret
// (<gateway>-model-connections) in the gateway's namespace, the
// deployment gets envFrom on it, and openclaw.json carries only the
// `${MODELCONN_<NAME>_API_KEY}` env reference — the same expansion
// contract the canonical apiKey route already uses.

// connEnvVarName maps a connection name to the env var carrying its
// key: `model-desk` → `MODELCONN_MODEL_DESK_API_KEY`.
func connEnvVarName(connName string) string {
	up := strings.ToUpper(connName)
	var b strings.Builder
	for _, r := range up {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return "MODELCONN_" + b.String() + "_API_KEY"
}

// modelConnectionsSecretName is the gateway-owned derived Secret that
// aggregates the API keys of every connection this gateway renders.
func modelConnectionsSecretName(gwName string) string {
	return gwName + "-model-connections"
}

// collectModelConnections resolves the endpoint-kind ModelConnections
// referenced by AgentWorkstations targeting this gateway.
//
// Returns the openclaw provider blocks keyed by connection name, and
// the derived-Secret data (env var name → key bytes). Unresolvable
// refs and unreadable Secrets are logged and skipped — the
// workstation side reports them; rendering what does resolve keeps
// one bad ref from blocking every other agent on the gateway.
func (r *AgentGatewayReconciler) collectModelConnections(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) (map[string]map[string]interface{}, map[string][]byte, error) {
	log := logf.FromContext(ctx)

	var awList agentofficev1alpha1.AgentWorkstationList
	if err := r.List(ctx, &awList, client.InNamespace(gw.Namespace)); err != nil {
		return nil, nil, fmt.Errorf("listing AgentWorkstations: %w", err)
	}

	refs := map[string]bool{}
	for _, aw := range awList.Items {
		if effectiveGatewayRef(&aw) != gw.Name {
			continue
		}
		if ref := aw.Spec.Model.ConnectionRef; ref != "" {
			refs[ref] = true
		}
	}

	providers := map[string]map[string]interface{}{}
	secretData := map[string][]byte{}

	// Tenant gate. A workspace that acts for a principal (namespace
	// annotations agentoffice.ai/user|groups) renders only the
	// connections offered to that principal — the same rule that
	// projects its ModelConnectionOffers and filters the Developer Hub
	// menu. Refused refs get no provider block and no projected key; the
	// workstation carries the reason (ModelConnectionOffered=False).
	// Platform namespaces (no annotations) render every ref, as before.
	var gwNS corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: gw.Namespace}, &gwNS); err != nil {
		return nil, nil, fmt.Errorf("reading namespace %s for the model connection gate: %w", gw.Namespace, err)
	}
	tenant, gated := tenantOf(&gwNS)

	// Carry-over. The gateway's persisted openclaw.json keeps every
	// provider block ever written into it (the init container seeds the
	// file once; the provider write needs a Ready pod to exec into), and
	// openclaw refuses to boot while a provider's ${MODELCONN_*_API_KEY}
	// is missing. Dropping a key the persisted config still references
	// therefore wedges the gateway: the pod rolls on the Secret change,
	// fails to boot, and the write that would have removed the block never
	// finds a Ready pod (seen live 2026-09-03: the Module 7 brain swap left
	// a seat in CrashLoopBackOff). So a connection whose key is already
	// projected stays rendered — block and key — for as long as it exists
	// and is offered here. A swap is additive; the old lane is simply
	// unused. Only a deleted or no-longer-offered connection is dropped.
	var projected corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: modelConnectionsSecretName(gw.Name)}, &projected); err == nil && len(projected.Data) > 0 {
		var all agentofficev1alpha1.ModelConnectionList
		if err := r.List(ctx, &all); err == nil {
			for _, c := range all.Items {
				if _, has := projected.Data[connEnvVarName(c.Name)]; has && !refs[c.Name] {
					refs[c.Name] = true
					log.V(1).Info("carrying projected ModelConnection", "connection", c.Name)
				}
			}
		}
	}

	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		var conn agentofficev1alpha1.ModelConnection
		if err := r.Get(ctx, client.ObjectKey{Name: name}, &conn); err != nil {
			log.Info("referenced ModelConnection not found; skipping", "connection", name, "err", err)
			continue
		}
		if gated {
			if offered, _ := connectionOfferedTo(&conn, tenant); !offered {
				log.Info("ModelConnection not offered to this workspace; not rendered",
					"connection", name, "namespace", gw.Namespace, "user", tenant.User, "groups", tenant.Groups)
				continue
			}
		}
		if conn.Spec.Kind != agentofficev1alpha1.ModelConnectionKindEndpoint {
			// subscription/apiKey connections ride the gateway's
			// existing modelAuth plumbing; nothing to render.
			continue
		}
		if conn.Spec.BaseURL == "" {
			log.Info("endpoint ModelConnection has no baseUrl; skipping", "connection", name)
			continue
		}

		block := map[string]interface{}{
			"baseUrl": conn.Spec.BaseURL,
			"api":     defaultIfEmpty(conn.Spec.API, "openai-completions"),
		}
		models := make([]map[string]interface{}, 0, len(conn.Spec.Models))
		for _, m := range conn.Spec.Models {
			models = append(models, map[string]interface{}{
				"id": m.ID, "name": defaultIfEmpty(m.Name, m.ID),
			})
		}
		block["models"] = models

		if ref := conn.Spec.APIKeySecretRef; ref != nil && ref.Name != "" {
			ns := defaultIfEmpty(ref.Namespace, "agent-office")
			key := defaultIfEmpty(ref.Key, "api-key")
			var sec corev1.Secret
			// Uncached read: connection Secrets live in the admin
			// namespace, which may be outside the pinned core-type
			// cache namespaces (see cmd/main.go coreByObject) — a
			// cached Get would miss with "unknown namespace".
			reader := r.Reader
			if reader == nil {
				reader = r.Client
			}
			if err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &sec); err != nil {
				log.Info("ModelConnection secret unreadable; rendering provider without key", "connection", name, "secret", ns+"/"+ref.Name, "err", err)
			} else if v, ok := sec.Data[key]; !ok {
				log.Info("ModelConnection secret missing key; rendering provider without key", "connection", name, "secret", ns+"/"+ref.Name, "key", key)
			} else {
				envVar := connEnvVarName(conn.Name)
				secretData[envVar] = v
				block["apiKey"] = "${" + envVar + "}"
			}
		}

		providers[conn.Name] = block
	}

	return providers, secretData, nil
}

// ensureModelConnectionsSecret converges the gateway-owned derived
// Secret to exactly `data`; with no data it deletes the Secret.
// Returns the Secret name when it exists (for envFrom wiring).
func (r *AgentGatewayReconciler) ensureModelConnectionsSecret(ctx context.Context, gw *agentofficev1alpha1.AgentGateway, data map[string][]byte) (string, error) {
	name := modelConnectionsSecretName(gw.Name)

	if len(data) == 0 {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gw.Namespace}}
		if err := r.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("deleting stale model-connections secret: %w", err)
		}
		return "", nil
	}

	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gw.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Data = data
		return controllerutil.SetControllerReference(gw, sec, r.Scheme)
	})
	if err != nil {
		return "", fmt.Errorf("ensuring model-connections secret: %w", err)
	}
	return name, nil
}
