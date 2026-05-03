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
	"crypto/rand"
	"encoding/base64"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
	"github.com/enterprisewebservice/agent-office-operator/internal/templates"
)

// DefaultOpenClawImage is the browser-enabled OpenClaw image used when
// the AW spec doesn't pin one.
const DefaultOpenClawImage = "quay-quay-quay-test.apps.salamander.aimlworkbench.com/deanpeterson/openclaw-browser:0.1.0"

// resource names — agent-office-server's existing convention so we
// can adopt the existing 5 agents without renaming anything.
func cmName(awName string) string      { return "agent-" + awName + "-config" }
func tokenName(awName string) string   { return "agent-" + awName + "-token" }
func pvcName(awName string) string     { return "agent-" + awName + "-workspace" }
func deployName(awName string) string  { return "agent-" + awName }
func serviceName(awName string) string { return "agent-" + awName }
func routeName(awName string) string   { return "agent-" + awName }

// agentLabels are stamped onto every owned resource so the existing
// label selectors (used by agent-office-server's watcher cache and the
// office UI) keep working.
func agentLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "agent-office-operator",
		"app.kubernetes.io/name":       name,
		"agentoffice.ai/agent":         name,
	}
}

// generateGatewayToken creates a random URL-safe token for OpenClaw
// gateway auth. Operator generates this once and persists it in the
// per-agent token Secret so re-reconcile is idempotent.
func generateGatewayToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// reconcileChildren applies all owned child resources from the AW spec.
// Each builder creates a fresh object; CreateOrUpdate adopts existing
// resources (sets ownerReference) and patches drift.
func (r *AgentWorkstationReconciler) reconcileChildren(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) error {
	// 1. Token Secret — operator-owned, holds the random gateway token.
	//    Generate once; on subsequent reconciles preserve the value.
	tok, err := r.reconcileTokenSecret(ctx, aw)
	if err != nil {
		return fmt.Errorf("token secret: %w", err)
	}

	// 2. ConfigMap — openclaw.json + workspace .md files. Rendered
	//    deterministically from spec so a spec edit produces a CM update
	//    (init container re-copies on next pod restart).
	if err := r.reconcileConfigMap(ctx, aw, tok); err != nil {
		return fmt.Errorf("configmap: %w", err)
	}

	// 3. PVC — persistent workspace.
	if err := r.reconcilePVC(ctx, aw); err != nil {
		return fmt.Errorf("pvc: %w", err)
	}

	// 4. Deployment.
	if err := r.reconcileDeployment(ctx, aw); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}

	// 5. Service.
	if err := r.reconcileService(ctx, aw); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	// 6. Route — only adopt if Spec doesn't say otherwise. Built as
	//    unstructured so the operator stays portable to vanilla k8s.
	if err := r.reconcileRoute(ctx, aw); err != nil {
		return fmt.Errorf("route: %w", err)
	}

	return nil
}

// reconcileTokenSecret creates the per-agent token Secret if missing
// and returns the token value. The User-supplied API-key Secret
// (apiKeySecretRef) stays separate — operator never writes to it.
func (r *AgentWorkstationReconciler) reconcileTokenSecret(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) (string, error) {
	key := client.ObjectKey{Namespace: aw.Namespace, Name: tokenName(aw.Name)}
	var existing corev1.Secret
	err := r.Get(ctx, key, &existing)
	if err == nil {
		if t, ok := existing.Data["OPENCLAW_GATEWAY_TOKEN"]; ok && len(t) > 0 {
			return string(t), nil
		}
		// Secret exists but token missing — patch it in.
		t, err := generateGatewayToken()
		if err != nil {
			return "", err
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data["OPENCLAW_GATEWAY_TOKEN"] = []byte(t)
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		for k, v := range agentLabels(aw.Name) {
			existing.Labels[k] = v
		}
		if err := controllerutil.SetControllerReference(aw, &existing, r.Scheme); err != nil {
			return "", err
		}
		return t, r.Update(ctx, &existing)
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}
	t, err := generateGatewayToken()
	if err != nil {
		return "", err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    agentLabels(aw.Name),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"OPENCLAW_GATEWAY_TOKEN": []byte(t)},
	}
	if err := controllerutil.SetControllerReference(aw, sec, r.Scheme); err != nil {
		return "", err
	}
	return t, r.Create(ctx, sec)
}

// reconcileConfigMap renders all template files and applies the CM.
func (r *AgentWorkstationReconciler) reconcileConfigMap(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation, gatewayToken string) error {
	openclawJSON, err := templates.RenderOpenClawConfig(aw, gatewayToken)
	if err != nil {
		return err
	}
	agentsMd, err := templates.RenderAgentsMd(aw)
	if err != nil {
		return err
	}
	identityMd, err := templates.RenderIdentityMd(aw)
	if err != nil {
		return err
	}
	soulMd, err := templates.RenderSoulMd(aw)
	if err != nil {
		return err
	}
	userMd, err := templates.RenderUserMd()
	if err != nil {
		return err
	}
	toolsMd, err := templates.RenderToolsMd(aw)
	if err != nil {
		return err
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName(aw.Name),
			Namespace: aw.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = mergeLabels(cm.Labels, agentLabels(aw.Name))
		cm.Data = map[string]string{
			"openclaw.json": openclawJSON,
			"AGENTS.md":     agentsMd,
			"IDENTITY.md":   identityMd,
			"SOUL.md":       soulMd,
			"USER.md":       userMd,
			"TOOLS.md":      toolsMd,
		}
		return controllerutil.SetControllerReference(aw, cm, r.Scheme)
	})
	return err
}

// reconcilePVC creates the per-agent workspace PVC if missing. PVC
// spec is immutable post-create so we only create-once.
func (r *AgentWorkstationReconciler) reconcilePVC(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) error {
	key := client.ObjectKey{Namespace: aw.Namespace, Name: pvcName(aw.Name)}
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, key, &existing)
	if err == nil {
		// Adopt by setting ownerRef + labels if missing.
		changed := false
		if controllerutil.SetControllerReference(aw, &existing, r.Scheme) == nil {
			changed = true
		}
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
			changed = true
		}
		for k, v := range agentLabels(aw.Name) {
			if existing.Labels[k] != v {
				existing.Labels[k] = v
				changed = true
			}
		}
		if changed {
			return r.Update(ctx, &existing)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    agentLabels(aw.Name),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("2Gi"),
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(aw, pvc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pvc)
}

// reconcileDeployment builds the OpenClaw Deployment from the spec.
func (r *AgentWorkstationReconciler) reconcileDeployment(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) error {
	image := aw.Spec.Image
	if image == "" {
		image = DefaultOpenClawImage
	}
	apiKeySecretName := aw.Spec.APIKeySecretRef
	if apiKeySecretName == "" {
		// Convention used by agent-office-server before slice 4.
		apiKeySecretName = "agent-" + aw.Name + "-credentials"
	}

	replicas := int32(1)
	fsGroup := int64(1000)
	dshmSize := resource.MustParse("1Gi")
	labels := agentLabels(aw.Name)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName(aw.Name),
			Namespace: aw.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = mergeLabels(dep.Labels, labels)
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				SecurityContext: &corev1.PodSecurityContext{FSGroup: &fsGroup},
				InitContainers: []corev1.Container{{
					Name:  "init-config",
					Image: "registry.access.redhat.com/ubi9/ubi-minimal:latest",
					Command: []string{"/bin/sh", "-c", `
						cp /config/openclaw.json /workspace/openclaw.json
						mkdir -p /workspace/workspace
						for f in AGENTS.md IDENTITY.md SOUL.md USER.md TOOLS.md; do
							if [ ! -f "/workspace/workspace/$f" ]; then
								cp "/config/$f" "/workspace/workspace/$f"
							fi
						done
					`},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "config", MountPath: "/config", ReadOnly: true},
						{Name: "workspace", MountPath: "/workspace"},
					},
				}},
				Containers: []corev1.Container{{
					Name:  "openclaw",
					Image: image,
					Ports: []corev1.ContainerPort{{
						Name: "gateway", ContainerPort: 18789, Protocol: corev1.ProtocolTCP,
					}},
					EnvFrom: []corev1.EnvFromSource{
						// Operator-owned token (always exists).
						{SecretRef: &corev1.SecretEnvSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: tokenName(aw.Name)},
						}},
						// User-supplied API key (optional — pod will
						// crash if missing, that's fine; status surfaces it).
						{SecretRef: &corev1.SecretEnvSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: apiKeySecretName},
							Optional:             boolPtr(true),
						}},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/home/node/.openclaw"},
						{Name: "dshm", MountPath: "/dev/shm"},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "config", VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: cmName(aw.Name)},
						}}},
					{Name: "workspace", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName(aw.Name),
						}}},
					{Name: "dshm", VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium: corev1.StorageMediumMemory, SizeLimit: &dshmSize,
						}}},
				},
			},
		}
		if aw.Spec.Resources != nil {
			dep.Spec.Template.Spec.Containers[0].Resources = *aw.Spec.Resources
		}
		return controllerutil.SetControllerReference(aw, dep, r.Scheme)
	})
	return err
}

// reconcileService creates / patches the agent's Service.
func (r *AgentWorkstationReconciler) reconcileService(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) error {
	labels := agentLabels(aw.Name)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName(aw.Name),
			Namespace: aw.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = mergeLabels(svc.Labels, labels)
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{
			Name: "gateway", Port: 18789, TargetPort: intstr.FromInt32(18789), Protocol: corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(aw, svc, r.Scheme)
	})
	return err
}

// reconcileRoute creates / patches the agent's Route via unstructured
// (route.openshift.io is OpenShift-only; building it inline keeps the
// operator buildable on vanilla k8s).
func (r *AgentWorkstationReconciler) reconcileRoute(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) error {
	gvk := schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
	rt := &unstructured.Unstructured{}
	rt.SetGroupVersionKind(gvk)
	rt.SetName(routeName(aw.Name))
	rt.SetNamespace(aw.Namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rt, func() error {
		labels := mergeLabels(rt.GetLabels(), agentLabels(aw.Name))
		rt.SetLabels(labels)
		// Build spec each reconcile (drift correction).
		spec := map[string]interface{}{
			"to": map[string]interface{}{
				"kind": "Service",
				"name": serviceName(aw.Name),
			},
			"port": map[string]interface{}{
				"targetPort": "gateway",
			},
			"tls": map[string]interface{}{
				"termination":                   "edge",
				"insecureEdgeTerminationPolicy": "Redirect",
			},
		}
		// Preserve auto-generated host if already set.
		if existingHost, ok, _ := unstructured.NestedString(rt.Object, "spec", "host"); ok && existingHost != "" {
			spec["host"] = existingHost
		}
		rt.Object["spec"] = spec
		return controllerutil.SetControllerReference(aw, rt, r.Scheme)
	})
	if apierrors.IsNotFound(err) {
		// Route CRD missing (vanilla k8s) — non-fatal, operator just
		// won't manage Route in that environment.
		return nil
	}
	return err
}

// mergeLabels overlays src onto dst, returning the merged map.
func mergeLabels(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func boolPtr(b bool) *bool { return &b }
