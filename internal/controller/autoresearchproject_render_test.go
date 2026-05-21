/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// Unit tests for the renderPipelineYAML pathway.
//
// WHY THIS FILE EXISTS: v0.0.49 shipped a renderer that used
// strings.Replace(..., 1) when the embedded pipeline.yaml had
// {{TRAINER_IMAGE}} in TWO places (documentation comment +
// the actual image: line). The 1-count replace substituted the
// comment instead of the image, DSP accepted the upload (kfp
// doesn't validate image format), and every trainer pod went
// into ImagePullBackOff on the literal placeholder. Cost: one
// full operator rebuild cycle to ship v0.0.50.
//
// Every assert in this file represents a class of bug that
// previously required a Konflux build to surface. Add to this
// file every time a new render bug bites; never let it shrink.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// newReconcilerForRenderTest builds a minimal AutoResearchProjectReconciler
// against a fake client. Render-only tests don't touch the API beyond
// the optional ConfigMap lookup in resolveTrainerImage(), so a fake
// k8s client is sufficient.
func newReconcilerForRenderTest(t *testing.T, objs ...runtime.Object) *AutoResearchProjectReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := agentofficev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agentoffice scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &AutoResearchProjectReconciler{Client: c, Scheme: scheme}
}

func newProject(image string) *agentofficev1alpha1.AutoResearchProject {
	return &agentofficev1alpha1.AutoResearchProject{
		ObjectMeta: metav1.ObjectMeta{Name: "test-project", Namespace: "test-ns"},
		Spec: agentofficev1alpha1.AutoResearchProjectSpec{
			Trainer: agentofficev1alpha1.AutoResearchTrainer{Image: image},
		},
	}
}

// TestRender_NoPlaceholderSurvives is the regression test for the
// v0.0.49 bug. If renderPipelineYAML ever leaves a `{{TRAINER_IMAGE}}`
// literal in its output, this test fails BEFORE go test exits ok and
// thus before the operator binary ships.
func TestRender_NoPlaceholderSurvives(t *testing.T) {
	r := newReconcilerForRenderTest(t)
	p := newProject("quay.example.com/team/trainer:v1.2.3")

	out, err := r.renderPipelineYAML(context.Background(), p)
	if err != nil {
		t.Fatalf("renderPipelineYAML: %v", err)
	}
	if strings.Contains(string(out), "{{TRAINER_IMAGE}}") {
		t.Fatalf("rendered pipeline.yaml still contains {{TRAINER_IMAGE}} — "+
			"this is exactly the v0.0.49 bug (1-replace hit the comment "+
			"instead of the image: line). Output:\n%s", string(out))
	}
}

// TestRender_ImageLineHasResolvedImage verifies the image actually
// landed where DSP will read it from. A pipeline can be syntactically
// valid YAML and still wrong if the substitution missed the image:
// line specifically.
func TestRender_ImageLineHasResolvedImage(t *testing.T) {
	const sentinel = "quay.example.com/team/trainer:v9.9.9-sentinel"
	r := newReconcilerForRenderTest(t)
	p := newProject(sentinel)

	out, err := r.renderPipelineYAML(context.Background(), p)
	if err != nil {
		t.Fatalf("renderPipelineYAML: %v", err)
	}
	// The kfp v2 IR puts the trainer image at
	// deploymentSpec.executors.exec-train-qlora.container.image. The
	// renderer is a textual replace, so check the rendered line
	// shape directly.
	wantLine := "        image: '" + sentinel + "'"
	if !strings.Contains(string(out), wantLine) {
		t.Fatalf("expected rendered pipeline.yaml to contain %q, did not.\n"+
			"This means the substitution targeted the wrong line\n"+
			"(probably a comment or annotation). Full output:\n%s",
			wantLine, string(out))
	}
}

// TestRender_ParsesAsYAML catches any way the substitution could
// produce malformed YAML — e.g. the resolved image contains a single
// quote that breaks the surrounding YAML quoting. Cheap insurance.
func TestRender_ParsesAsYAML(t *testing.T) {
	r := newReconcilerForRenderTest(t)
	p := newProject("quay.example.com/team/trainer:v1.0.0")

	out, err := r.renderPipelineYAML(context.Background(), p)
	if err != nil {
		t.Fatalf("renderPipelineYAML: %v", err)
	}
	var ir map[string]any
	if err := yaml.Unmarshal(out, &ir); err != nil {
		t.Fatalf("rendered pipeline.yaml is not valid YAML: %v\n%s", err, string(out))
	}
	if ir["pipelineInfo"] == nil {
		t.Fatalf("rendered pipeline.yaml parsed but has no pipelineInfo — "+
			"substitution may have corrupted structure. Output:\n%s", string(out))
	}
}

// TestParseBearerChallenge verifies the WWW-Authenticate parser
// handles the exact shape Quay returns on its v2 endpoints.
func TestParseBearerChallenge(t *testing.T) {
	cases := []struct {
		name, header, wantRealm, wantService, wantScope string
	}{
		{
			name:        "quay v2 manifests pull challenge",
			header:      `Bearer realm="https://quay.example.com/v2/auth",service="quay.example.com",scope="repository:dean/foo:pull"`,
			wantRealm:   "https://quay.example.com/v2/auth",
			wantService: "quay.example.com",
			wantScope:   "repository:dean/foo:pull",
		},
		{
			name:        "no scope (e.g. /v2/ root challenge)",
			header:      `Bearer realm="https://quay.example.com/v2/auth",service="quay.example.com"`,
			wantRealm:   "https://quay.example.com/v2/auth",
			wantService: "quay.example.com",
			wantScope:   "",
		},
		{
			name:   "Basic challenge (not bearer) returns empty",
			header: `Basic realm="my-registry"`,
		},
		{
			name:   "garbage returns empty",
			header: "BANANA",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, s, sc := parseBearerChallenge(tc.header)
			if r != tc.wantRealm || s != tc.wantService || sc != tc.wantScope {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)",
					r, s, sc, tc.wantRealm, tc.wantService, tc.wantScope)
			}
		})
	}
}

// TestVerifyAdapterArtifact_BearerFlow simulates Quay's
// challenge-then-token flow with an httptest.Server and asserts
// verifyAdapterArtifact actually flips Unknown→OK when the bearer
// exchange succeeds. This is the regression test for v0.0.50's
// "AdapterPushOK=Unknown forever even though the manifest is right
// there" symptom.
func TestVerifyAdapterArtifact_BearerFlow(t *testing.T) {
	const (
		repo        = "dean/agent-office-adapter"
		tag         = "round-1"
		bearerToken = "TEST-BEARER-TOKEN"
		basicUser   = "robot"
		basicPass   = "secret"
	)
	wantBasicHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(basicUser+":"+basicPass))

	// Fake registry: first HEAD comes back 401 + WWW-Authenticate;
	// /v2/auth checks Basic and returns a token; subsequent HEAD with
	// matching bearer returns 200.
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/v2/auth"):
			if req.Header.Get("Authorization") != wantBasicHeader {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"token":%q}`, bearerToken)
		case req.Method == "HEAD" && strings.Contains(req.URL.Path, "/v2/"+repo+"/manifests/"):
			if req.Header.Get("Authorization") == "Bearer "+bearerToken {
				w.WriteHeader(200)
				return
			}
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/v2/auth",service="%s",scope="repository:%s:pull"`,
					srv.URL, strings.TrimPrefix(srv.URL, "https://"), repo))
			w.WriteHeader(401)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	hostport := strings.TrimPrefix(srv.URL, "https://")

	// Build a quay-push-secret dockerconfigjson the operator's
	// readQuayBasicAuth can decode for this registry host.
	auth := base64.StdEncoding.EncodeToString([]byte(basicUser + ":" + basicPass))
	cfg := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, hostport, auth)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "quay-push-secret", Namespace: "ns"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{".dockerconfigjson": []byte(cfg)},
	}
	r := newReconcilerForRenderTest(t, sec)
	p := &agentofficev1alpha1.AutoResearchProject{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
	}

	uri := fmt.Sprintf("%s/%s:%s", hostport, repo, tag)
	status, detail := r.verifyAdapterArtifact(context.Background(), p, uri)
	if status != "OK" {
		t.Fatalf("expected status=OK after bearer flow, got %s (%s)", status, detail)
	}
}

// TestVerifyAdapterArtifact_BearerFlow_404 verifies a 404 from the
// registry (even after a successful bearer exchange) is faithfully
// reported as Missing — i.e., the trainer reported it pushed but the
// manifest isn't actually there.
func TestVerifyAdapterArtifact_BearerFlow_404(t *testing.T) {
	const bearerToken = "T"
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/v2/auth"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"token":%q}`, bearerToken)
		case req.Method == "HEAD" && strings.Contains(req.URL.Path, "/manifests/"):
			if req.Header.Get("Authorization") == "Bearer "+bearerToken {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/v2/auth",service="x",scope="repository:y:pull"`, srv.URL))
			w.WriteHeader(401)
		}
	}))
	defer srv.Close()
	hostport := strings.TrimPrefix(srv.URL, "https://")
	cfg := fmt.Sprintf(`{"auths":{%q:{"auth":"%s"}}}`,
		hostport, base64.StdEncoding.EncodeToString([]byte("u:p")))
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "quay-push-secret", Namespace: "ns"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{".dockerconfigjson": []byte(cfg)},
	}
	r := newReconcilerForRenderTest(t, sec)
	p := &agentofficev1alpha1.AutoResearchProject{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
	}
	status, _ := r.verifyAdapterArtifact(context.Background(), p, hostport+"/y:z")
	if status != "Missing" {
		t.Errorf("expected Missing on 404, got %s", status)
	}
}

// ---- v0.0.52: agent-integration tests ----

// TestParseAgentProposal verifies the various reply shapes we expect
// from the experimenter agent — straight JSON, JSON inside a markdown
// fence, JSON embedded in prose — all parse to a usable QLoRAConfig.
// Crucially the round number is always overridden by the operator so
// a hallucinating agent can't desync the loop counter.
// TestParseAgentProposal + TestParseAgentProposal_RejectsGarbage
// + TestExtractJSONObject were removed in v1.1.0 along with the
// parseAgentProposal / stripCodeFences / extractJSONObject
// functions they tested. The deterministic searcher
// (internal/search) owns proposal generation now; there is no
// LLM-reply parser on the proposal critical path. The strategist
// runner has its own envelope-extraction (extractStrategistReply
// in autoresearchproject_strategist.go) — see that file's own
// tests if more parser coverage is needed in the future.

// TestValidateQLoRAConfig ensures the operator clamps out-of-range
// proposals. An agent that hallucinates a 0 learning_rate or a
// 100000-step run shouldn't get to burn a real training cycle.
// Still used: validateQLoRAConfig is now called from the searcher's
// trial-to-config mapping path (round trip from goptuna into a
// QLoRAConfig validates the same bounds before the trainer sees
// the config).
func TestValidateQLoRAConfig(t *testing.T) {
	good := QLoRAConfig{LoraRank: 8, LoraAlpha: 16, LearningRate: 2e-4, NumTrainingSteps: 200, TargetModules: []string{"q_proj"}}
	if err := validateQLoRAConfig(&good); err != nil {
		t.Errorf("good config rejected: %v", err)
	}
	bads := []QLoRAConfig{
		{LoraRank: 0, LoraAlpha: 16, LearningRate: 2e-4, NumTrainingSteps: 200, TargetModules: []string{"q_proj"}},
		{LoraRank: 8, LoraAlpha: 16, LearningRate: 0, NumTrainingSteps: 200, TargetModules: []string{"q_proj"}},
		{LoraRank: 8, LoraAlpha: 16, LearningRate: 2e-4, NumTrainingSteps: 99999, TargetModules: []string{"q_proj"}},
		{LoraRank: 8, LoraAlpha: 16, LearningRate: 2e-4, NumTrainingSteps: 200, TargetModules: nil},
	}
	for i, b := range bads {
		if err := validateQLoRAConfig(&b); err == nil {
			t.Errorf("case %d: bad config accepted: %+v", i, b)
		}
	}
}


// ---- end of v0.0.52 agent-integration tests ----

// ---- v0.0.53: KnowledgeBase autoresearch-bootstrap tests ----

// TestRenderAutoresearchGoals verifies the GOALS.md output uses the
// project's actual base model + dataset + metric, with sensible
// fallbacks when fields are empty.
func TestRenderAutoresearchGoals(t *testing.T) {
	p := &agentofficev1alpha1.AutoResearchProject{
		Spec: agentofficev1alpha1.AutoResearchProjectSpec{
			BaseModel: agentofficev1alpha1.AutoResearchBaseModel{
				HuggingfaceID: "ibm-granite/granite-4.1-8b",
			},
			TrainingData: agentofficev1alpha1.AutoResearchTrainingData{
				HuggingfaceDataset: "ise-uiuc/Magicoder-OSS-Instruct-75K",
			},
			Eval: agentofficev1alpha1.AutoResearchEval{
				Metric:    "eval_loss",
				Direction: "minimize",
			},
		},
	}
	out := renderAutoresearchGoals("autoresearch-granite-8b", p)
	for _, want := range []string{
		"# Goals",
		"autoresearch-granite-8b",
		"ibm-granite/granite-4.1-8b",
		"ise-uiuc/Magicoder-OSS-Instruct-75K",
		"eval_loss",
		"minimize",
		"mermaid",       // the loop diagram
		"Success criteria",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("GOALS.md missing %q", want)
		}
	}

	// Nil/empty project still produces something usable (the KB
	// can exist before the project is created).
	empty := renderAutoresearchGoals("foo", nil)
	if !strings.Contains(empty, "(unset)") {
		t.Errorf("empty project should produce (unset) placeholders, got: %s", empty)
	}
}

// TestRenderAutoresearchDashboard verifies the dashboard scaffold
// + spec values get plumbed through.
func TestRenderAutoresearchDashboard(t *testing.T) {
	p := &agentofficev1alpha1.AutoResearchProject{
		Spec: agentofficev1alpha1.AutoResearchProjectSpec{
			LoopConfig: agentofficev1alpha1.AutoResearchLoopConfig{
				CadenceMinutes:      45,
				MaxTotalExperiments: 200,
			},
		},
	}
	out := renderAutoresearchDashboard("autoresearch-granite-8b", p)
	for _, want := range []string{
		"# Dashboard",
		"autoresearch-granite-8b",
		"45 minutes",
		"200",
		"| Round | eval_loss",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("DASHBOARD.md missing %q", want)
		}
	}
}

// TestRenderAutoresearchAgentPage verifies the agent-page render
// includes the system prompt, tools, and the openclaw invocation
// command the operator would actually run.
func TestRenderAutoresearchAgentPage(t *testing.T) {
	aw := &agentofficev1alpha1.AgentWorkstation{
		ObjectMeta: metav1.ObjectMeta{Name: "granite-8b-experimenter", Namespace: "agent-office"},
		Spec: agentofficev1alpha1.AgentWorkstationSpec{
			DisplayName:  "Granite-8B QLoRA Experimenter",
			Description:  "QLoRA proposer.",
			Emoji:        "🔬",
			SystemPrompt: "You are a QLoRA experimenter...",
			Tools: &agentofficev1alpha1.ToolsSpec{
				Allow: []string{"memory", "file_read", "file_write"},
			},
			Runtime: &agentofficev1alpha1.RuntimeSpec{
				Shared: &agentofficev1alpha1.SharedRuntime{GatewayRef: "research-gateway"},
			},
			Model: agentofficev1alpha1.ModelSpec{
				ModelName: "gpt-5.5",
				Provider:  agentofficev1alpha1.ModelProvider("openai-codex"),
			},
		},
	}
	out := renderAutoresearchAgentPage(aw)
	for _, want := range []string{
		"granite-8b-experimenter",
		"Granite-8B QLoRA Experimenter",
		"research-gateway",
		"openclaw agent --agent granite-8b-experimenter",
		"file_read",
		"file_write",
		"gpt-5.5",
		"openai-codex",
		"You are a QLoRA experimenter",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("agent page missing %q", want)
		}
	}
}

// ---- end of v0.0.53 KnowledgeBase autoresearch-bootstrap tests ----

// ---- v0.0.55: BackstageCatalog entity-emit tests ----

// TestMvToBackstageResource_BaseModel verifies a base-model
// ModelVersion is converted to a Backstage Resource with the right
// title, annotations, and tags.
func TestMvToBackstageResource_BaseModel(t *testing.T) {
	rm := map[string]any{
		"id":          "1",
		"name":        "ibm-granite/granite-4.1-8b",
		"description": "IBM Granite 4.1-8B Instruct.",
	}
	mv := map[string]any{
		"id":                 "2",
		"name":               "main",
		"registeredModelId":  "1",
		"customProperties": map[string]any{
			"uri": map[string]any{
				"string_value":  "huggingface://ibm-granite/granite-4.1-8b",
				"metadataType": "MetadataStringValue",
			},
			"kind": map[string]any{
				"string_value":  "base-model",
				"metadataType": "MetadataStringValue",
			},
		},
	}
	got := mvToBackstageResource("autoresearch-registry", rm, mv)
	if got.Kind != "Resource" {
		t.Errorf("wanted kind=Resource, got %s", got.Kind)
	}
	if !strings.Contains(got.Name, "granite-4-1-8b") {
		t.Errorf("name should contain sanitized granite id: %q", got.Name)
	}
	// v0.0.59: titles are source-prefixed so users can tell Registry
	// vs Catalog at a glance in the EntityPicker dropdown.
	if got.Title != "[Registry] ibm-granite/granite-4.1-8b @ main" {
		t.Errorf("unexpected title: %q", got.Title)
	}
	if got.Annotations["agentoffice.ai/model-uri"] != "huggingface://ibm-granite/granite-4.1-8b" {
		t.Errorf("missing uri annotation: %#v", got.Annotations)
	}
	if got.Annotations["agentoffice.ai/model-source"] != "model-registry" {
		t.Errorf("missing model-source annotation: %#v", got.Annotations)
	}
	hasBaseTag, hasSourceTag := false, false
	for _, tag := range got.Tags {
		if tag == "base-model" {
			hasBaseTag = true
		}
		if tag == "model-registry" {
			hasSourceTag = true
		}
	}
	if !hasBaseTag {
		t.Errorf("missing base-model tag: %v", got.Tags)
	}
	if !hasSourceTag {
		t.Errorf("missing model-registry tag (needed by template's Source filter): %v", got.Tags)
	}
}

// TestMvToBackstageResource_FineTuned verifies a fine-tuned-adapter
// ModelVersion surfaces eval_loss + kept annotations.
func TestMvToBackstageResource_FineTuned(t *testing.T) {
	rm := map[string]any{
		"id":          "3",
		"name":        "autoresearch-granite-8b",
		"description": "Fine-tunes of granite-4.1-8b.",
	}
	mv := map[string]any{
		"id":                "5",
		"name":              "round-37",
		"registeredModelId": "3",
		"customProperties": map[string]any{
			"adapter_uri": map[string]any{
				"string_value":  "quay-quay.../adapters:round-37",
				"metadataType": "MetadataStringValue",
			},
			"eval_loss": map[string]any{
				"string_value":  "0.496074",
				"metadataType": "MetadataStringValue",
			},
			"kept": map[string]any{
				"string_value":  "true",
				"metadataType": "MetadataStringValue",
			},
			"kind": map[string]any{
				"string_value":  "fine-tuned-adapter",
				"metadataType": "MetadataStringValue",
			},
		},
	}
	got := mvToBackstageResource("autoresearch-registry", rm, mv)
	if got.Annotations["agentoffice.ai/eval-loss"] != "0.496074" {
		t.Errorf("missing eval-loss annotation: %#v", got.Annotations)
	}
	if got.Annotations["agentoffice.ai/kept"] != "true" {
		t.Errorf("missing kept annotation: %#v", got.Annotations)
	}
}

// TestSanitizeName covers the edge cases that bit Backstage's strict
// DNS-label validation in past projects (uppercase, slashes, dots,
// trailing dashes, all-bad-chars).
func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"ibm-granite/granite-4.1-8b":         "ibm-granite-granite-4-1-8b",
		"AutoResearch/Granite-8B":            "autoresearch-granite-8b",
		"foo.bar.baz":                        "foo-bar-baz",
		"!!@@##":                             "model",
		"-leading-and-trailing-dashes-":      "leading-and-trailing-dashes",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRenderBackstageYAML_Empty makes sure we emit a valid empty
// catalog (with a comment) rather than something Backstage rejects.
func TestRenderBackstageYAML_Empty(t *testing.T) {
	out := renderBackstageYAML(nil)
	if !strings.Contains(out, "no ModelVersion entries found") {
		t.Errorf("missing 'no entries' comment in empty output:\n%s", out)
	}
	if strings.Contains(out, "---") {
		t.Errorf("empty catalog shouldn't have any document separators")
	}
}

// TestRenderBackstageYAML_Roundtrip verifies a couple entities render
// to a YAML doc whose required fields are present + deterministic.
func TestRenderBackstageYAML_Roundtrip(t *testing.T) {
	entities := []backstageEntity{
		{
			Kind:        "Resource",
			Name:        "granite-4-1-8b-main",
			Title:       "ibm-granite/granite-4.1-8b @ main",
			Description: "IBM Granite 4.1-8B",
			Tags:        []string{"model", "base-model"},
			Annotations: map[string]string{
				"agentoffice.ai/model-uri":     "huggingface://ibm-granite/granite-4.1-8b",
				"agentoffice.ai/model-version": "main",
			},
			SpecType:  "ml-model",
			Owner:     "user:default/deanpeterson",
			Lifecycle: "production",
		},
	}
	out := renderBackstageYAML(entities)
	for _, want := range []string{
		"apiVersion: backstage.io/v1alpha1",
		"kind: Resource",
		"name: granite-4-1-8b-main",
		`title: "ibm-granite/granite-4.1-8b @ main"`,
		"type: ml-model",
		"owner: user:default/deanpeterson",
		"agentoffice.ai/model-uri",
		"---",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered YAML missing %q\nfull output:\n%s", want, out)
		}
	}
}

// ---- end of v0.0.55 BackstageCatalog tests ----

// TestResolveTrainerImage_Precedence verifies the three-tier lookup
// (spec > ConfigMap > const). Catches regressions in the precedence
// order — e.g. if the ConfigMap ever wins over an explicit spec
// override, every project gets the cluster-wide default and CR-
// level pins become silently dead.
func TestResolveTrainerImage_Precedence(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: trainerImageDefaultsConfigMap, Namespace: "test-ns"},
		Data:       map[string]string{"trainerImage": "cm-image:v1"},
	}
	r := newReconcilerForRenderTest(t, cm)

	// 1. spec.trainer.image wins when set.
	specP := newProject("spec-image:v1")
	if got := r.resolveTrainerImage(context.Background(), specP); got != "spec-image:v1" {
		t.Errorf("spec wins: want spec-image:v1, got %q", got)
	}
	// 2. ConfigMap falls in when spec empty.
	cmP := newProject("")
	if got := r.resolveTrainerImage(context.Background(), cmP); got != "cm-image:v1" {
		t.Errorf("ConfigMap fallback: want cm-image:v1, got %q", got)
	}
	// 3. Const fallback when neither set — use a different namespace
	//    so the ConfigMap doesn't apply.
	constP := &agentofficev1alpha1.AutoResearchProject{
		ObjectMeta: metav1.ObjectMeta{Name: "no-cm", Namespace: "other-ns"},
	}
	if got := r.resolveTrainerImage(context.Background(), constP); got != defaultTrainerImage {
		t.Errorf("const fallback: want %q, got %q", defaultTrainerImage, got)
	}
}
