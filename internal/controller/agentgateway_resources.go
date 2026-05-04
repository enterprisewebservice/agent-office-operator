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

// gw* helpers — name conventions for resources owned by an
// AgentGateway. Distinct from the agent-* convention used by
// dedicated AgentWorkstations.
func gwCMName(gwName string)      string { return gwName + "-config" }
func gwTokenName(gwName string)   string { return gwName + "-token" }
func gwPVCName(gwName string)     string { return gwName + "-workspace" }
func gwDeployName(gwName string)  string { return gwName }
func gwServiceName(gwName string) string { return gwName }
func gwRouteName(gwName string)   string { return gwName }

// gatewayEnvFrom builds the gateway container's envFrom list. Always
// includes the gateway's own token Secret; optionally adds an
// operator-supplied secret carrying model-provider API keys
// (OPENAI_API_KEY / ANTHROPIC_API_KEY / etc.) when
// spec.envFromSecretRef is set.
func gatewayEnvFrom(gw *agentofficev1alpha1.AgentGateway, tokenSecretName string) []corev1.EnvFromSource {
	out := []corev1.EnvFromSource{
		{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecretName},
		}},
	}
	if gw.Spec.EnvFromSecretRef != "" {
		out = append(out, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: gw.Spec.EnvFromSecretRef},
				Optional:             ptrBool(true),
			},
		})
	}
	return out
}

func ptrBool(b bool) *bool { return &b }

// gatewayLabels stamps a uniform set on every owned resource.
func gatewayLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "agent-office-operator",
		"app.kubernetes.io/name":       name,
		"app.kubernetes.io/component":  "agent-gateway",
		"agentoffice.ai/gateway":       name,
	}
}

// reconcileGatewayChildren applies all owned child resources for an
// AgentGateway (Pod, PVC, Service, Route, ConfigMap, Secret).
// Mirror of the AgentWorkstation dedicated path but for the gateway
// runtime. agents.list inside openclaw.json starts empty — the
// AgentWorkstation runtime.shared path appends to it.
func (r *AgentGatewayReconciler) reconcileGatewayChildren(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) error {
	tok, err := r.reconcileGatewayTokenSecret(ctx, gw)
	if err != nil {
		return fmt.Errorf("token secret: %w", err)
	}
	if err := r.reconcileGatewayConfigMap(ctx, gw, tok); err != nil {
		return fmt.Errorf("configmap: %w", err)
	}
	if err := r.reconcileGatewayPVC(ctx, gw); err != nil {
		return fmt.Errorf("pvc: %w", err)
	}
	if err := r.reconcileGatewayDeployment(ctx, gw); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	if err := r.reconcileGatewayService(ctx, gw); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if err := r.reconcileGatewayRoute(ctx, gw); err != nil {
		return fmt.Errorf("route: %w", err)
	}
	return nil
}

// reconcileGatewayTokenSecret creates the gateway's
// OPENCLAW_GATEWAY_TOKEN secret on first reconcile, idempotent. The
// node-host VM consumes the same value (operator-managed sync, not
// hand-copied).
func (r *AgentGatewayReconciler) reconcileGatewayTokenSecret(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) (string, error) {
	name := gwTokenName(gw.Name)
	if gw.Spec.SharedTokenSecretRef != "" {
		name = gw.Spec.SharedTokenSecretRef
	}
	key := client.ObjectKey{Namespace: gw.Namespace, Name: name}
	var existing corev1.Secret
	err := r.Get(ctx, key, &existing)
	if err == nil {
		if t, ok := existing.Data["OPENCLAW_GATEWAY_TOKEN"]; ok && len(t) > 0 {
			return string(t), nil
		}
		t, err := generateGatewayToken()
		if err != nil {
			return "", err
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data["OPENCLAW_GATEWAY_TOKEN"] = []byte(t)
		existing.Labels = mergeLabels(existing.Labels, gatewayLabels(gw.Name))
		if err := controllerutil.SetControllerReference(gw, &existing, r.Scheme); err != nil {
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
			Labels:    gatewayLabels(gw.Name),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"OPENCLAW_GATEWAY_TOKEN": []byte(t)},
	}
	if err := controllerutil.SetControllerReference(gw, sec, r.Scheme); err != nil {
		return "", err
	}
	return t, r.Create(ctx, sec)
}

// reconcileGatewayConfigMap renders the base openclaw.json (no
// agents yet) and applies the CM. agents.list will be appended to
// at runtime by the AgentWorkstation runtime.shared reconciler.
func (r *AgentGatewayReconciler) reconcileGatewayConfigMap(ctx context.Context, gw *agentofficev1alpha1.AgentGateway, gatewayToken string) error {
	openclawJSON, err := templates.RenderAgentGatewayConfig(gw, gatewayToken, appsDomain())
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gwCMName(gw.Name),
			Namespace: gw.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = mergeLabels(cm.Labels, gatewayLabels(gw.Name))
		cm.Data = map[string]string{"openclaw.json": openclawJSON}
		return controllerutil.SetControllerReference(gw, cm, r.Scheme)
	})
	return err
}

// reconcileGatewayPVC sizes the workspace bigger than the
// AgentWorkstation default — many openclaw agents share this PVC.
func (r *AgentGatewayReconciler) reconcileGatewayPVC(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) error {
	key := client.ObjectKey{Namespace: gw.Namespace, Name: gwPVCName(gw.Name)}
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, key, &existing)
	if err == nil {
		changed := false
		if controllerutil.SetControllerReference(gw, &existing, r.Scheme) == nil {
			changed = true
		}
		merged := mergeLabels(existing.Labels, gatewayLabels(gw.Name))
		if existing.Labels == nil || len(merged) != len(existing.Labels) {
			existing.Labels = merged
			changed = true
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
			Labels:    gatewayLabels(gw.Name),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("10Gi"),
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(gw, pvc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pvc)
}

// reconcileGatewayDeployment runs the OpenClaw image with the
// generated openclaw.json + token Secret. Init container seeds
// openclaw.json into the PVC on first start; subsequent starts
// preserve any per-agent files OpenClaw has written there.
func (r *AgentGatewayReconciler) reconcileGatewayDeployment(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) error {
	image := gw.Spec.Image
	if image == "" {
		image = DefaultOpenClawImage
	}
	tokenSecretName := gwTokenName(gw.Name)
	if gw.Spec.SharedTokenSecretRef != "" {
		tokenSecretName = gw.Spec.SharedTokenSecretRef
	}

	replicas := int32(1)
	dshmSize := resource.MustParse("1Gi")
	labels := gatewayLabels(gw.Name)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gwDeployName(gw.Name),
			Namespace: gw.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = mergeLabels(dep.Labels, labels)
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				ImagePullSecrets: []corev1.LocalObjectReference{
					{Name: "quay-pull-secret"},
				},
				InitContainers: []corev1.Container{{
					Name:  "init-config",
					Image: "registry.access.redhat.com/ubi9/ubi-minimal:latest",
					// Seed-only: preserve the gateway's openclaw.json
					// across restarts. Per-agent additions live in
					// the file (managed by AW reconcile via exec) and
					// would be lost if init unconditionally
					// overwrote.
					Command: []string{"/bin/sh", "-c", `
						if [ ! -f /workspace/openclaw.json ]; then
							cp /config/openclaw.json /workspace/openclaw.json
						fi
						mkdir -p /workspace/agents
					`},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "config", MountPath: "/config", ReadOnly: true},
						{Name: "workspace", MountPath: "/workspace"},
					},
				}},
				Containers: []corev1.Container{{
					Name:            "openclaw",
					Image:           image,
					ImagePullPolicy: corev1.PullAlways,
					Ports: []corev1.ContainerPort{{
						Name: "gateway", ContainerPort: 18789, Protocol: corev1.ProtocolTCP,
					}},
					EnvFrom: gatewayEnvFrom(gw, tokenSecretName),
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/home/node/.openclaw"},
						{Name: "dshm", MountPath: "/dev/shm"},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "config", VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: gwCMName(gw.Name)},
						}}},
					{Name: "workspace", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: gwPVCName(gw.Name),
						}}},
					{Name: "dshm", VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium: corev1.StorageMediumMemory, SizeLimit: &dshmSize,
						}}},
				},
			},
		}
		if gw.Spec.Resources != nil {
			dep.Spec.Template.Spec.Containers[0].Resources = *gw.Spec.Resources
		}
		return controllerutil.SetControllerReference(gw, dep, r.Scheme)
	})
	return err
}

func (r *AgentGatewayReconciler) reconcileGatewayService(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) error {
	labels := gatewayLabels(gw.Name)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gwServiceName(gw.Name),
			Namespace: gw.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = mergeLabels(svc.Labels, labels)
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{
			Name: "gateway", Port: 18789, TargetPort: intstr.FromInt32(18789), Protocol: corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(gw, svc, r.Scheme)
	})
	return err
}

func (r *AgentGatewayReconciler) reconcileGatewayRoute(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) error {
	gvk := schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
	rt := &unstructured.Unstructured{}
	rt.SetGroupVersionKind(gvk)
	rt.SetName(gwRouteName(gw.Name))
	rt.SetNamespace(gw.Namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rt, func() error {
		rt.SetLabels(mergeLabels(rt.GetLabels(), gatewayLabels(gw.Name)))
		spec := map[string]interface{}{
			"to":   map[string]interface{}{"kind": "Service", "name": gwServiceName(gw.Name)},
			"port": map[string]interface{}{"targetPort": "gateway"},
			"tls": map[string]interface{}{
				"termination":                   "edge",
				"insecureEdgeTerminationPolicy": "Redirect",
			},
		}
		if existingHost, ok, _ := unstructured.NestedString(rt.Object, "spec", "host"); ok && existingHost != "" {
			spec["host"] = existingHost
		}
		rt.Object["spec"] = spec
		return controllerutil.SetControllerReference(gw, rt, r.Scheme)
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
