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
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
// Caches result in ProjectStatus so we don't re-resolve every
// reconcile pass.
func (r *AutoResearchProjectReconciler) ensureDSPRegistration(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject) (pipelineID, experimentID string, err error) {
	dspClient, err := dspClientFor(ctx, r.Client, p.Namespace)
	if err != nil {
		return "", "", err
	}

	pipe, err := dspClient.FindOrCreatePipeline(ctx, dspPipelineName, dspPipelineDescription, pipelineYAML)
	if err != nil {
		return "", "", fmt.Errorf("ensure pipeline: %w", err)
	}

	experimentName := p.Spec.ExperimentName
	if experimentName == "" {
		experimentName = p.Name
	}
	exp, err := dspClient.FindOrCreateExperiment(ctx, experimentName,
		fmt.Sprintf("AutoResearchProject %s/%s — autonomous QLoRA loop.", p.Namespace, p.Name))
	if err != nil {
		return "", "", fmt.Errorf("ensure experiment: %w", err)
	}

	return pipe.PipelineID, exp.ExperimentID, nil
}

// submitDSPRun creates a kfp Run for one experiment cycle. The
// operator calls this instead of submitTrainerJob when DSP
// integration is active (v0.0.2 default).
func (r *AutoResearchProjectReconciler) submitDSPRun(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject, runID string, round int, cfg QLoRAConfig) (*dsp.Run, error) {
	dspClient, err := dspClientFor(ctx, r.Client, p.Namespace)
	if err != nil {
		return nil, err
	}
	pipelineID, experimentID, err := r.ensureDSPRegistration(ctx, p)
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
