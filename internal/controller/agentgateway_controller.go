/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
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
	// RestConfig powers the auto-approve action's exec into the
	// gateway pod (we hand-edit ~/.openclaw/devices/paired.json
	// because OpenClaw's CLI approval flow is itself blocked by
	// pairing — chicken-and-egg).
	RestConfig *rest.Config
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

	// 2. Auto-approve the configured node-host's pending pairing
	//    request (if any). One-shot per pairing — once approved, the
	//    paired entry persists in the gateway PVC.
	autoApprove := gw.Spec.AutoApproveNodeHost == nil || *gw.Spec.AutoApproveNodeHost
	if autoApprove && gw.Spec.NodeHostRef != nil && gw.Spec.NodeHostRef.Name != "" {
		if err := r.maybeAutoApprovePending(ctx, &gw); err != nil {
			log.Info("auto-approve skipped", "err", err)
			// non-fatal — the gateway pod may not be Ready yet, will retry
		}
	}

	// 3. Ensure the gateway's openclaw.json has models.providers
	//    declared. The init container is seed-only, so existing PVCs
	//    that pre-date the providers field need an in-place merge.
	//    Idempotent: if the provider already exists, this is a no-op.
	if err := r.maybeMergeModelsProviders(ctx, &gw); err != nil {
		log.Info("models.providers merge skipped", "err", err)
	}

	// 4. Compute status from observed Deployment + Route + AW count.
	return r.reconcileGatewayStatus(ctx, &gw)
}

// maybeMergeModelsProviders ensures models.providers.openai exists in
// the gateway's openclaw.json. Existing PVCs created before the
// providers feature don't have it; without it, agents fail with
// "No API key found for provider openai" even when OPENAI_API_KEY is
// in the pod env. Idempotent — only writes when the field is absent
// or shape-incomplete.
func (r *AgentGatewayReconciler) maybeMergeModelsProviders(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) error {
	if r.RestConfig == nil {
		return fmt.Errorf("RestConfig not set; cannot exec")
	}
	pod, err := r.findReadyGatewayPod(ctx, gw)
	if err != nil {
		return err
	}
	script := `
const fs = require("fs");
const p = "/home/node/.openclaw/openclaw.json";
const cfg = JSON.parse(fs.readFileSync(p, "utf8"));
let changed = false;
cfg.models = cfg.models || {};
cfg.models.providers = cfg.models.providers || {};
if (!cfg.models.providers.openai || !cfg.models.providers.openai.apiKey) {
  cfg.models.providers.openai = {
    baseUrl: "https://api.openai.com/v1",
    api: "openai-completions",
    apiKey: "${OPENAI_API_KEY}",
    models: [
      { id: "gpt-5.4", name: "gpt-5.4" },
      { id: "gpt-4o-mini", name: "gpt-4o-mini" },
      { id: "gpt-4o", name: "gpt-4o" }
    ]
  };
  changed = true;
}
if (changed) {
  fs.writeFileSync(p, JSON.stringify(cfg, null, 2));
  console.log("MERGED_MODELS_PROVIDERS");
} else {
  console.log("NO_CHANGE");
}
`
	out, err := r.execInGatewayPod(ctx, pod, []string{"node", "-e", script})
	if err != nil {
		return fmt.Errorf("merge models.providers: %w (out=%s)", err, out)
	}
	logf.FromContext(ctx).V(1).Info("models.providers merge", "result", strings.TrimSpace(out))
	return nil
}

// maybeAutoApprovePending checks the gateway pod's pairing store and
// approves any pending request whose displayName matches the
// configured nodeHostRef. Idempotent: if the matching node is
// already paired, this is a no-op. After approval the gateway pod
// is bounced once so OpenClaw reloads the pairing store on next
// boot.
func (r *AgentGatewayReconciler) maybeAutoApprovePending(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) error {
	if r.RestConfig == nil {
		return fmt.Errorf("RestConfig not set; cannot exec")
	}
	pod, err := r.findReadyGatewayPod(ctx, gw)
	if err != nil {
		return err
	}

	// Inline node script — same logic as the CLI recipe but checks
	// for already-approved displayName before doing anything.
	script := fmt.Sprintf(`
const fs = require("fs"); const crypto = require("crypto");
const dir = "/home/node/.openclaw/devices";
const want = %q; // nodeHostRef.name (matches displayName "<name>-nodehost")
let pending; let paired;
try { pending = JSON.parse(fs.readFileSync(dir+"/pending.json","utf8")); } catch (e) { pending = {}; }
try { paired = JSON.parse(fs.readFileSync(dir+"/paired.json","utf8")); } catch (e) { paired = {}; }

// Already paired? No-op.
const alreadyPaired = Object.values(paired).some(d =>
  (d.displayName || "").includes(want) && d.role === "node"
);
if (alreadyPaired) { console.log("ALREADY_PAIRED"); process.exit(0); }

// Find a matching pending request.
const target = Object.entries(pending).find(([_, p]) =>
  (p.displayName || "").includes(want) && p.role === "node"
);
if (!target) { console.log("NO_PENDING"); process.exit(0); }
const [reqId, p] = target;
const now = Date.now();
const newToken = () => crypto.randomBytes(32).toString("base64url");
const tokens = {};
if (p.role) tokens[p.role] = { token: newToken(), role: p.role, scopes: p.scopes||[], createdAtMs: now };
paired[p.deviceId] = {
  deviceId: p.deviceId, publicKey: p.publicKey, displayName: p.displayName,
  platform: p.platform, deviceFamily: p.deviceFamily, clientId: p.clientId,
  clientMode: p.clientMode, role: p.role, roles: p.roles||[p.role],
  scopes: p.scopes||[], approvedScopes: p.scopes||[], remoteIp: p.remoteIp,
  tokens, createdAtMs: now, approvedAtMs: now,
};
delete pending[reqId];
fs.writeFileSync(dir+"/pending.json", JSON.stringify(pending));
fs.writeFileSync(dir+"/paired.json", JSON.stringify(paired));
console.log("APPROVED " + p.deviceId + " " + p.displayName);
`, gw.Spec.NodeHostRef.Name)

	out, err := r.execInGatewayPod(ctx, pod, []string{"node", "-e", script})
	if err != nil {
		return fmt.Errorf("exec auto-approve: %w (out=%s)", err, out)
	}
	logf.FromContext(ctx).Info("auto-approve result", "result", out)

	// If we approved something, bounce the pod once so the gateway
	// reloads paired.json. ALREADY_PAIRED / NO_PENDING → no bounce.
	if !strings.Contains(out, "APPROVED") {
		return nil
	}
	if err := r.Delete(ctx, pod); err != nil {
		return fmt.Errorf("bounce gateway pod after approval: %w", err)
	}
	return nil
}

func (r *AgentGatewayReconciler) findReadyGatewayPod(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{"agentoffice.ai/gateway": gw.Name},
	); err != nil {
		return nil, err
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, c := range p.Status.ContainerStatuses {
			if c.Name == "openclaw" && c.Ready {
				return &p, nil
			}
		}
	}
	return nil, fmt.Errorf("no Ready openclaw container in any %s gateway pod", gw.Name)
}

func (r *AgentGatewayReconciler) execInGatewayPod(ctx context.Context, pod *corev1.Pod, cmd []string) (string, error) {
	cs, err := kubernetes.NewForConfig(r.RestConfig)
	if err != nil {
		return "", err
	}
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(pod.Namespace).Name(pod.Name).
		SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: "openclaw", Command: cmd, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(r.RestConfig, "POST", req.URL())
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &out})
	if err != nil {
		return out.String(), err
	}
	return out.String(), nil
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
