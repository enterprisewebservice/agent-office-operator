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
