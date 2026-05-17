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
// The embedded copy contains the literal placeholder
// '{{TRAINER_IMAGE}}' for the trainer container's image field.
// renderPipelineYAML() substitutes the resolved image before
// upload; the substituted bytes are the basis for the kfp
// content-hash version name, so a trainer-image bump produces a
// new DSP pipeline version with no operator rebuild needed.
//
//go:embed pipeline.yaml
var pipelineYAML []byte

// trainerImagePlaceholder is the substring in the embedded
// pipeline.yaml that resolveTrainerImage()'s output replaces.
const trainerImagePlaceholder = "{{TRAINER_IMAGE}}"

// trainerImageDefaultsConfigMap is the well-known ConfigMap name
// in the project's namespace whose `trainerImage` key supplies the
// default trainer image when the project's spec.trainer.image is
// empty. Lets cluster admins roll a trainer-image bump cluster-
// wide by editing one ConfigMap in GitOps — no operator rebuild,
// no per-project CR edits.
//
// Lookup precedence (resolveTrainerImage):
//
//   1. p.Spec.Trainer.Image                                — explicit per-project pin
//   2. ConfigMap autoresearch-defaults.trainerImage         — namespace default
//   3. defaultTrainerImage const in controller package      — last-resort fallback
const trainerImageDefaultsConfigMap = "autoresearch-defaults"

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

	// Render the embedded pipeline.yaml with the resolved trainer
	// image. The substituted bytes are what DSP sees AND what
	// FindOrCreatePipeline content-hashes to derive the version
	// name — so a trainer-image change produces a fresh pipeline
	// version automatically without any operator rebuild.
	renderedYAML, renderErr := r.renderPipelineYAML(ctx, p)
	if renderErr != nil {
		return "", "", "", fmt.Errorf("render pipeline: %w", renderErr)
	}

	pipe, versionID, err := dspClient.FindOrCreatePipeline(ctx, dspPipelineName, dspPipelineDescription, renderedYAML)
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

// deriveQuayPushDestination builds the full OCI URI the trainer
// pushes the adapter to for a given round. Honors spec.adapterStorage
// overrides; auto-derives the rest from project metadata.
//
// Default registry: the cluster's internal Quay route (the same one
// our operator + trainer images already live in — verified via the
// `quay-quay-quay-test.apps.salamander.aimlworkbench.com` hostname
// across all the .tekton pipeline output-image refs).
//
// Default repository: "<namespace>-<projectName>" — uses a dash
// (Quay disallows slashes inside a repo path component) so multi-
// project namespaces stay collision-free.
//
// Default tag: "round-<N>" — deterministic per round so re-runs
// overwrite the previous attempt's adapter for the same round.
//
// Returns "" when adapter persistence is intentionally disabled
// (no spec.adapterStorage + no default Quay reachable) so the
// trainer's "skip push when destination is empty" path kicks in.
func deriveQuayPushDestination(p *agentofficev1alpha1.AutoResearchProject, round int) string {
	defaultRegistry := "quay-quay-quay-test.apps.salamander.aimlworkbench.com"
	// IMPORTANT: Quay rejects pushes with no namespace component
	// (the path before the first /) with a 401 from the registry
	// API even when the robot's auth is valid. The trainer +
	// operator images all live under the "deanpeterson" Quay
	// namespace on this cluster, so we default adapter pushes
	// there too. Override per-project via spec.adapterStorage.quay.
	defaultNamespace := "deanpeterson"
	defaultRepo := fmt.Sprintf("%s/%s-%s", defaultNamespace, p.Namespace, p.Name)
	defaultTag := fmt.Sprintf("round-%d", round)

	registry := defaultRegistry
	repo := defaultRepo
	tag := defaultTag

	if p.Spec.AdapterStorage != nil && p.Spec.AdapterStorage.Quay != nil {
		q := p.Spec.AdapterStorage.Quay
		if q.Registry != "" {
			registry = q.Registry
		}
		if q.Repository != "" {
			repo = q.Repository
		}
		if q.TagPattern != "" {
			// Templated tag substitution: only "{{.Round}}" is
			// currently honored. Add more substitutions as
			// users ask for them.
			tag = strings.ReplaceAll(q.TagPattern, "{{.Round}}", fmt.Sprintf("%d", round))
		}
	}
	return fmt.Sprintf("%s/%s:%s", registry, repo, tag)
}

// deriveModelRegistryTarget returns the (URL, RegisteredModel name)
// pair the trainer should register its ModelVersion against.
// Auto-discovers the cluster's default ModelRegistry instance via
// the in-cluster service when no override is given.
//
// Returns ("", "") when no Model Registry is reachable / configured
// — trainer's "skip registration when URL empty" path then no-ops.
func deriveModelRegistryTarget(ctx context.Context, c client.Client, p *agentofficev1alpha1.AutoResearchProject) (url, name string) {
	defaultInstance := "default-modelregistry"
	defaultRegistryNS := "rhoai-model-registries"

	instance := defaultInstance
	regName := p.Name
	if p.Spec.AdapterStorage != nil && p.Spec.AdapterStorage.ModelRegistry != nil {
		mr := p.Spec.AdapterStorage.ModelRegistry
		if mr.RegistryInstance != "" {
			instance = mr.RegistryInstance
		}
		if mr.Name != "" {
			regName = mr.Name
		}
	}

	// Look up the Service the ModelRegistry instance exposes for
	// REST. RHOAI's operator names it "<instance>-rest" in the
	// rhoai-model-registries namespace.
	var svc corev1.Service
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: defaultRegistryNS,
		Name:      instance + "-rest",
	}, &svc); err != nil {
		// Fallback: no service → skip registration this round.
		// Don't fail the whole training — adapter still pushes
		// to Quay, just no MR record. The next reconcile retries.
		return "", regName
	}
	return fmt.Sprintf("https://%s.%s.svc.cluster.local:8443", svc.Name, svc.Namespace), regName
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

	// Adapter storage destinations — auto-derived from project
	// metadata when spec.adapterStorage is omitted or has empty
	// fields. Workshop attendees don't have to specify any of
	// this; team leads can override per project.
	quayDest := deriveQuayPushDestination(p, round)
	mrURL, mrName := deriveModelRegistryTarget(ctx, r.Client, p)

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
		// New in v0.0.43: adapter persistence + registration.
		"adapter_push_destination": quayDest,
		"model_registry_url":       mrURL,
		"model_registry_name":      mrName,
	}

	run, err := dspClient.CreateRun(ctx, runID, experimentID, pipelineID, params)
	if err != nil {
		return nil, fmt.Errorf("create DSP run: %w", err)
	}
	return run, nil
}

// renderPipelineYAML substitutes {{TRAINER_IMAGE}} in the embedded
// pipeline.yaml with the resolved image for this project. Returns
// an error only if the placeholder is missing from the embedded
// YAML — that's a developer error (someone edited pipeline.yaml
// and accidentally inlined the image again), not a runtime
// condition the operator should silently tolerate.
//
// v0.0.50 fix: switched from strings.Replace(..., 1) to ReplaceAll
// because v0.0.49 mentioned {{TRAINER_IMAGE}} TWICE in pipeline.yaml
// (once in a documentation comment, once on the actual image: line).
// The 1-count replace hit the comment first, leaving the image: line
// with the literal placeholder string. DSP accepted the upload
// (kfp doesn't validate image format), but every trainer pod
// went into ImagePullBackOff trying to pull image "{{TRAINER_IMAGE}}".
// ReplaceAll handles both occurrences correctly — the rendered
// comment ends up with the resolved image URL too, which is
// harmless (comments are stripped by kfp's parser).
func (r *AutoResearchProjectReconciler) renderPipelineYAML(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject) ([]byte, error) {
	if !strings.Contains(string(pipelineYAML), trainerImagePlaceholder) {
		return nil, fmt.Errorf("embedded pipeline.yaml missing %s placeholder — "+
			"someone reverted the templating in pipeline.yaml", trainerImagePlaceholder)
	}
	image := r.resolveTrainerImage(ctx, p)
	rendered := strings.ReplaceAll(string(pipelineYAML), trainerImagePlaceholder, image)
	// Belt and braces: assert no placeholder survived. If we ever
	// switch to a more complex template format and miss a token
	// the operator should refuse to upload rather than ship a
	// pipeline that ImagePullBackOffs every pod.
	if strings.Contains(rendered, trainerImagePlaceholder) {
		return nil, fmt.Errorf("renderPipelineYAML: %s placeholder survived substitution — "+
			"this means the template has a form ReplaceAll can't handle; refusing to upload",
			trainerImagePlaceholder)
	}
	return []byte(rendered), nil
}

// resolveTrainerImage returns the trainer container image the
// operator should plumb into pipeline.yaml for this project,
// honoring the precedence documented at trainerImageDefaultsConfigMap.
//
// ConfigMap lookup is best-effort — if it doesn't exist or the
// key is missing we silently fall through to the compiled-in
// default. This keeps a fresh cluster install working without any
// extra bootstrap; admins opt into ConfigMap-driven defaults by
// creating the resource.
func (r *AutoResearchProjectReconciler) resolveTrainerImage(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject) string {
	if p.Spec.Trainer.Image != "" {
		return p.Spec.Trainer.Image
	}
	var cm corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{
		Namespace: p.Namespace,
		Name:      trainerImageDefaultsConfigMap,
	}, &cm)
	if err == nil {
		if v, ok := cm.Data["trainerImage"]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return defaultTrainerImage
}

// verifyAdapterArtifact does an OCI manifest HEAD against the given
// adapter URI to confirm the trainer's reported push actually
// landed in the registry. Used by the post-round drain to flip
// silent persistence skips into loud, status-visible failures.
//
// Returns:
//   - status="OK"      → 200; adapter is pullable
//   - status="Missing" → 404; trainer reported success but registry has no manifest
//   - status="Unknown" → network/auth error; treat as inconclusive, don't punish round
//
// Auth: reads Secret/quay-push-secret in the project's namespace
// (the same one the trainer uses for the push side), decodes the
// `.dockerconfigjson` value, finds the matching registry's
// "auth" field, and uses it as the Authorization header. If the
// Secret is missing or auth can't be extracted we still send the
// HEAD unauthenticated — public manifests work, private ones come
// back 401 and we surface that as Unknown rather than failure.
func (r *AutoResearchProjectReconciler) verifyAdapterArtifact(ctx context.Context, p *agentofficev1alpha1.AutoResearchProject, uri string) (status, detail string) {
	registry, repo, tag, perr := splitOCIURI(uri)
	if perr != nil {
		return "Unknown", fmt.Sprintf("malformed adapter URI %q: %v", uri, perr)
	}
	authHeader := r.readQuayBasicAuth(ctx, p.Namespace, registry)
	headURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, repo, tag)

	req, _ := newHTTPRequestWithContext(ctx, "HEAD", headURL, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	// Accept the modern OCI + Docker v2 manifest media types so the
	// registry returns a 200 instead of an old "manifest unknown"
	// fallback when the content is OCI-specific (our adapter
	// artifact is application/vnd.peft.lora.v1).
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, "+
		"application/vnd.oci.image.index.v1+json, "+
		"application/vnd.docker.distribution.manifest.v2+json")

	resp, err := insecureHTTPClient().Do(req)
	if err != nil {
		return "Unknown", fmt.Sprintf("HEAD %s failed: %v", headURL, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		return "OK", fmt.Sprintf("manifest present at %s", uri)
	case 404:
		return "Missing", fmt.Sprintf("registry returned 404 for %s — trainer reported "+
			"push success but the manifest isn't there", uri)
	case 401, 403:
		return "Unknown", fmt.Sprintf("registry returned %d for %s — operator can't see this "+
			"repo (auth issue); persistence may still be working", resp.StatusCode, uri)
	default:
		return "Unknown", fmt.Sprintf("registry returned HTTP %d for %s", resp.StatusCode, uri)
	}
}

// splitOCIURI parses "registry/repo:tag" into its three parts.
// Repo may itself contain slashes; tag is the substring after the
// LAST colon (digest references with @sha256:... are not supported
// since the trainer always pushes by tag).
func splitOCIURI(uri string) (registry, repo, tag string, err error) {
	colon := strings.LastIndex(uri, ":")
	if colon < 0 {
		return "", "", "", fmt.Errorf("no :tag suffix")
	}
	tag = uri[colon+1:]
	body := uri[:colon]
	slash := strings.Index(body, "/")
	if slash < 0 {
		return "", "", "", fmt.Errorf("no registry/repo separator")
	}
	registry = body[:slash]
	repo = body[slash+1:]
	if registry == "" || repo == "" || tag == "" {
		return "", "", "", fmt.Errorf("empty registry, repo, or tag")
	}
	return registry, repo, tag, nil
}

// readQuayBasicAuth fetches Secret/quay-push-secret in the given
// namespace and returns a "Basic <base64>" Authorization header
// for the given registry hostname, or "" if the secret is missing
// or doesn't have an entry for that registry.
//
// Best-effort by design — verifyAdapterArtifact falls back to an
// unauthenticated HEAD when this returns "".
func (r *AutoResearchProjectReconciler) readQuayBasicAuth(ctx context.Context, namespace, registry string) string {
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      "quay-push-secret",
	}, &sec); err != nil {
		return ""
	}
	raw, ok := sec.Data[".dockerconfigjson"]
	if !ok {
		return ""
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	// Docker config keys can be exact hostname OR a URL like
	// "https://<host>/v1/"; check both. Quay typically uses the
	// hostname directly.
	for k, v := range cfg.Auths {
		if v.Auth == "" {
			continue
		}
		if k == registry || strings.Contains(k, registry) {
			return "Basic " + v.Auth
		}
	}
	return ""
}

