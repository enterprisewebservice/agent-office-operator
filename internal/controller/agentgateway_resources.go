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

func ptrBool(b bool) *bool    { return &b }
func ptrInt32(i int32) *int32 { return &i }

// gatewayVolumeMounts returns the openclaw container's volume
// mounts. Always includes the workspace PVC + /dev/shm. When
// spec.codexCredentialsSecretRef is set, also mounts the
// secret's `auth.json` key as the file at
// /home/node/.codex/auth.json — the path OpenClaw natively reads
// on agent startup (pi-ai readCodexCliCredentials, see
// profiles-*.js in /app/dist). When `attachedKBs` is non-empty,
// each KnowledgeBase is mounted at
// /home/node/.openclaw/wiki/<kb-name>/ so all logical agents in
// the gateway pod see the same wiki content.
// gatewayInitMounts returns the volume mounts the init-config
// initContainer needs. Always includes the openclaw config CM +
// the workspace PVC. When Codex creds are wired, also mounts the
// read-only Secret projection and the writable .codex/ emptyDir
// so the init step can seed auth.json into the latter.
func gatewayInitMounts(gw *agentofficev1alpha1.AgentGateway) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/config", ReadOnly: true},
		{Name: "workspace", MountPath: "/workspace"},
	}
	if gw.Spec.CodexCredentialsSecretRef != "" {
		mounts = append(mounts,
			corev1.VolumeMount{Name: "codex-auth-src", MountPath: "/var/lib/codex-auth", ReadOnly: true},
			corev1.VolumeMount{Name: "codex-home", MountPath: "/codex-home"},
		)
	}
	return mounts
}

// codexAuthSyncContainer returns a tiny sidecar that watches
// /var/lib/codex-auth/auth.json (kept fresh by kubelet on Secret
// updates, which happen when ESO syncs new Vault data) and
// re-copies it into /home/node/.codex/auth.json so codex-acp and
// openclaw see the latest token. Without this, ESO can update
// the Secret all it wants but the gateway pod's emptyDir copy
// stays stale until the pod restarts.
func codexAuthSyncContainer() corev1.Container {
	return corev1.Container{
		Name:  "codex-auth-sync",
		Image: "registry.access.redhat.com/ubi9/ubi-minimal:latest",
		Command: []string{"/bin/sh", "-c", `
			set -u
			prev=""
			while true; do
				if [ -f /var/lib/codex-auth/auth.json ]; then
					cur=$(sha256sum /var/lib/codex-auth/auth.json | cut -d' ' -f1)
					if [ "$cur" != "$prev" ]; then
						cp /var/lib/codex-auth/auth.json /home/node/.codex/auth.json
						chmod 0600 /home/node/.codex/auth.json
						echo "codex-auth-sync: refreshed auth.json (sha256=$cur)"
						prev="$cur"
					fi
				fi
				sleep 30
			done
		`},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "codex-auth-src", MountPath: "/var/lib/codex-auth", ReadOnly: true},
			{Name: "codex-home", MountPath: "/home/node/.codex"},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	}
}

func gatewayVolumeMounts(gw *agentofficev1alpha1.AgentGateway, attachedKBs []agentofficev1alpha1.KnowledgeBase) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/home/node/.openclaw"},
		{Name: "dshm", MountPath: "/dev/shm"},
	}
	if gw.Spec.CodexCredentialsSecretRef != "" {
		// Two-volume layout (v0.0.65+): the K8s Secret is mounted
		// READ-ONLY at /var/lib/codex-auth/ (kubelet refreshes it
		// in place when ESO updates the Vault-sourced data), and
		// /home/node/.codex/ is a WRITABLE emptyDir owned by the
		// pod's fsGroup. The init-config initContainer + the
		// codex-auth-sync sidecar keep auth.json in sync between
		// the two. We split them because the codex-acp binary
		// (@zed-industries/codex-acp, invoked by openclaw) needs
		// to write into .codex/ — adjust PATH, persist a
		// config.toml — and SubPath-mounted Secret directories are
		// root-owned + read-only, which made codex-acp exit before
		// initialize with "Permission denied (os error 13)".
		mounts = append(mounts,
			corev1.VolumeMount{
				Name:      "codex-auth-src",
				MountPath: "/var/lib/codex-auth",
				ReadOnly:  true,
			},
			corev1.VolumeMount{
				Name:      "codex-home",
				MountPath: "/home/node/.codex",
			},
		)
	}
	for _, kb := range attachedKBs {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      kbVolumeName(kb.Name),
			MountPath: kbMountPath(kb.Name),
		})
	}
	return mounts
}

// gatewayVolumes returns the pod's volumes. Always: openclaw
// config CM, workspace PVC, in-memory /dev/shm. When codex creds
// are configured, also project the secret's auth.json key. When
// `attachedKBs` is non-empty, each KB's PVC is added as a volume
// (paired with the matching mount in gatewayVolumeMounts).
func gatewayVolumes(gw *agentofficev1alpha1.AgentGateway, dshmSize resource.Quantity, attachedKBs []agentofficev1alpha1.KnowledgeBase) []corev1.Volume {
	vols := []corev1.Volume{
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
	}
	if gw.Spec.CodexCredentialsSecretRef != "" {
		// codex-auth-src: read-only projection of the K8s Secret.
		// kubelet keeps this fresh as ESO updates the Secret —
		// the underlying file changes in place, our sidecar
		// detects the change and re-copies into codex-home.
		vols = append(vols, corev1.Volume{
			Name: "codex-auth-src",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: gw.Spec.CodexCredentialsSecretRef,
					Items: []corev1.KeyToPath{
						{Key: "auth.json", Path: "auth.json"},
					},
					DefaultMode: ptrInt32(0o444),
					Optional:    ptrBool(true),
				},
			},
		})
		// codex-home: writable emptyDir for /home/node/.codex/.
		// OpenShift's restricted SCC injects fsGroup automatically
		// and emptyDirs honor fsGroup, so the pod user can read,
		// write, and create files (config.toml, etc.) here.
		vols = append(vols, corev1.Volume{
			Name: "codex-home",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}
	for _, kb := range attachedKBs {
		vols = append(vols, corev1.Volume{
			Name: kbVolumeName(kb.Name),
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: kbPVCName(kb.Name),
				},
			},
		})
		// Per-KB git-sync ConfigMap (script + bootstrap
		// templates) — mounted into the git-sync sidecar at
		// /sync. Only added when the KB has GitMirror so the
		// pod stays minimal when mirroring isn't configured.
		if kb.Spec.GitMirror != nil {
			vols = append(vols, gitSyncVolume(kb))
		}
	}
	return vols
}

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

	// Discover KnowledgeBases attached to this gateway. Sorted
	// by name so the resulting volume/mount slices are
	// deterministic — keeps the Deployment's pod template
	// equality check stable and avoids spurious rollouts when
	// reconciles see the same set in a different list order.
	var kbList agentofficev1alpha1.KnowledgeBaseList
	if err := r.List(ctx, &kbList, client.InNamespace(gw.Namespace)); err != nil {
		return fmt.Errorf("listing KnowledgeBases: %w", err)
	}
	attachedKBs := make([]agentofficev1alpha1.KnowledgeBase, 0, len(kbList.Items))
	for _, kb := range kbList.Items {
		if kb.Spec.GatewayRef.Name == gw.Name {
			attachedKBs = append(attachedKBs, kb)
		}
	}
	sort.Slice(attachedKBs, func(i, j int) bool {
		return attachedKBs[i].Name < attachedKBs[j].Name
	})

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
					// overwrote. Also: if Codex creds are wired,
					// copy auth.json from the read-only Secret mount
					// into the writable .codex/ emptyDir so codex-acp
					// can boot. Subsequent ESO refreshes are
					// propagated by the codex-auth-sync sidecar.
					Command: []string{"/bin/sh", "-c", `
						set -eu
						if [ ! -f /workspace/openclaw.json ]; then
							cp /config/openclaw.json /workspace/openclaw.json
						fi
						mkdir -p /workspace/agents

						if [ -f /var/lib/codex-auth/auth.json ]; then
							cp /var/lib/codex-auth/auth.json /codex-home/auth.json
							chmod 0600 /codex-home/auth.json
							echo "codex auth.json seeded into .codex/ (init)"
						fi
					`},
					VolumeMounts: gatewayInitMounts(gw),
				}},
				Containers: gatewayContainers(image, gw, tokenSecretName, attachedKBs),
				Volumes:    gatewayVolumes(gw, dshmSize, attachedKBs),
			},
		}
		if gw.Spec.Resources != nil {
			dep.Spec.Template.Spec.Containers[0].Resources = *gw.Spec.Resources
		}
		return controllerutil.SetControllerReference(gw, dep, r.Scheme)
	})
	return err
}

// gatewayContainers returns the pod's container list: always
// the openclaw runtime, plus one git-sync sidecar per attached
// KnowledgeBase whose spec.gitMirror is configured. Sidecars
// share the wiki PVC volume already in the pod (no rsync between
// containers — both read/write the same files).
func gatewayContainers(image string, gw *agentofficev1alpha1.AgentGateway, tokenSecretName string, attachedKBs []agentofficev1alpha1.KnowledgeBase) []corev1.Container {
	out := []corev1.Container{{
		Name:            "openclaw",
		Image:           image,
		ImagePullPolicy: corev1.PullAlways,
		Ports: []corev1.ContainerPort{{
			Name: "gateway", ContainerPort: 18789, Protocol: corev1.ProtocolTCP,
		}},
		EnvFrom:      gatewayEnvFrom(gw, tokenSecretName),
		VolumeMounts: gatewayVolumeMounts(gw, attachedKBs),
	}}
	// codex-auth-sync sidecar: propagates Vault → Secret → emptyDir
	// updates so the user's re-auth dialog flow rotates the token
	// without a pod restart. Only added when Codex creds are wired.
	if gw.Spec.CodexCredentialsSecretRef != "" {
		out = append(out, codexAuthSyncContainer())
	}
	for _, kb := range attachedKBs {
		if kb.Spec.GitMirror != nil {
			out = append(out, gitSyncContainer(kb))
		}
	}
	return out
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
