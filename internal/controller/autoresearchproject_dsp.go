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
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
	"github.com/enterprisewebservice/agent-office-operator/internal/dsp"
)

// pipelineYAML is the compiled kfp v2 IR for the
// autoresearch-qlora-train-eval pipeline. Embedded at build
// time from scripts/autoresearch-pipeline/pipeline.yaml so the
// operator can register the pipeline with DSP at startup
// without depending on Python/kfp at runtime.
//
//go:embed pipeline.yaml
var pipelineYAML []byte

// dspPipelineName + dspPipelineDescription are constants
// referenced by both the upload-on-startup and the per-run
// submission paths.
const (
	dspPipelineName        = "autoresearch-qlora-train-eval"
	dspPipelineDescription = "Karpathy-style AutoResearch QLoRA fine-tune + eval cycle. " +
		"Operator submits one Run per AutoResearchProject cycle; eval_loss feeds keep/revert."
)

// dspClientCache memoizes a DSP Client per (namespace, dspaName)
// so we don't rebuild the HTTP client + auth on every reconcile.
// Token is read from the operator pod's mounted SA token; if
// the token file changes (rotation), we pick it up on next
// reconcile because k8s remounts in place.
type dspClientCache struct {
	mu      sync.Mutex
	clients map[string]*dsp.Client
}

var dspCache = &dspClientCache{clients: map[string]*dsp.Client{}}

// dspClientFor returns a Client targeting the DSPA in the given
// namespace. Looks up the DSPA's API server Service to derive
// the in-cluster URL, reads the SA token from the well-known
// path. The result is cached per namespace.
//
// In-cluster URL pattern from RHOAI:
//   ds-pipeline-<dspa>.<namespace>.svc.cluster.local:8443
//
// where <dspa> is the DataSciencePipelinesApplication's name —
// we hard-code "dspa" because that's the conventional one-per-
// namespace name the dashboard creates. If a project uses a
// non-default name, set spec.pipelineRef.dspaName (future
// CRD field; v0.0.2 assumes default).
func dspClientFor(ctx context.Context, c client.Client, namespace string) (*dsp.Client, error) {
	const dspaName = "dspa"
	key := namespace + "/" + dspaName

	dspCache.mu.Lock()
	defer dspCache.mu.Unlock()
	if existing, ok := dspCache.clients[key]; ok && existing.Token != "" {
		return existing, nil
	}

	// Verify the API server Service exists — early failure with
	// a clear message beats a 30s timeout on the first run.
	var svc corev1.Service
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      "ds-pipeline-" + dspaName,
	}, &svc); err != nil {
		return nil, fmt.Errorf("DSP API server Service ds-pipeline-%s not found in %s "+
			"(is the DataSciencePipelinesApplication deployed?): %w",
			dspaName, namespace, err)
	}

	baseURL := fmt.Sprintf("https://ds-pipeline-%s.%s.svc.cluster.local:8443",
		dspaName, namespace)

	// SA token — read from the standard projected path the kubelet
	// mounts into every pod by default. The operator's SA needs
	// permission against the DSPA; granted via the
	// dspa-pipeline-runner RoleBinding the DSPA operator creates.
	tokenBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return nil, fmt.Errorf("read SA token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))

	client := dsp.NewClient(baseURL, token)
	// Trust the cluster's internal CA — Service URLs use the
	// kube-apiserver-signed cert. RHOAI's DSP API server is
	// served by a TLS-terminating reverse proxy in the same pod.
	// We'd ideally configure the system trust store; for v0.0.30
	// disable verify on the in-cluster Service URL since these
	// connections never leave the cluster network and the only
	// alternative endpoint (the Route) would require ingress CA
	// loading we haven't yet wired.
	transport := defaultInsecureTransport()
	client.HTTPClient.Transport = transport

	dspCache.clients[key] = client
	return client, nil
}

// ensureDSPRegistration uploads the pipeline (idempotent) and
// finds/creates the per-project Experiment. Operator calls this
// at the top of each reconcile so a freshly-applied project
// gets its pipeline + experiment registered on first cycle.
//
// Also returns the pipeline version ID corresponding to the
// embedded pipeline.yaml's content hash — callers pass that
// to cullStaleOpenRuns so any in-flight runs from a previous
// operator version (with a different embedded YAML, e.g. an
// older trainer image tag) get terminated immediately rather
// than wasting GPU time on a doomed config.
func (r *AutoResearchProjectReconciler) ensureDSPRegistration(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject) (pipelineID, currentVersionID, experimentID string, err error) {
	dspClient, err := dspClientFor(ctx, r.Client, p.Namespace)
	if err != nil {
		return "", "", "", err
	}

	pipe, versionID, err := dspClient.FindOrCreatePipeline(ctx, dspPipelineName, dspPipelineDescription, pipelineYAML)
	if err != nil {
		return "", "", "", fmt.Errorf("ensure pipeline: %w", err)
	}

	experimentName := p.Spec.ExperimentName
	if experimentName == "" {
		experimentName = p.Name
	}
	exp, err := dspClient.FindOrCreateExperiment(ctx, experimentName,
		fmt.Sprintf("AutoResearchProject %s/%s — autonomous QLoRA loop.", p.Namespace, p.Name))
	if err != nil {
		return "", "", "", fmt.Errorf("ensure experiment: %w", err)
	}

	return pipe.PipelineID, versionID, exp.ExperimentID, nil
}

// cullStaleOpenRuns terminates and removes any in-flight DSP
// run whose pipeline_version_id no longer matches the operator's
// current embedded pipeline.yaml (i.e. the user has deployed a
// new operator version whose pipeline.yaml differs — e.g.
// trainer image tag bump). Without this, stale runs keep
// running on the old trainer config and waste GPU time on a
// doomed cycle before failing naturally.
//
// Termination sequence (DSP doesn't expose a usable :terminate
// REST endpoint on RHOAI 3.3.1 — both colon and slash forms
// return 501/404 — so we do this in two steps):
//
//  1. Delete the underlying Argo Workflow, label-selected by
//     pipeline/runid=<dspRunID>. The Workflow's controller then
//     terminates the running pods immediately.
//  2. Best-effort delete the DSP run record. (If this fails,
//     the run record stays around but is harmless — the next
//     drain pass will see it terminal.)
//  3. Remove the entry from status.OpenRuns and append a
//     "Superseded" marker to status.RecentRuns so the wiki +
//     experiments UI show the lineage.
//
// Returns the number culled. Caller uses this >0 as a signal to
// bypass the cadence gate and immediately submit a fresh cycle
// on the current pipeline version.
func (r *AutoResearchProjectReconciler) cullStaleOpenRuns(
	ctx context.Context,
	p *agentofficev1alpha1.AutoResearchProject,
	currentVersionID string,
) (int, error) {
	log := logf.FromContext(ctx)
	if currentVersionID == "" || len(p.Status.OpenRuns) == 0 {
		return 0, nil
	}
	dspClient, err := dspClientFor(ctx, r.Client, p.Namespace)
	if err != nil {
		return 0, err
	}
	culled := 0
	for displayID, dspRunID := range p.Status.OpenRuns {
		run, err := dspClient.GetRun(ctx, dspRunID)
		if err != nil {
			continue
		}
		runVersionID := run.PipelineVersionReference.PipelineVersionID
		if runVersionID == "" || runVersionID == currentVersionID {
			continue
		}
		// Stale — cull.
		log.Info("culling stale-version run",
			"runID", displayID, "dspRunID", dspRunID,
			"runVersion", runVersionID, "currentVersion", currentVersionID)

		// 1. Delete the Argo workflow via label selector. The
		//    kfp v2 runtime labels each step pod with
		//    `pipeline/runid=<DSP run UUID>`, and the parent
		//    Workflow inherits this label.
		wfs := &unstructured.UnstructuredList{}
		wfs.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "argoproj.io", Version: "v1alpha1", Kind: "WorkflowList",
		})
		_ = r.List(ctx, wfs,
			client.InNamespace(p.Namespace),
			client.MatchingLabels{"pipeline/runid": dspRunID},
		)
		for i := range wfs.Items {
			_ = r.Delete(ctx, &wfs.Items[i])
		}

		// 2. Best-effort delete the DSP run record.
		_ = dspClient.DeleteRun(ctx, dspRunID)

		// 3. Status bookkeeping: remove from OpenRuns, append
		//    a Superseded marker to RecentRuns. We extract the
		//    round number from the runID (deterministic format
		//    "<project>-round-<N>") rather than reaching into
		//    DSP's runtime config — the latter shape isn't part
		//    of our minimal dsp.Run type.
		delete(p.Status.OpenRuns, displayID)
		now := metav1.Now()
		fail := false
		round := extractRoundFromRunID(displayID, p.Name)
		var startedPtr *metav1.Time
		if t, perr := time.Parse(time.RFC3339, run.CreatedAt); perr == nil {
			started := metav1.NewTime(t)
			startedPtr = &started
		}
		p.Status.RecentRuns = append(p.Status.RecentRuns, agentofficev1alpha1.AutoResearchExperimentRecord{
			Round:        round,
			RunID:        displayID,
			Experimenter: p.Spec.Experimenter.Workstation,
			ProposalPath: fmt.Sprintf("proposals/round-%d.yaml", round),
			StartedAt:    startedPtr,
			CompletedAt:  &now,
			Kept:         &fail,
			EvalLoss:     "superseded",
		})
		culled++
	}
	return culled, nil
}

// extractRoundFromRunID parses N out of "<project>-round-<N>".
// Falls back to 0 if the runID doesn't match the format.
func extractRoundFromRunID(runID, projectName string) int32 {
	prefix := projectName + "-round-"
	if !strings.HasPrefix(runID, prefix) {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(runID, prefix))
	if err != nil {
		return 0
	}
	return int32(n)
}

// submitDSPRun creates a kfp Run for one experiment cycle. The
// operator calls this instead of submitTrainerJob when DSP
// integration is active (v0.0.2 default).
func (r *AutoResearchProjectReconciler) submitDSPRun(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject, runID string, round int, cfg QLoRAConfig) (*dsp.Run, error) {
	dspClient, err := dspClientFor(ctx, r.Client, p.Namespace)
	if err != nil {
		return nil, err
	}
	pipelineID, _, experimentID, err := r.ensureDSPRegistration(ctx, p)
	if err != nil {
		return nil, err
	}

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}

	sampleCount := int32(2000)
	if p.Spec.TrainingData.SampleCount != nil {
		sampleCount = *p.Spec.TrainingData.SampleCount
	}
	// ValidationMode clamps the dataset down to a smoke-test
	// slice so dataset load + tokenization stay fast. Paired
	// with validationQLoRAConfig (10 steps / batch 1) the
	// whole run including model download lands in 3-5 min.
	if p.Spec.ValidationMode && sampleCount > 200 {
		sampleCount = 200
	}

	params := map[string]any{
		"base_model":              p.Spec.BaseModel.HuggingfaceID,
		"base_model_revision":     defaultIfEmpty(p.Spec.BaseModel.Revision, "main"),
		"training_data":           p.Spec.TrainingData.HuggingfaceDataset,
		"training_split":          defaultIfEmpty(p.Spec.TrainingData.Split, "train"),
		"training_sample_count":   sampleCount,
		"qlora_config_json":       string(cfgJSON),
		"eval_metric":             defaultIfEmpty(p.Spec.Eval.Metric, "eval_loss"),
		"eval_direction":          defaultIfEmpty(p.Spec.Eval.Direction, "minimize"),
		"autoresearch_project":    p.Name,
		"autoresearch_round":      round,
		"autoresearch_run_id":     runID,
	}

	run, err := dspClient.CreateRun(ctx, runID, experimentID, pipelineID, params)
	if err != nil {
		return nil, fmt.Errorf("create DSP run: %w", err)
	}
	return run, nil
}
