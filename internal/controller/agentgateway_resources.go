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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
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
func gwCMName(gwName string) string      { return gwName + "-config" }
func gwTokenName(gwName string) string   { return gwName + "-token" }
func gwPVCName(gwName string) string     { return gwName + "-workspace" }
func gwDeployName(gwName string) string  { return gwName }
func gwServiceName(gwName string) string { return gwName }
func gwRouteName(gwName string) string   { return gwName }

// gatewayRuntimeEnv returns the HOME/cache env the OpenClaw image
// needs to start under OpenShift's restricted SCC.
//
// OpenShift runs the container as an arbitrary UID with no /etc/passwd
// entry, so HOME resolves to "/". OpenClaw 2026.7.x then tries to
// create its state dir at /.openclaw and dies with
// `EACCES: permission denied, mkdir '/.openclaw'` before the gateway
// ever listens. The XDG + npm cache vars have the same root cause:
// they default under $HOME, and only /home/node/.openclaw (the PVC) is
// writable — not /home/node itself — so anything writing beside the
// state dir fails too.
//
// These are properties of the image + platform, not user
// configuration, so the operator owns them. They were previously
// hand-added to the user's spec.envFromSecretRef Secret to unblock the
// 7.x upgrade, which meant a freshly created gateway (or one with no
// envFromSecretRef) silently failed to boot.
//
// Set as explicit container env, which takes precedence over envFrom,
// so a stale copy left behind in a user Secret can't override them.
func gatewayRuntimeEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "HOME", Value: "/home/node"},
		{Name: "XDG_CACHE_HOME", Value: "/tmp/.cache"},
		{Name: "XDG_STATE_HOME", Value: "/tmp/.state"},
		{Name: "npm_config_cache", Value: "/tmp/.npm"},
	}
}

// gatewayEnvFrom builds the gateway container's envFrom list. Always
// includes the gateway's own token Secret; optionally adds an
// operator-supplied secret carrying model-provider API keys
// (OPENAI_API_KEY / ANTHROPIC_API_KEY / etc.) when
// spec.envFromSecretRef is set; optionally adds extra Secrets the
// caller passes in (in v1.4.1+, these are the per-AW
// `spec.tools.mcpServers[].envFromSecret` values discovered by the
// AgentGateway reconciler — see reconcileGatewayDeployment's call
// to collectMCPEnvFromSecrets()).
//
// All extra Secrets are marked Optional so missing references don't
// block the pod from starting — openclaw will surface a Config
// warning instead, which is the right UX during demo/dev iteration.
func gatewayEnvFrom(gw *agentofficev1alpha1.AgentGateway, tokenSecretName string, extraSecrets []string) []corev1.EnvFromSource {
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
	for _, s := range extraSecrets {
		out = append(out, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: s},
				Optional:             ptrBool(true),
			},
		})
	}
	return out
}

// gatewayEnv is gatewayRuntimeEnv plus, when hooks are on, the hook
// token sourced straight from its Secret key as OPENCLAW_HOOKS_TOKEN.
// openclaw.json carries only "${OPENCLAW_HOOKS_TOKEN}", so this env
// var is the one place the literal exists outside the Secret. It is
// deliberately NOT optional: a missing Secret should fail the pod
// loudly (CreateContainerConfigError) rather than start a gateway
// that then refuses its config over an unresolvable reference —
// though resolveHooks normally drops the env var before it comes to
// that.
func gatewayEnv(hooksSecret *corev1.SecretKeySelector) []corev1.EnvVar {
	env := gatewayRuntimeEnv()
	if hooksSecret != nil && hooksSecret.Name != "" {
		env = append(env, corev1.EnvVar{
			Name: templates.HooksTokenEnvVar,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: hooksSecret.Name},
					Key:                  hooksSecret.Key,
				},
			},
		})
	}
	return env
}

// reloaderSecrets lists every Secret whose rotation must roll the
// gateway — the MCP credential Secrets plus the hooks token Secret —
// deduplicated and sorted so the result is stable across reconciles.
// "" when there is nothing to watch. Since v1.7.45 this feeds the
// operator's OWN env-secrets content hash (below) instead of a
// Reloader annotation: this Deployment is re-rendered wholesale every
// reconcile, so any template patch a third party makes (Reloader's
// env injection, a manual `rollout restart`) is stomped before the
// rollout starts — observed live 2026-08-25 when a rotated seat token
// left a gateway running on a REVOKED credential. The operator is the
// only actor whose template changes survive itself.
func reloaderSecrets(mcpExtraSecrets []string, hooksSecret *corev1.SecretKeySelector) string {
	set := map[string]struct{}{}
	for _, s := range mcpExtraSecrets {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	if hooksSecret != nil && hooksSecret.Name != "" {
		set[hooksSecret.Name] = struct{}{}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// envSecretsHash folds the CONTENT of every env-sourced Secret into one
// stable digest for the pod template. When a credential rotates, the
// digest changes, the template changes, and the Deployment rolls — the
// same mechanism codexAuthSyncScriptHash uses for script changes. A
// referenced Secret that does not exist yet contributes its name plus a
// marker, so its later creation also rolls the pod.
func (r *AgentGatewayReconciler) envSecretsHash(ctx context.Context, ns string, mcpExtraSecrets []string, hooksSecret *corev1.SecretKeySelector) string {
	names := map[string]struct{}{}
	for _, n := range mcpExtraSecrets {
		if n != "" {
			names[n] = struct{}{}
		}
	}
	if hooksSecret != nil && hooksSecret.Name != "" {
		names[hooksSecret.Name] = struct{}{}
	}
	if len(names) == 0 {
		return ""
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	h := sha256.New()
	for _, n := range sorted {
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &sec); err != nil {
			fmt.Fprintf(h, "%s\x00absent\x00", n)
			continue
		}
		keys := make([]string, 0, len(sec.Data))
		for k := range sec.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(h, "%s\x00%s\x00", n, k)
			h.Write(sec.Data[k])
			h.Write([]byte{0})
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
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

// codex-auth-sync sidecar wiring. The script itself is
// templates.CodexAuthSyncScript, rendered into the gateway config CM
// under codexAuthSyncScriptKey and mounted read-only at
// codexAuthSyncScriptPath.
const (
	codexAuthSyncScriptKey  = "codex-auth-sync.mjs"
	codexAuthSyncScriptDir  = "/var/lib/codex-auth-sync"
	codexAuthSyncScriptPath = codexAuthSyncScriptDir + "/" + codexAuthSyncScriptKey
)

// codexAuthSyncScriptHash pins the script content into the pod template
// (as an annotation) so a script change rolls the gateway. The sidecar
// reads the script once at process start; without this, editing the
// embedded script would update the CM in place and then silently change
// nothing until some unrelated event restarted the pod.
func codexAuthSyncScriptHash() string {
	sum := sha256.Sum256([]byte(templates.CodexAuthSyncScript))
	return hex.EncodeToString(sum[:])[:12]
}

// codexAuthSyncContainer returns the sidecar that keeps the pod's Codex
// credential usable end to end. Historically a ubi-minimal shell loop
// that only mirrored the Secret projection
// (/var/lib/codex-auth/auth.json, kept fresh by kubelet as ESO / the
// Dev Hub re-auth flow updates the Secret) into the writable
// /home/node/.codex/auth.json. That left a gap: each logical agent
// holds its OWN copy of the OAuth profile in
// ~/.openclaw/agents/<id>/agent/openclaw-agent.sqlite, seeded only at
// provisioning. When the access token expired and a refresh hiccuped
// (2026-08-23), OpenClaw pruned those profiles and nothing re-seeded
// them — a 16h outage while the pod held a fresh credential in
// auth.json the whole time.
//
// The sidecar now runs templates.CodexAuthSyncScript under node
// instead: the same copy-on-change mirror, plus a per-agent seed pass
// that upserts the shared profile into any store lacking a usable
// openai OAuth profile (or holding a strictly older one). It runs on
// the gateway's own OpenClaw image — already pulled for the main
// container, and the only image guaranteed to carry the node:sqlite
// runtime matching the DBs it touches — and therefore needs the
// workspace PVC mount the shell loop never had.
func codexAuthSyncContainer(image string) corev1.Container {
	return corev1.Container{
		Name:            "codex-auth-sync",
		Image:           image,
		ImagePullPolicy: corev1.PullAlways,
		Command:         []string{"node", codexAuthSyncScriptPath},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "codex-auth-src", MountPath: "/var/lib/codex-auth", ReadOnly: true},
			{Name: "codex-home", MountPath: "/home/node/.codex"},
			{Name: "workspace", MountPath: "/home/node/.openclaw"},
			{Name: "config", MountPath: codexAuthSyncScriptDir, ReadOnly: true},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("48Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
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
	// Skill catalog image volume, read-only. Mounted UNDER the
	// workspace mount (/home/node/.openclaw) at a dedicated subpath;
	// the deeper mount wins for that subtree. The AW seed step copies
	// skill folders out of here into each per-agent workspace skills/.
	mounts = append(mounts, corev1.VolumeMount{
		Name:      skillsCatalogVolName,
		MountPath: skillsCatalogMountPath,
		ReadOnly:  true,
	})
	return mounts
}

// gatewayVolumes returns the pod's volumes. Always: openclaw
// config CM, workspace PVC, in-memory /dev/shm. When codex creds
// are configured, also project the secret's auth.json key. When
// `attachedKBs` is non-empty, each KB's PVC is added as a volume
// (paired with the matching mount in gatewayVolumeMounts).
// defaultSkillsImage is the OCI image holding the SKILL.md skill
// catalog (built from skills-image/ in the agent-office repo via
// Konflux). Mounted into the gateway pod as a Kubernetes image
// volume at skillsCatalogMountPath, then the AgentWorkstation seed
// step copies skill folders from it into each agent's workspace —
// the Red Hat skill-distribution pattern (cf. openshift/agentic-skills:
// SKILL.md folders in a plain image, consumed via image volume).
//
// Bump the tag here + re-release the operator to roll a new skill
// catalog (incl. bundled scripts/references) into agents.
const (
	defaultSkillsImage = "quay-quay-quay-test.apps.salamander.aimlworkbench.com/deanpeterson/agent-office-skills:v0.0.1"
	// MUST be a top-level path, NOT under /home/node/.openclaw. The
	// workspace PVC mounts at /home/node/.openclaw; a nested image
	// volume there gets shadowed by the PVC mount (the PVC mounts at
	// the parent AFTER the image volume mounts at the child, hiding
	// it — confirmed via /proc/mounts in v1.6.1). A sibling top-level
	// path avoids the shadowing.
	skillsCatalogMountPath = "/skills-catalog"
	// The skills image is built with `COPY skills /skills/`, so within
	// the mounted image rootfs the skill folders live under /skills.
	skillsCatalogSkillsDir = skillsCatalogMountPath + "/skills"
	skillsCatalogVolName   = "skills-catalog"
)

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
	// Skill catalog image volume (read-only). The kubelet pulls the
	// skills image and exposes its filesystem here; the AW seed step
	// copies skill folders from skillsCatalogMountPath into each
	// agent's per-workspace skills/ dir. Requires the ImageVolume
	// feature gate (GA on OCP 4.20+; verified enabled on this cluster).
	// The skills-catalog image is configurable per-gateway via
	// spec.skillsImage, so a new catalog version is rolled out by bumping
	// the gateway CR — no operator release. Empty ⇒ built-in default.
	// PullIfNotPresent suffices because each new catalog is a new tag (a
	// changed Reference ⇒ a fresh pull when the gateway pod is re-created).
	skillsImg := gw.Spec.SkillsImage
	if skillsImg == "" {
		skillsImg = defaultSkillsImage
	}
	vols = append(vols, corev1.Volume{
		Name: skillsCatalogVolName,
		VolumeSource: corev1.VolumeSource{
			Image: &corev1.ImageVolumeSource{
				Reference:  skillsImg,
				PullPolicy: corev1.PullIfNotPresent,
			},
		},
	})
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
func (r *AgentGatewayReconciler) reconcileGatewayChildren(ctx context.Context, gw *agentofficev1alpha1.AgentGateway, hooks hooksState) error {
	tok, err := r.reconcileGatewayTokenSecret(ctx, gw)
	if err != nil {
		return fmt.Errorf("token secret: %w", err)
	}
	if err := r.reconcileGatewayConfigMap(ctx, gw, tok, hooks.render); err != nil {
		return fmt.Errorf("configmap: %w", err)
	}
	if err := r.reconcileGatewayPVC(ctx, gw); err != nil {
		return fmt.Errorf("pvc: %w", err)
	}
	if err := r.reconcileGatewayDeployment(ctx, gw, hooks.secretKey); err != nil {
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
// hooks (spec.hooks, resolved) seeds the hooks block for a NEW
// gateway; existing PVCs get it via reconcileHooksConfig instead.
func (r *AgentGatewayReconciler) reconcileGatewayConfigMap(ctx context.Context, gw *agentofficev1alpha1.AgentGateway, gatewayToken string, hooks *templates.HooksRender) error {
	openclawJSON, err := templates.RenderAgentGatewayConfig(gw, gatewayToken, appsDomain(), hooks)
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
		cm.Data = map[string]string{
			"openclaw.json": openclawJSON,
			// Always rendered (harmless without codex creds); only the
			// codex-auth-sync sidecar, added when
			// spec.codexCredentialsSecretRef is set, executes it.
			codexAuthSyncScriptKey: templates.CodexAuthSyncScript,
		}
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
//
// hooksSecret (spec.hooks, resolved) wires the hook token into the
// openclaw container as OPENCLAW_HOOKS_TOKEN and adds its Secret to
// the Reloader annotation so a rotated token rolls the pod.
func (r *AgentGatewayReconciler) reconcileGatewayDeployment(ctx context.Context, gw *agentofficev1alpha1.AgentGateway, hooksSecret *corev1.SecretKeySelector) error {
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

	// v1.4.1: discover EnvFromSecret values from all AWs targeting
	// this gateway via spec.tools.mcpServers. These secrets get
	// envFrom'd onto the openclaw container so MCP server header
	// templates like `Authorization: "Bearer ${GITHUB_PERSONAL_ACCESS_TOKEN}"`
	// resolve at request time. The deduplicated, sorted list also
	// drives a Reloader annotation on the pod template so the pod
	// rolls when any of those Secrets rotate (e.g. ESO-managed
	// GitHub App installation tokens that refresh every 30min).
	mcpExtraSecrets, err := r.collectMCPEnvFromSecrets(ctx, gw)
	if err != nil {
		return fmt.Errorf("collecting mcp envFrom secrets: %w", err)
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gwDeployName(gw.Name),
			Namespace: gw.Namespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = mergeLabels(dep.Labels, labels)
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		// Reloader annotation: bounce the pod when any MCP-credential
		// Secret (or the hooks token Secret) rotates. Set
		// unconditionally so we DELETE the annotation when the last
		// AW drops its mcpServers entry and hooks are off.
		podAnnotations := map[string]string{}
		if hash := r.envSecretsHash(ctx, gw.Namespace, mcpExtraSecrets, hooksSecret); hash != "" {
			// Rolls the pod when any env-sourced credential ROTATES —
			// the operator's own analogue of the Reloader annotation it
			// replaces (Reloader's patches cannot survive this
			// wholesale re-render; see reloaderSecrets).
			podAnnotations["agentoffice.ai/env-secrets-sha"] = hash
		}
		if gw.Spec.CodexCredentialsSecretRef != "" {
			// See codexAuthSyncScriptHash — rolls the pod when the
			// sidecar script content changes.
			podAnnotations["agentoffice.ai/codex-auth-sync-script"] = codexAuthSyncScriptHash()
		}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: podAnnotations},
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
				Containers: gatewayContainers(image, gw, tokenSecretName, attachedKBs, mcpExtraSecrets, hooksSecret),
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

// collectMCPEnvFromSecrets walks all AgentWorkstations in the
// gateway's namespace, filters to those targeting THIS gateway via
// spec.runtime.shared.gatewayRef, and collects the deduplicated,
// sorted list of Secret names declared in their
// spec.tools.mcpServers[].envFromSecret fields that still need ENV
// delivery.
//
// v1.7.46: a Secret whose keys are referenced by the declaring
// server's headers (`Authorization: Bearer ${GITEA_TOKEN}`) is
// EXCLUDED here — the AW reconciler renders the literal value into
// openclaw.json (`openclaw mcp set`) and the runtime hot-reloads it,
// so envFrom + an envSecretsHash roll would only add a pointless pod
// restart per rotation. A Secret referenced by ANY server that does
// not consume it via headers keeps the env+hash path (the fallback
// for plain env injection), as does a Secret that cannot be read yet
// (its later creation must roll the pod exactly as before).
//
// Why on the AG reconciler (v1.4.1 fix): the AG reconciler OWNS the
// Deployment via SetControllerReference and rebuilds the pod
// template from scratch on every pass. v1.4.0 put the same merge
// logic on the AW reconciler, which raced and lost — every AW
// reconcile that patched the Deployment was immediately reverted
// by the next AG reconcile. Moving it here makes the merge part of
// the AG's authoritative state, so it survives. AW reconcile still
// calls `openclaw mcp set` (no Deployment write) and touches the
// AG to trigger a prompt re-reconcile.
func (r *AgentGatewayReconciler) collectMCPEnvFromSecrets(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) ([]string, error) {
	var awList agentofficev1alpha1.AgentWorkstationList
	if err := r.List(ctx, &awList, client.InNamespace(gw.Namespace)); err != nil {
		return nil, fmt.Errorf("listing AgentWorkstations: %w", err)
	}
	// Secret name → does any referencing server still need it as env?
	needsEnv := map[string]bool{}
	secretData := map[string]map[string][]byte{}
	lookup := func(name string) map[string][]byte {
		if d, ok := secretData[name]; ok {
			return d
		}
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: name}, &sec); err != nil {
			secretData[name] = nil // unreadable/absent → env fallback
		} else {
			secretData[name] = sec.Data
		}
		return secretData[name]
	}
	for _, aw := range awList.Items {
		// Include every agent on THIS gateway — shared agents that
		// reference it AND a dedicated agent whose own gateway this is.
		if effectiveGatewayRef(&aw) != gw.Name {
			continue
		}
		if aw.Spec.Tools == nil {
			continue
		}
		for i := range aw.Spec.Tools.MCPServers {
			srv := &aw.Spec.Tools.MCPServers[i]
			if srv.EnvFromSecret == "" {
				continue
			}
			data := lookup(srv.EnvFromSecret)
			if data == nil || !mcpServerConsumesSecretViaConfig(srv, data) {
				needsEnv[srv.EnvFromSecret] = true
			} else if _, seen := needsEnv[srv.EnvFromSecret]; !seen {
				needsEnv[srv.EnvFromSecret] = false
			}
		}
	}
	out := make([]string, 0, len(needsEnv))
	for s, env := range needsEnv {
		if env {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out, nil
}

// gatewayContainers returns the pod's container list: always
// the openclaw runtime, plus one git-sync sidecar per attached
// KnowledgeBase whose spec.gitMirror is configured. Sidecars
// share the wiki PVC volume already in the pod (no rsync between
// containers — both read/write the same files).
//
// mcpExtraSecrets (v1.4.1+) is the deduplicated list of
// EnvFromSecret values declared across all AgentWorkstations
// targeting this gateway via spec.tools.mcpServers — gives openclaw
// the env vars it needs to resolve ${VAR} references in MCP
// server header configs (e.g. ${GITHUB_PERSONAL_ACCESS_TOKEN}).
//
// hooksSecret, when non-nil, is the Secret key the hook token is read
// from (spec.hooks.tokenSecretRef, resolved); see gatewayEnv.
func gatewayContainers(image string, gw *agentofficev1alpha1.AgentGateway, tokenSecretName string, attachedKBs []agentofficev1alpha1.KnowledgeBase, mcpExtraSecrets []string, hooksSecret *corev1.SecretKeySelector) []corev1.Container {
	out := []corev1.Container{{
		Name:            "openclaw",
		Image:           image,
		ImagePullPolicy: corev1.PullAlways,
		Ports: []corev1.ContainerPort{{
			Name: "gateway", ContainerPort: 18789, Protocol: corev1.ProtocolTCP,
		}},
		Env:          gatewayEnv(hooksSecret),
		EnvFrom:      gatewayEnvFrom(gw, tokenSecretName, mcpExtraSecrets),
		VolumeMounts: gatewayVolumeMounts(gw, attachedKBs),
	}}
	// codex-auth-sync sidecar: propagates Vault → Secret → emptyDir
	// updates so the user's re-auth dialog flow rotates the token
	// without a pod restart, and re-seeds any agent auth store that
	// lost its openai profile. Only added when Codex creds are wired.
	if gw.Spec.CodexCredentialsSecretRef != "" {
		out = append(out, codexAuthSyncContainer(image))
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
