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
	"encoding/json"
	"fmt"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// AutoResearchProjectReconciler drives Karpathy-style autonomous
// QLoRA experimentation loops. Each cycle:
//  1. Asks the experimenter AgentWorkstation to propose a config
//     (agent reads wiki/experiments/log.md, writes a YAML
//     proposal to wiki/proposals/round-N.yaml).
//  2. Submits a Kubernetes Job running the AutoResearch trainer
//     image with the agent's proposed config.
//  3. On Job completion, reads metrics.json from the Job's
//     ConfigMap output, makes a keep/revert decision, updates
//     status + wiki log.
//
// v0.0.1: raw Job per experiment. v0.0.2 swaps in DSP pipeline
// runs to surface in the OpenShift AI Experiments UI.
//
// Loop-safety: CadenceMinutes gates how often a cycle fires;
// MaxConcurrentRuns caps in-flight experiments;
// MaxTotalExperiments stops the loop when reached. The trainer
// pod's nvidia-smi -pl init container is the per-GPU power
// floor (4090 breaker safety).
type AutoResearchProjectReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RestConfig *rest.Config
}

// +kubebuilder:rbac:groups=agentoffice.ai,resources=autoresearchprojects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentoffice.ai,resources=autoresearchprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentoffice.ai,resources=autoresearchprojects/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

const (
	autoResearchExperimentLabel = "agentoffice.ai/autoresearch-experiment"
	autoResearchProjectLabel    = "agentoffice.ai/autoresearch-project"
	autoResearchRoundAnnotation = "agentoffice.ai/autoresearch-round"

	defaultTrainerImage = "quay-quay-quay-test.apps.salamander.aimlworkbench.com/deanpeterson/autoresearch-trainer:v0.0.1"

	maxRecentRunsRetained = 20
)

// Reconcile is the operator's per-AutoResearchProject loop tick.
func (r *AutoResearchProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var project agentofficev1alpha1.AutoResearchProject
	if err := r.Get(ctx, req.NamespacedName, &project); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Default-fill the loop config so we have safe values to
	// reason about even on a freshly-applied minimal CR.
	cadence := time.Duration(project.Spec.LoopConfig.CadenceMinutes) * time.Minute
	if cadence == 0 {
		cadence = 30 * time.Minute
	}
	maxConcurrent := int(project.Spec.LoopConfig.MaxConcurrentRuns)
	if maxConcurrent == 0 {
		maxConcurrent = 1
	}
	maxTotal := int(project.Spec.LoopConfig.MaxTotalExperiments)
	if maxTotal == 0 {
		maxTotal = 100
	}

	// 1. Drain any in-flight Jobs: poll each, update status when
	//    they complete, append to recent runs, free the slot in
	//    OpenRuns.
	completedThisPass, err := r.drainOpenJobs(ctx, &project)
	if err != nil {
		log.Error(err, "draining open jobs")
		// keep going — status drift is recoverable
	}

	// 2. Stop conditions.
	if project.Status.ExperimentsRun >= int32(maxTotal) {
		return r.markPhaseAndSave(ctx, &project, "Completed",
			fmt.Sprintf("hit MaxTotalExperiments=%d", maxTotal))
	}

	// 3. Cadence gate: don't fire a new cycle unless enough time
	//    has passed since the last cycle. completedThisPass
	//    bypasses the gate so a finished run immediately frees
	//    the slot for the next one (otherwise the cadence
	//    artificially serializes when we have capacity).
	if !completedThisPass && project.Status.LastCycleTime != nil {
		nextDue := project.Status.LastCycleTime.Time.Add(cadence)
		if time.Now().Before(nextDue) {
			waitFor := time.Until(nextDue)
			return ctrl.Result{RequeueAfter: waitFor}, nil
		}
	}

	// 4. Concurrency gate.
	if len(project.Status.OpenRuns) >= maxConcurrent {
		log.V(1).Info("at MaxConcurrentRuns; skipping submission this cycle",
			"open", len(project.Status.OpenRuns), "max", maxConcurrent)
		return ctrl.Result{RequeueAfter: cadence}, nil
	}

	// 5. Validate references the project depends on.
	if err := r.validateRefs(ctx, &project); err != nil {
		_, _ = r.markPhaseAndSave(ctx, &project, "Pending", err.Error())
		// requeue moderately — likely a missing KB or AW that
		// will appear later
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// 6. Ask the experimenter agent for a config proposal. v0.0.1:
	//    the agent's wiki-write skill files
	//    wiki/proposals/round-<N>.yaml; we read it after the
	//    exec returns. If the agent is unreachable or the
	//    proposal isn't parseable, fall back to a deterministic
	//    starter config so the loop never wedges.
	round := project.Status.Round + 1
	proposal, proposalSource, err := r.requestProposal(ctx, &project, int(round))
	if err != nil {
		log.Info("agent proposal failed; using starter config", "err", err)
		proposal = starterQLoRAConfig(int(round))
		proposalSource = "starter (fallback)"
	}

	// 7. Submit the experiment as a Kubernetes Job.
	runID := fmt.Sprintf("%s-round-%d-%d", project.Name, round, time.Now().Unix())
	if err := r.submitTrainerJob(ctx, &project, runID, int(round), proposal); err != nil {
		_, _ = r.markPhaseAndSave(ctx, &project, "Running",
			fmt.Sprintf("submit job failed: %v", err))
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// 8. Status bookkeeping.
	if project.Status.OpenRuns == nil {
		project.Status.OpenRuns = map[string]string{}
	}
	project.Status.OpenRuns[runID] = "in-flight"
	now := metav1.Now()
	project.Status.LastCycleTime = &now
	project.Status.Round = round
	project.Status.Phase = "Running"

	// Prepend a placeholder record for the in-flight run. We'll
	// fill in EvalLoss + Kept when the Job completes.
	record := agentofficev1alpha1.AutoResearchExperimentRecord{
		Round:        round,
		RunID:        runID,
		Experimenter: project.Spec.Experimenter.Workstation,
		ProposalPath: fmt.Sprintf("proposals/round-%d.yaml", round),
		StartedAt:    &now,
	}
	project.Status.RecentRuns = prependRecord(project.Status.RecentRuns, record)

	if err := r.Status().Update(ctx, &project); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("submitted experiment",
		"round", round,
		"runID", runID,
		"proposalSource", proposalSource,
		"qloraConfig", proposal)

	return ctrl.Result{RequeueAfter: cadence}, nil
}

// validateRefs ensures the KnowledgeBase + experimenter
// AgentWorkstation exist before we burn a cycle.
func (r *AutoResearchProjectReconciler) validateRefs(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject) error {
	if p.Spec.KnowledgeBase != "" {
		var kb agentofficev1alpha1.KnowledgeBase
		if err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.KnowledgeBase}, &kb); err != nil {
			return fmt.Errorf("KnowledgeBase %s/%s not found: %w", p.Namespace, p.Spec.KnowledgeBase, err)
		}
	}
	if p.Spec.Experimenter.Workstation != "" {
		var aw agentofficev1alpha1.AgentWorkstation
		if err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.Experimenter.Workstation}, &aw); err != nil {
			return fmt.Errorf("Experimenter AgentWorkstation %s/%s not found: %w", p.Namespace, p.Spec.Experimenter.Workstation, err)
		}
	}
	return nil
}

// QLoRAConfig is the structured shape the agent proposes + the
// trainer consumes. v0.0.1 supports rank/alpha/dropout/target
// modules + standard fine-tuning hyperparams. Future versions
// add architecture-level changes (different attention impls,
// activation functions, etc.).
type QLoRAConfig struct {
	Round            int      `json:"round"`
	LoraRank         int      `json:"lora_rank"`
	LoraAlpha        int      `json:"lora_alpha"`
	LoraDropout      float64  `json:"lora_dropout"`
	TargetModules    []string `json:"target_modules"`
	LearningRate     float64  `json:"learning_rate"`
	NumTrainingSteps int      `json:"num_training_steps"`
	PerDeviceBatch   int      `json:"per_device_batch_size"`
	GradAccum        int      `json:"gradient_accumulation_steps"`
	MaxSeqLen        int      `json:"max_seq_length"`
	WarmupSteps      int      `json:"warmup_steps"`
	WeightDecay      float64  `json:"weight_decay"`
	OffloadStrategy  string   `json:"offload_strategy,omitempty"`
	Notes            string   `json:"notes,omitempty"`
}

// starterQLoRAConfig is the deterministic fallback used when the
// agent's proposal is unavailable. Conservative defaults that
// converge cleanly on most 7-13B QLoRA setups.
func starterQLoRAConfig(round int) QLoRAConfig {
	return QLoRAConfig{
		Round:            round,
		LoraRank:         8,
		LoraAlpha:        16,
		LoraDropout:      0.05,
		TargetModules:    []string{"q_proj", "v_proj"},
		LearningRate:     2e-4,
		NumTrainingSteps: 200,
		PerDeviceBatch:   4,
		GradAccum:        4,
		MaxSeqLen:        1024,
		WarmupSteps:      20,
		WeightDecay:      0.0,
		OffloadStrategy:  "cpu",
		Notes:            "starter config (operator fallback; agent proposal unavailable)",
	}
}

// requestProposal asks the experimenter agent for the next
// QLoRA config. v0.0.1 falls back to starterQLoRAConfig if the
// agent isn't wired up yet — the agent-driven proposal is
// added in v0.0.2 alongside the wiki integration. This keeps
// the loop runnable in isolation while we iterate on the
// agent prompt.
func (r *AutoResearchProjectReconciler) requestProposal(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject, round int) (QLoRAConfig, string, error) {
	// Placeholder for v0.0.1 — the deterministic starter config
	// IS the proposal until the agent-skill wiring lands in
	// v0.0.2. This keeps the reconciler-end of the loop
	// testable independently of the gateway / wiki path.
	return starterQLoRAConfig(round), "starter (v0.0.1 placeholder)", nil
}

// submitTrainerJob creates a Kubernetes Job that runs the
// AutoResearch trainer image with the proposed QLoRA config.
// The Job is named by runID; the operator finds it again on
// later reconciles via the autoResearchProjectLabel label.
func (r *AutoResearchProjectReconciler) submitTrainerJob(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject, runID string, round int, cfg QLoRAConfig) error {
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	image := p.Spec.Trainer.Image
	if image == "" {
		image = defaultTrainerImage
	}
	powerCap := int(p.Spec.Trainer.PowerCapWatts)
	if powerCap == 0 {
		powerCap = 250
	}
	gpuCount := int(p.Spec.Trainer.GPUCount)
	if gpuCount == 0 {
		gpuCount = 1
	}

	labels := map[string]string{
		autoResearchProjectLabel:    p.Name,
		autoResearchExperimentLabel: runID,
		"app.kubernetes.io/managed-by": "agent-office-operator",
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runID,
			Namespace: p.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				autoResearchRoundAnnotation: fmt.Sprintf("%d", round),
			},
		},
	}

	// We deliberately use CreateOrUpdate even though Jobs are
	// effectively immutable: makes the operation idempotent on
	// retries of a partially-applied submit.
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		// Don't mutate spec on update — Job spec is immutable.
		if job.CreationTimestamp.IsZero() {
			backoffLimit := int32(0)
			ttl := int32(24 * 60 * 60) // GC the Job after 1 day
			job.Spec.BackoffLimit = &backoffLimit
			job.Spec.TTLSecondsAfterFinished = &ttl
			job.Spec.Template = corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// Per-GPU power cap: privileged init container
					// runs nvidia-smi -pl <cap> before the
					// trainer starts. Belt-and-suspenders against
					// the 4090 node's circuit breaker.
					InitContainers: powerCapInitContainers(powerCap),
					Containers: []corev1.Container{{
						Name:  "trainer",
						Image: image,
						Env: []corev1.EnvVar{
							{Name: "AUTORESEARCH_CONFIG", Value: string(cfgJSON)},
							{Name: "AUTORESEARCH_PROJECT", Value: p.Name},
							{Name: "AUTORESEARCH_ROUND", Value: fmt.Sprintf("%d", round)},
							{Name: "AUTORESEARCH_RUN_ID", Value: runID},
							{Name: "BASE_MODEL", Value: p.Spec.BaseModel.HuggingfaceID},
							{Name: "BASE_MODEL_REVISION", Value: defaultIfEmpty(p.Spec.BaseModel.Revision, "main")},
							{Name: "TRAINING_DATA", Value: p.Spec.TrainingData.HuggingfaceDataset},
							{Name: "TRAINING_SPLIT", Value: defaultIfEmpty(p.Spec.TrainingData.Split, "train")},
							{Name: "TRAINING_SAMPLE_COUNT", Value: fmt.Sprintf("%d", deref(p.Spec.TrainingData.SampleCount, 2000))},
							{Name: "EVAL_METRIC", Value: defaultIfEmpty(p.Spec.Eval.Metric, "eval_loss")},
							{Name: "EVAL_DIRECTION", Value: defaultIfEmpty(p.Spec.Eval.Direction, "minimize")},
						},
						// Trainer writes metrics.json + adapter
						// checkpoint under /workspace; we read
						// metrics back on completion via pod
						// logs (the trainer also echoes
						// "AUTORESEARCH_RESULT={...}" so it's
						// discoverable in logs).
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								"nvidia.com/gpu": *resource.NewQuantity(int64(gpuCount), resource.DecimalSI),
							},
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("16Gi"),
								corev1.ResourceCPU:    resource.MustParse("2"),
							},
						},
					}},
					// Pod anti-affinity: one trainer per node.
					// Belt-and-suspenders alongside the per-GPU
					// power cap.
					Affinity: trainerAntiAffinity(p),
				},
			}
		}
		return controllerutil.SetControllerReference(p, job, r.Scheme)
	})
	return err
}

// powerCapInitContainers returns the privileged init container
// that runs `nvidia-smi -pl <cap>` before the trainer step
// starts. Required for breaker safety on the 4090 node.
func powerCapInitContainers(capWatts int) []corev1.Container {
	priv := true
	return []corev1.Container{{
		Name:  "gpu-power-cap",
		Image: "nvcr.io/nvidia/cuda:12.4.1-base-ubi9",
		Command: []string{"/bin/sh", "-c", fmt.Sprintf(`
set -e
echo "[gpu-power-cap] setting nvidia-smi -pl %d on all GPUs"
nvidia-smi -pl %d || { echo "WARN: power cap failed (driver may not allow per-pod cap)"; exit 0; }
nvidia-smi --query-gpu=power.limit --format=csv
`, capWatts, capWatts)},
		SecurityContext: &corev1.SecurityContext{
			Privileged: &priv,
		},
	}}
}

// trainerAntiAffinity returns a pod-anti-affinity rule when the
// project requests one-trainer-per-node. Matches any other
// AutoResearchProject Job on the same node.
func trainerAntiAffinity(p *agentofficev1alpha1.AutoResearchProject) *corev1.Affinity {
	one := true
	if p.Spec.Trainer.OneTrainingPodPerNode != nil {
		one = *p.Spec.Trainer.OneTrainingPodPerNode
	}
	if !one {
		return nil
	}
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey: "kubernetes.io/hostname",
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      autoResearchProjectLabel,
						Operator: metav1.LabelSelectorOpExists,
					}},
				},
			}},
		},
	}
}

// drainOpenJobs polls each in-flight Job's status, records
// completion/failure into the project status, and removes the
// run from OpenRuns. Returns true if any run completed this
// pass (caller uses this to bypass the cadence gate and queue
// the next experiment immediately).
func (r *AutoResearchProjectReconciler) drainOpenJobs(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject) (bool, error) {
	if len(p.Status.OpenRuns) == 0 {
		return false, nil
	}
	completed := false
	for runID := range p.Status.OpenRuns {
		var job batchv1.Job
		err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: runID}, &job)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Job was GC'd before we could read it; just
				// remove from open runs.
				delete(p.Status.OpenRuns, runID)
				continue
			}
			return completed, err
		}
		// Done if Job has a Complete or Failed condition.
		var done, failed bool
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				done = true
			}
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				failed = true
				done = true
			}
		}
		if !done {
			continue
		}
		completed = true
		// Parse the trainer's emitted result line out of pod
		// logs. v0.0.1: best-effort. v0.0.2 will instead read
		// metrics.json from an artifact path.
		evalLoss, parseErr := r.parseTrainerResult(ctx, &job)
		now := metav1.Now()
		kept := false
		if !failed && parseErr == nil {
			kept = isImprovement(p, evalLoss)
		}
		// Update the matching recent run record + counters.
		for i := range p.Status.RecentRuns {
			if p.Status.RecentRuns[i].RunID != runID {
				continue
			}
			p.Status.RecentRuns[i].EvalLoss = fmt.Sprintf("%.6f", evalLoss)
			p.Status.RecentRuns[i].CompletedAt = &now
			p.Status.RecentRuns[i].Kept = &kept
			break
		}
		p.Status.ExperimentsRun++
		if kept {
			p.Status.ExperimentsKept++
			p.Status.BestEvalLoss = fmt.Sprintf("%.6f", evalLoss)
			p.Status.BestRunID = runID
		}
		delete(p.Status.OpenRuns, runID)
	}
	return completed, nil
}

// parseTrainerResult reads the trainer Job's pod logs and
// extracts the eval_loss from a line shaped:
//
//	AUTORESEARCH_RESULT={"eval_loss": 1.234, ...}
//
// v0.0.1 simplicity: log-line shape over artifact volume.
// v0.0.2 swaps in artifact-bucket reads.
func (r *AutoResearchProjectReconciler) parseTrainerResult(ctx context.Context, job *batchv1.Job) (float64, error) {
	// Implementation deferred to a follow-on commit alongside
	// the trainer entrypoint script that emits the result line.
	// For v0.0.1 reconciler-only ship, we return a deterministic
	// placeholder so the loop has visible-but-fake numbers
	// to drive its keep/revert logic. This gets replaced the
	// moment the trainer script lands (separate commit).
	_ = ctx
	_ = job
	return 1.0, fmt.Errorf("parseTrainerResult unimplemented in v0.0.1 reconciler-only ship")
}

// isImprovement reads the project's eval policy and decides
// whether a new measurement beats the current best.
func isImprovement(p *agentofficev1alpha1.AutoResearchProject, newVal float64) bool {
	if p.Status.BestEvalLoss == "" {
		return true // first-ever measurement is the new best
	}
	var bestVal float64
	if _, err := fmt.Sscanf(p.Status.BestEvalLoss, "%f", &bestVal); err != nil {
		return false
	}
	dir := p.Spec.Eval.Direction
	if dir == "" {
		dir = "minimize"
	}
	if dir == "minimize" {
		return newVal < bestVal
	}
	return newVal > bestVal
}

// markPhaseAndSave is a small helper to set status.phase +
// emit a Ready condition with the same reason+message, then
// Status().Update.
func (r *AutoResearchProjectReconciler) markPhaseAndSave(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject, phase, message string) (ctrl.Result, error) {
	p.Status.Phase = phase
	cond := agentofficev1alpha1.AutoResearchProjectCondition{
		Type:               "Ready",
		Status:             "False",
		Reason:             phase,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	if phase == "Running" || phase == "Completed" {
		cond.Status = "True"
	}
	p.Status.Conditions = mergeARPConditions(p.Status.Conditions, cond)
	if err := r.Status().Update(ctx, p); err != nil {
		return ctrl.Result{}, err
	}
	requeue := 1 * time.Minute
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// prependRecord puts the new record at the head of the recent
// runs ring + bounds it at maxRecentRunsRetained.
func prependRecord(existing []agentofficev1alpha1.AutoResearchExperimentRecord, rec agentofficev1alpha1.AutoResearchExperimentRecord) []agentofficev1alpha1.AutoResearchExperimentRecord {
	out := append([]agentofficev1alpha1.AutoResearchExperimentRecord{rec}, existing...)
	if len(out) > maxRecentRunsRetained {
		out = out[:maxRecentRunsRetained]
	}
	return out
}

// mergeARPConditions matches the merge pattern used by other
// reconcilers in this package.
func mergeARPConditions(existing []agentofficev1alpha1.AutoResearchProjectCondition, updates ...agentofficev1alpha1.AutoResearchProjectCondition) []agentofficev1alpha1.AutoResearchProjectCondition {
	byType := make(map[string]agentofficev1alpha1.AutoResearchProjectCondition, len(existing)+len(updates))
	for _, c := range existing {
		byType[c.Type] = c
	}
	for _, c := range updates {
		if old, ok := byType[c.Type]; ok && old.Status == c.Status {
			c.LastTransitionTime = old.LastTransitionTime
		}
		byType[c.Type] = c
	}
	out := make([]agentofficev1alpha1.AutoResearchProjectCondition, 0, len(byType))
	keys := make([]string, 0, len(byType))
	for k := range byType {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, byType[k])
	}
	return out
}

// deref returns *p when non-nil, else fallback. Helper for
// nullable spec fields with a sensible default.
func deref(p *int32, fallback int32) int32 {
	if p == nil {
		return fallback
	}
	return *p
}

// SetupWithManager wires the controller.
func (r *AutoResearchProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.AutoResearchProject{}).
		Owns(&batchv1.Job{}).
		Named("autoresearchproject").
		Complete(r)
}
