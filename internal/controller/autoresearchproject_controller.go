/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
	"github.com/enterprisewebservice/agent-office-operator/internal/dsp"
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
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

const (
	autoResearchExperimentLabel = "agentoffice.ai/autoresearch-experiment"
	autoResearchProjectLabel    = "agentoffice.ai/autoresearch-project"
	autoResearchRoundAnnotation = "agentoffice.ai/autoresearch-round"

	defaultTrainerImage = "quay-quay-quay-test.apps.salamander.aimlworkbench.com/deanpeterson/autoresearch-trainer:v0.0.13"

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

	// 1b. Reconcile DSP registration (uploads new pipeline
	//     version if our embedded pipeline.yaml drifted from
	//     what's in DSP — e.g. a new operator version with a
	//     bumped trainer image tag) and then cull any
	//     in-flight runs that were submitted against the old
	//     pipeline version. Without this, stale runs keep
	//     wasting GPU on a doomed trainer config until they
	//     fail naturally. We do this BEFORE the cadence gate
	//     so a culled run frees a slot immediately for a fresh
	//     submission on the new pipeline version.
	_, currentVersionID, _, regErr := r.ensureDSPRegistration(ctx, &project)
	if regErr != nil {
		log.Error(regErr, "ensure DSP registration (continuing; cull skipped)")
		// Surface the DSP rejection on Status so it's visible via
		// `oc describe`/`oc get conditions` without log-tailing.
		// Caught real failure: v0.0.46 silently shipped a kfp
		// schema (platforms.kubernetes.secretAsVolume) that RHOAI
		// 3.3.1's DSP rejects with "unknown template format" —
		// loop kept reusing the prior version so no one noticed
		// for hours.
		project.Status.Conditions = mergeARPConditions(project.Status.Conditions,
			agentofficev1alpha1.AutoResearchProjectCondition{
				Type:               "PipelineUploadOK",
				Status:             "False",
				Reason:             "DSPRejected",
				Message:            truncateMsg(regErr.Error(), 256),
				LastTransitionTime: metav1.Now(),
			})
	} else {
		project.Status.Conditions = mergeARPConditions(project.Status.Conditions,
			agentofficev1alpha1.AutoResearchProjectCondition{
				Type:               "PipelineUploadOK",
				Status:             "True",
				Reason:             "Uploaded",
				Message:            "embedded pipeline.yaml accepted by DSP (version " + currentVersionID + ")",
				LastTransitionTime: metav1.Now(),
			})
		if currentVersionID != "" {
			culled, cerr := r.cullStaleOpenRuns(ctx, &project, currentVersionID)
			if cerr != nil {
				log.Error(cerr, "culling stale open runs")
			}
			if culled > 0 {
				// Treat culling as completion-this-pass so the
				// cadence gate doesn't artificially delay the
				// replacement run.
				completedThisPass = true
			}
		}
	}

	// 1c. Periodically clean up orphaned MLMD Executions stuck
	//     in RUNNING state. kfp v2's launcher doesn't always
	//     close its Executions when a run dies mid-flight,
	//     so the OpenShift AI dashboard's Pipelines/Executions
	//     view fills with stale "Running" entries that aren't
	//     actually running anywhere. Internal cooldown (10 min)
	//     prevents hammering the DSP MariaDB. Errors logged
	//     but never block the reconcile.
	if _, mlmdErr := r.cleanupStaleMLMDExecutions(ctx, &project); mlmdErr != nil {
		log.V(1).Info("mlmd janitor skipped this pass", "err", mlmdErr.Error())
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

	// 7. Submit the experiment as a DSP pipeline run. RunID is
	//    deterministic on (project, round) so DSP's API and our
	//    status surface use the same handle, and concurrent
	//    reconciles converge instead of submitting duplicates.
	//    The kfp pipeline (one container step running the
	//    AutoResearch trainer image) lands in OpenShift AI's
	//    Develop & train → Experiments UI, grouped under
	//    spec.experimentName (or the project name).
	runID := fmt.Sprintf("%s-round-%d", project.Name, round)
	dspRun, err := r.submitDSPRun(ctx, &project, runID, int(round), proposal)
	if err != nil {
		_, _ = r.markPhaseAndSave(ctx, &project, "Running",
			fmt.Sprintf("submit DSP run failed: %v", err))
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// 8. Status bookkeeping. We set LastCycleTime + Round +
	//    OpenRuns in one Status().Update so a racing
	//    reconciler that reads our cluster state sees a
	//    coherent "cycle is in flight" snapshot.
	if project.Status.OpenRuns == nil {
		project.Status.OpenRuns = map[string]string{}
	}
	// Store the DSP-issued run ID so drain can poll it. Our
	// deterministic display name (runID) and DSP's internal
	// run_id are separate; we key by display name in OpenRuns
	// and stash the DSP id as the value.
	project.Status.OpenRuns[runID] = dspRun.RunID
	now := metav1.Now()
	project.Status.LastCycleTime = &now
	project.Status.Round = round
	project.Status.Phase = "Running"

	// v0.0.65: refresh Ready=True with a CURRENT message on every
	// successful submit. Previously a once-stuck failure message
	// (e.g. v0.0.49's "submit DSP run failed: unknown template
	// format") could persist on the Ready condition long after the
	// problem was fixed because markPhaseAndSave wasn't called on
	// the happy path. Updating here guarantees the message reflects
	// the most-recent reconcile.
	project.Status.Conditions = mergeARPConditions(project.Status.Conditions,
		agentofficev1alpha1.AutoResearchProjectCondition{
			Type:   "Ready",
			Status: "True",
			Reason: "Running",
			Message: fmt.Sprintf("submitted round %d (runID=%s, dspRunID=%s)",
				round, runID, dspRun.RunID),
			LastTransitionTime: now,
		})

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

// validationQLoRAConfig is the smoke-test profile triggered by
// spec.validationMode=true. Designed to complete on a single
// 24GB GPU in 3-5 min including model load — small enough to
// validate end-to-end plumbing without burning a real run's
// worth of GPU time. Loss numbers from this profile mean
// nothing; the only signal we want is "did the pipeline run
// reach AUTORESEARCH_RESULT= and did the operator scrape it."
func validationQLoRAConfig(round int) QLoRAConfig {
	return QLoRAConfig{
		Round:            round,
		LoraRank:         4,
		LoraAlpha:        8,
		LoraDropout:      0.05,
		TargetModules:    []string{"q_proj", "v_proj"},
		LearningRate:     2e-4,
		NumTrainingSteps: 10,
		PerDeviceBatch:   1,
		GradAccum:        1,
		MaxSeqLen:        256,
		WarmupSteps:      0,
		WeightDecay:      0.0,
		OffloadStrategy:  "cpu",
		Notes:            "VALIDATION mode (10 steps, batch 1, seq 256); loss meaningless",
	}
}

// requestProposal asks the experimenter agent for the next
// QLoRA config. v0.0.1 falls back to starterQLoRAConfig if the
// agent isn't wired up yet — the agent-driven proposal is
// added in v0.0.2 alongside the wiki integration. This keeps
// the loop runnable in isolation while we iterate on the
// agent prompt.
//
// spec.validationMode=true short-circuits everything to the
// validation profile — neither the agent nor the starter is
// consulted, since the whole point of validation mode is to
// run a tiny known-shape experiment.
func (r *AutoResearchProjectReconciler) requestProposal(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject, round int) (QLoRAConfig, string, error) {
	log := logf.FromContext(ctx)

	if p.Spec.ValidationMode {
		return validationQLoRAConfig(round), "validation (spec.validationMode=true)", nil
	}

	// v0.0.65: try to get a real proposal from the experimenter agent.
	// Failure falls back to the starter config so the loop never
	// wedges — AgentIntegrationOK condition surfaces what went wrong.
	cfg, source, err := r.proposeViaAgent(ctx, p, round)
	if err == nil {
		p.Status.Conditions = mergeARPConditions(p.Status.Conditions,
			agentofficev1alpha1.AutoResearchProjectCondition{
				Type:               "AgentIntegrationOK",
				Status:             "True",
				Reason:             "AgentReplied",
				Message:            fmt.Sprintf("experimenter %s proposed round %d", p.Spec.Experimenter.Workstation, round),
				LastTransitionTime: metav1.Now(),
			})
		return cfg, source, nil
	}
	log.Info("agent proposal failed; falling back to starter config", "err", err.Error())
	p.Status.Conditions = mergeARPConditions(p.Status.Conditions,
		agentofficev1alpha1.AutoResearchProjectCondition{
			Type:               "AgentIntegrationOK",
			Status:             "False",
			Reason:             "AgentFallback",
			Message:            truncateMsg(fmt.Sprintf("falling back to starter config: %v", err), 256),
			LastTransitionTime: metav1.Now(),
		})
	return starterQLoRAConfig(round), "starter (agent fallback)", nil
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
						Name:    "trainer",
						Image:   image,
						Command: []string{"python", "/opt/autoresearch/run.py"},
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

// drainOpenJobs polls each in-flight DSP run's status (the
// historical name is kept for diff legibility against the v0.0.1
// Job-based code; this implementation talks to DSP, not batch/v1
// Jobs). Records completion/failure into the project status,
// removes the run from OpenRuns. Returns true if any run
// completed this pass — caller uses this to bypass the cadence
// gate and queue the next experiment immediately when capacity
// frees mid-cycle.
func (r *AutoResearchProjectReconciler) drainOpenJobs(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject) (bool, error) {
	log := logf.FromContext(ctx)
	if len(p.Status.OpenRuns) == 0 {
		return false, nil
	}
	dspClient, err := dspClientFor(ctx, r.Client, p.Namespace)
	if err != nil {
		// DSP unreachable — don't drop OpenRuns; we'll retry
		// next reconcile.
		return false, fmt.Errorf("dspClientFor: %w", err)
	}
	completed := false
	for displayID, dspRunID := range p.Status.OpenRuns {
		run, err := dspClient.GetRun(ctx, dspRunID)
		if err != nil {
			// Transient or our run was deleted — keep entry
			// for next cycle's retry.
			continue
		}
		if !isRunTerminal(run.State) {
			continue
		}
		completed = true
		var evalLoss float64
		var adapterURI string
		var kept bool
		if run.State == "SUCCEEDED" {
			// v0.0.43: fetch the full trainer result so we
			// can record adapter_uri + model_version_id in
			// status.recentRuns, not just eval_loss.
			if full := r.readFullResultFromDSPRun(ctx, p, run); full != nil {
				evalLoss = full.EvalLoss
				adapterURI = full.AdapterURI
			}
			kept = isImprovement(p, evalLoss)

			// v0.0.65: verify the adapter URI is actually
			// pullable from the registry before trusting the
			// trainer's "I pushed it" claim. Silent persistence
			// skips (round 17 in v0.0.47/v0.0.12 — trainer warned
			// "no quay-push-secret mounted" and the loop kept
			// happily marking rounds Succeeded) now produce a
			// loud, status-visible AdapterPushOK=False condition.
			//
			// We only set the condition when persistence was
			// actually expected — i.e., either spec.adapterStorage
			// is configured OR the trainer reported a non-empty
			// adapter_uri. Otherwise we'd flap False on every
			// project that legitimately doesn't want persistence.
			storageWanted := p.Spec.AdapterStorage != nil
			if adapterURI != "" {
				status, detail := r.verifyAdapterArtifact(ctx, p, adapterURI)
				condStatus := "True"
				reason := "Verified"
				if status == "Missing" {
					condStatus = "False"
					reason = "ManifestMissing"
				} else if status == "Unknown" {
					condStatus = "Unknown"
					reason = "VerifyInconclusive"
				}
				p.Status.Conditions = mergeARPConditions(p.Status.Conditions,
					agentofficev1alpha1.AutoResearchProjectCondition{
						Type:               "AdapterPushOK",
						Status:             condStatus,
						Reason:             reason,
						Message:            detail,
						LastTransitionTime: metav1.Now(),
					})
			} else if storageWanted {
				p.Status.Conditions = mergeARPConditions(p.Status.Conditions,
					agentofficev1alpha1.AutoResearchProjectCondition{
						Type:   "AdapterPushOK",
						Status: "False",
						Reason: "EmptyURI",
						Message: "spec.adapterStorage is configured but trainer reported an " +
							"empty adapter_uri — the push step in run.py likely failed " +
							"silently (check the round's pod logs for a [autoresearch] " +
							"WARN line)",
						LastTransitionTime: metav1.Now(),
					})
			}

			// v0.0.65: write the result to the wiki repo so the
			// agent (and humans) can read it on the next cycle.
			// Best-effort — wiki commit failures shouldn't fail
			// the round's status update.
			round := extractRoundFromRunID(displayID, p.Name)
			if werr := r.wikiPushResult(ctx, p, int(round), evalLoss, adapterURI, kept); werr != nil {
				log.Info("wiki result commit failed (continuing)", "round", round, "err", werr.Error())
				p.Status.Conditions = mergeARPConditions(p.Status.Conditions,
					agentofficev1alpha1.AutoResearchProjectCondition{
						Type:               "WikiSyncOK",
						Status:             "False",
						Reason:             "WriteFailed",
						Message:            truncateMsg(werr.Error(), 256),
						LastTransitionTime: metav1.Now(),
					})
			} else {
				p.Status.Conditions = mergeARPConditions(p.Status.Conditions,
					agentofficev1alpha1.AutoResearchProjectCondition{
						Type:               "WikiSyncOK",
						Status:             "True",
						Reason:             "Committed",
						Message:            fmt.Sprintf("results/round-%d.md + log/log.md committed to wiki", round),
						LastTransitionTime: metav1.Now(),
					})
			}
		}
		now := metav1.Now()
		for i := range p.Status.RecentRuns {
			if p.Status.RecentRuns[i].RunID != displayID {
				continue
			}
			if run.State == "SUCCEEDED" {
				p.Status.RecentRuns[i].EvalLoss = fmt.Sprintf("%.6f", evalLoss)
				p.Status.RecentRuns[i].Kept = &kept
				if adapterURI != "" {
					p.Status.RecentRuns[i].AdapterArtifactURI = adapterURI
				}
			} else {
				// State on failure becomes the "kept" string
				// so the UI shows what happened. Use a false
				// kept value alongside.
				fail := false
				p.Status.RecentRuns[i].Kept = &fail
			}
			p.Status.RecentRuns[i].CompletedAt = &now
			break
		}
		p.Status.ExperimentsRun++
		if kept {
			p.Status.ExperimentsKept++
			p.Status.BestEvalLoss = fmt.Sprintf("%.6f", evalLoss)
			p.Status.BestRunID = displayID
		}
		delete(p.Status.OpenRuns, displayID)
	}
	return completed, nil
}

// isRunTerminal reports whether a DSP run's state is one we
// shouldn't keep polling. Wraps dsp.IsTerminal but keeps the
// import surface tight from this file.
func isRunTerminal(state string) bool {
	switch state {
	case "SUCCEEDED", "FAILED", "SKIPPED", "CANCELED":
		return true
	}
	return false
}

// readEvalLossFromDSPRun fetches the eval_loss value for a
// completed run. v0.0.30 first cut: read the kfp metric artifact
// via DSP's artifact API. Falls back to log-scrape (the trainer's
// AUTORESEARCH_RESULT= line on the run's main pod) if the
// metrics artifact isn't yet ready or fails.
//
// Implementation note: DSP's artifact API surface is broader
// than what we need for this single use; for v0.0.30 we go via
// log-scrape (same path as v0.0.1) because it's already known
// to work and the operator doesn't need to depend on the
// artifact API's stability promise. Metrics still flow into the
// Experiments UI columns because kfp captures them server-side
// from the same source path — we just read from pod logs for
// the keep/revert decision. v0.0.31 can switch to the typed
// artifact when DSP's API contract is set in stone.
func (r *AutoResearchProjectReconciler) readEvalLossFromDSPRun(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject, run *dsp.Run) float64 {
	// kfp v2 labels every pipeline-step pod with
	// `pipeline/runid=<DSP run UUID>`. Verified empirically on
	// RHOAI 3.3.1 DSP — direct label match beats the previous
	// name-substring loop, which mis-fired when run IDs shared
	// prefixes. The trainer's container-impl pod is the only
	// one in the matched set that produces AUTORESEARCH_RESULT=
	// (driver/wait pods produce kfp launcher chatter only), so
	// we walk them and return the first parseable result.
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(p.Namespace),
		client.MatchingLabels{"pipeline/runid": run.RunID},
	); err != nil {
		return 0
	}
	for _, pod := range pods.Items {
		// Only the container-impl pod has our trainer; the
		// driver pods are kfp scaffolding.
		if !strings.Contains(pod.Name, "container-impl") {
			continue
		}
		evalLoss, err := r.parseTrainerResultByPodName(ctx, pod.Namespace, pod.Name)
		if err != nil {
			continue
		}
		return evalLoss
	}
	return 0
}

// readFullResultFromDSPRun is the v0.0.43 cousin of the eval-only
// reader. Same pod-discovery logic, but returns the structured
// autoresearchResult (with adapter_uri, model_version_id) so the
// drain can record them in status.recentRuns[].
func (r *AutoResearchProjectReconciler) readFullResultFromDSPRun(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject, run *dsp.Run) *autoresearchResult {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(p.Namespace),
		client.MatchingLabels{"pipeline/runid": run.RunID},
	); err != nil {
		return nil
	}
	for _, pod := range pods.Items {
		if !strings.Contains(pod.Name, "container-impl") {
			continue
		}
		res, err := r.parseTrainerResultFullByPodName(ctx, pod.Namespace, pod.Name)
		if err != nil {
			continue
		}
		return res
	}
	return nil
}

// parseTrainerResultByPodName reads pod logs by exact pod name
// (no label selector). Different from parseTrainerResult which
// finds the pod via the Job's experiment-label — kfp pods don't
// carry that label.
func (r *AutoResearchProjectReconciler) parseTrainerResultByPodName(ctx context.Context, namespace, podName string) (float64, error) {
	if r.RestConfig == nil {
		return 0, fmt.Errorf("RestConfig nil; cannot read pod logs")
	}
	cs, err := kubernetes.NewForConfig(r.RestConfig)
	if err != nil {
		return 0, err
	}
	// kfp v2 pipeline pods have three containers (main, wait,
	// kfp-launcher). GetLogs without an explicit container name
	// errors on multi-container pods, so the previous version
	// silently returned no result for DSP-pipeline runs. We
	// always want the trainer's stdout, which lives in `main`.
	logReq := cs.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: "main",
	})
	stream, err := logReq.Stream(ctx)
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	var lastResult string
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, "AUTORESEARCH_RESULT="); idx != -1 {
			lastResult = line[idx+len("AUTORESEARCH_RESULT="):]
		}
	}
	if lastResult == "" {
		return 0, fmt.Errorf("no AUTORESEARCH_RESULT= line")
	}
	res, err := parseAutoresearchResult(lastResult)
	if err != nil {
		return 0, err
	}
	return res.EvalLoss, nil
}

// autoresearchResult is the structured result the trainer's stdout
// AUTORESEARCH_RESULT={...} line is parsed into. v0.0.43 added
// adapter_uri + model_version_id for the Quay + Model Registry
// persistence path.
type autoresearchResult struct {
	Status         string  `json:"status"`
	EvalLoss       float64 `json:"eval_loss"`
	Error          string  `json:"error,omitempty"`
	AdapterURI     string  `json:"adapter_uri,omitempty"`
	ModelVersionID string  `json:"model_version_id,omitempty"`
}

// parseAutoresearchResult unmarshals + validates the trainer's
// AUTORESEARCH_RESULT= JSON payload. Returns an error for non-ok
// status so callers can treat trainer-emitted errors as failures.
func parseAutoresearchResult(payload string) (*autoresearchResult, error) {
	var res autoresearchResult
	if err := json.Unmarshal([]byte(payload), &res); err != nil {
		return nil, err
	}
	if res.Status != "ok" {
		return nil, fmt.Errorf("trainer error: %s", res.Error)
	}
	return &res, nil
}

// parseTrainerResultFullByPodName is the richer cousin of
// parseTrainerResultByPodName — returns the full autoresearchResult
// instead of just the eval_loss. Used by the drain path to record
// adapter_uri + model_version_id alongside eval_loss in status.
func (r *AutoResearchProjectReconciler) parseTrainerResultFullByPodName(ctx context.Context, namespace, podName string) (*autoresearchResult, error) {
	if r.RestConfig == nil {
		return nil, fmt.Errorf("RestConfig nil; cannot read pod logs")
	}
	cs, err := kubernetes.NewForConfig(r.RestConfig)
	if err != nil {
		return nil, err
	}
	logReq := cs.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: "main",
	})
	stream, err := logReq.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var lastResult string
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, "AUTORESEARCH_RESULT="); idx != -1 {
			lastResult = line[idx+len("AUTORESEARCH_RESULT="):]
		}
	}
	if lastResult == "" {
		return nil, fmt.Errorf("no AUTORESEARCH_RESULT= line")
	}
	return parseAutoresearchResult(lastResult)
}

// parseTrainerResult reads the trainer Job's pod logs and
// extracts the result payload from a line shaped:
//
//	AUTORESEARCH_RESULT={"status":"ok","eval_loss":1.234, ...}
//
// v0.0.1 design choice: log-line shape over artifact volume.
// Simpler than mounting a PVC or talking to MinIO; works
// without DSP set up. v0.0.2 will instead read metrics.json
// from a MinIO artifact via DSP.
//
// We scan from the END of the logs because the result line is
// emitted at the end of the trainer's run — most efficient and
// avoids re-parsing thousands of intermediate progress lines.
func (r *AutoResearchProjectReconciler) parseTrainerResult(ctx context.Context, job *batchv1.Job) (float64, error) {
	if r.RestConfig == nil {
		return 0, fmt.Errorf("RestConfig nil; cannot read pod logs")
	}
	cs, err := kubernetes.NewForConfig(r.RestConfig)
	if err != nil {
		return 0, fmt.Errorf("kubernetes client: %w", err)
	}

	// Find the trainer Pod for this Job.
	pods, err := cs.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", autoResearchExperimentLabel, job.Name),
	})
	if err != nil {
		return 0, fmt.Errorf("list job pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return 0, fmt.Errorf("no pods found for job %s", job.Name)
	}
	// Prefer the most recent (or only) pod.
	pod := pods.Items[0]
	for _, p := range pods.Items {
		if p.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = p
		}
	}

	logReq := cs.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: "trainer",
	})
	stream, err := logReq.Stream(ctx)
	if err != nil {
		return 0, fmt.Errorf("open pod log stream: %w", err)
	}
	defer stream.Close()

	// Read everything, scan for the last AUTORESEARCH_RESULT=
	// line. We deliberately don't tail — the trainer pod is
	// finished by the time we read, so the whole log is finite.
	var lastResult string
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024) // headroom for big lines
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, "AUTORESEARCH_RESULT="); idx != -1 {
			lastResult = line[idx+len("AUTORESEARCH_RESULT="):]
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return 0, fmt.Errorf("scan logs: %w", err)
	}
	if lastResult == "" {
		return 0, fmt.Errorf("no AUTORESEARCH_RESULT= line in trainer logs")
	}

	var payload struct {
		Status   string  `json:"status"`
		EvalLoss float64 `json:"eval_loss"`
		Error    string  `json:"error,omitempty"`
		Stage    string  `json:"stage,omitempty"`
	}
	if err := json.Unmarshal([]byte(lastResult), &payload); err != nil {
		return 0, fmt.Errorf("decode result JSON: %w (line=%q)", err, lastResult)
	}
	if payload.Status != "ok" {
		return 0, fmt.Errorf("trainer reported error (stage=%s): %s", payload.Stage, payload.Error)
	}
	return payload.EvalLoss, nil
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

// truncateMsg caps a condition Message at maxLen runes. Kept tight
// because Status updates with multi-KB error blobs (e.g. full DSP
// JSON error responses) eat etcd quota and crowd `oc describe`.
func truncateMsg(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// SetupWithManager wires the controller.
func (r *AutoResearchProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentofficev1alpha1.AutoResearchProject{}).
		Owns(&batchv1.Job{}).
		Named("autoresearchproject").
		Complete(r)
}
