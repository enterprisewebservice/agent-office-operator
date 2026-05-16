/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package dsp is a minimal HTTP client for OpenShift AI's Data
// Science Pipelines (DSP) REST API. It covers the small set of
// operations the AutoResearchProject reconciler needs:
//
//   - Upload a pipeline (v2 IR YAML), idempotently
//   - Look up the pipeline + a named version
//   - Create / get an Experiment (so Runs roll up under one
//     "AutoResearchProject" group in the Experiments UI)
//   - Create a Run with parameters
//   - Get Run status + parsed metrics
//
// Why hand-rolled instead of using the kfp Python SDK or a Go
// import of the kfp-server-api: those add dependencies that
// don't fit the agent-office-operator's tight Go module + don't
// give us much beyond what 5 HTTP endpoints do. Hand-rolled
// keeps the operator binary small and the failure modes
// debuggable.
//
// Auth: DSP API in OpenShift AI requires a bearer token
// authorized for the dspa namespace. We expect the caller to
// pass a ServiceAccount token (the operator's pod-mounted
// /var/run/secrets/kubernetes.io/serviceaccount/token).
package dsp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a single DSPA's REST API. Per-DSPA instance —
// the reconciler creates one Client per namespace it works in.
type Client struct {
	// BaseURL is the DSP API server's root (e.g.
	// "https://ds-pipeline-dspa.agent-office.svc.cluster.local:8443").
	// For in-cluster callers we use the Service URL; for
	// out-of-cluster, the Route.
	BaseURL string

	// Token is the bearer token the operator presents on each
	// request. Read from the operator pod's mounted SA token.
	Token string

	// HTTPClient is the underlying client. Caller can inject
	// one with custom TLS config (e.g. trust the cluster's
	// ingress CA when going via Route).
	HTTPClient *http.Client
}

// NewClient builds a Client with a reasonable default HTTP
// client (30s timeout, no body limit beyond what Go's default
// imposes). Caller is free to swap HTTPClient afterward.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Pipeline is a slim shape of the DSP v2beta1 Pipeline resource —
// only the fields the operator reads.
type Pipeline struct {
	PipelineID  string `json:"pipeline_id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
}

// PipelineVersion is the slim version shape — many runs can
// target the same pipeline; each version is a separately
// uploaded IR YAML.
type PipelineVersion struct {
	PipelineID        string `json:"pipeline_id"`
	PipelineVersionID string `json:"pipeline_version_id"`
	DisplayName       string `json:"display_name"`
}

// Experiment groups runs in the OpenShift AI Experiments UI.
type Experiment struct {
	ExperimentID string `json:"experiment_id"`
	DisplayName  string `json:"display_name"`
}

// Run is a single DSP execution. Status is one of "PENDING",
// "RUNNING", "SUCCEEDED", "SKIPPED", "FAILED", "CANCELING",
// "CANCELED" per the kfp v2 spec.
type Run struct {
	RunID        string                 `json:"run_id"`
	DisplayName  string                 `json:"display_name"`
	ExperimentID string                 `json:"experiment_id"`
	State        string                 `json:"state"`
	StateHistory []runStateHistoryEntry `json:"state_history,omitempty"`
	// RuntimeConfig.Parameters are the inputs to the pipeline.
	RuntimeConfig RuntimeConfig `json:"runtime_config,omitempty"`
}

type runStateHistoryEntry struct {
	UpdateTime string `json:"update_time"`
	State      string `json:"state"`
}

// RuntimeConfig wraps pipeline-run inputs.
type RuntimeConfig struct {
	Parameters map[string]any `json:"parameters,omitempty"`
}

// FindPipeline looks up a pipeline by displayName. Returns nil
// if not found (not an error — caller decides whether to
// upload).
func (c *Client) FindPipeline(ctx context.Context, displayName string) (*Pipeline, error) {
	body, err := c.doGet(ctx, "/apis/v2beta1/pipelines",
		map[string]string{
			"filter": fmt.Sprintf(`{"predicates":[{"operation":"EQUALS","key":"name","stringValue":%q}]}`, displayName),
		})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Pipelines []Pipeline `json:"pipelines"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode pipelines: %w (body=%s)", err, truncate(body, 400))
	}
	for i := range resp.Pipelines {
		if resp.Pipelines[i].DisplayName == displayName {
			return &resp.Pipelines[i], nil
		}
	}
	return nil, nil
}

// UploadPipeline POSTs the compiled IR YAML to DSP, creating a
// pipeline (and a v1 version) in one call. DSP's REST API
// uses multipart/form-data for the upload.
func (c *Client) UploadPipeline(ctx context.Context, displayName, description string, irYAML []byte) (*Pipeline, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("uploadfile", "pipeline.yaml")
	if err != nil {
		return nil, fmt.Errorf("multipart writer: %w", err)
	}
	if _, err := fw.Write(irYAML); err != nil {
		return nil, fmt.Errorf("write IR: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart: %w", err)
	}

	query := fmt.Sprintf("?name=%s&description=%s",
		urlEncode(displayName), urlEncode(description))
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+"/apis/v2beta1/pipelines/upload"+query, &buf)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	body, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	var pipe Pipeline
	if err := json.Unmarshal(body, &pipe); err != nil {
		return nil, fmt.Errorf("decode upload response: %w (body=%s)", err, truncate(body, 400))
	}
	return &pipe, nil
}

// FindOrCreatePipeline uploads the pipeline if missing,
// otherwise returns the existing one. Operator calls this at
// startup so every AutoResearchProject Run targets the same
// pipeline registration.
func (c *Client) FindOrCreatePipeline(ctx context.Context, displayName, description string, irYAML []byte) (*Pipeline, error) {
	if existing, err := c.FindPipeline(ctx, displayName); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	return c.UploadPipeline(ctx, displayName, description, irYAML)
}

// FindOrCreateExperiment is similar but for experiment groups.
// Returns the existing experiment when a name match exists.
func (c *Client) FindOrCreateExperiment(ctx context.Context, displayName, description string) (*Experiment, error) {
	body, err := c.doGet(ctx, "/apis/v2beta1/experiments", map[string]string{
		"filter": fmt.Sprintf(`{"predicates":[{"operation":"EQUALS","key":"name","stringValue":%q}]}`, displayName),
	})
	if err != nil {
		return nil, err
	}
	var listResp struct {
		Experiments []Experiment `json:"experiments"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		return nil, fmt.Errorf("decode experiments: %w", err)
	}
	for i := range listResp.Experiments {
		if listResp.Experiments[i].DisplayName == displayName {
			return &listResp.Experiments[i], nil
		}
	}
	// Create.
	create := map[string]any{
		"display_name": displayName,
		"description":  description,
	}
	bodyCreate, err := c.doPost(ctx, "/apis/v2beta1/experiments", create)
	if err != nil {
		return nil, err
	}
	var made Experiment
	if err := json.Unmarshal(bodyCreate, &made); err != nil {
		return nil, fmt.Errorf("decode created experiment: %w", err)
	}
	return &made, nil
}

// CreateRun submits a Run targeting the given pipeline +
// experiment with the provided parameters. Returns the
// created Run's ID + initial state.
func (c *Client) CreateRun(ctx context.Context, runDisplayName, experimentID, pipelineID string, params map[string]any) (*Run, error) {
	create := map[string]any{
		"display_name":   runDisplayName,
		"experiment_id":  experimentID,
		"pipeline_version_reference": map[string]any{
			"pipeline_id": pipelineID,
		},
		"runtime_config": map[string]any{
			"parameters": params,
		},
	}
	body, err := c.doPost(ctx, "/apis/v2beta1/runs", create)
	if err != nil {
		return nil, err
	}
	var run Run
	if err := json.Unmarshal(body, &run); err != nil {
		return nil, fmt.Errorf("decode run: %w (body=%s)", err, truncate(body, 400))
	}
	return &run, nil
}

// GetRun fetches a single Run's current state. The operator
// calls this on each reconcile to drain open runs.
func (c *Client) GetRun(ctx context.Context, runID string) (*Run, error) {
	body, err := c.doGet(ctx, "/apis/v2beta1/runs/"+runID, nil)
	if err != nil {
		return nil, err
	}
	var run Run
	if err := json.Unmarshal(body, &run); err != nil {
		return nil, fmt.Errorf("decode run: %w", err)
	}
	return &run, nil
}

// IsTerminal reports whether the Run's state is one we shouldn't
// keep polling — succeeded, failed, skipped, canceled.
func IsTerminal(state string) bool {
	switch state {
	case "SUCCEEDED", "FAILED", "SKIPPED", "CANCELED":
		return true
	}
	return false
}

// --- internal helpers ---

func (c *Client) applyAuth(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")
}

func (c *Client) doGet(ctx context.Context, path string, query map[string]string) ([]byte, error) {
	url := c.BaseURL + path
	if len(query) > 0 {
		sep := "?"
		for k, v := range query {
			url += sep + urlEncode(k) + "=" + urlEncode(v)
			sep = "&"
		}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	return c.doRequest(req)
}

func (c *Client) doPost(ctx context.Context, path string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	req.Header.Set("Content-Type", "application/json")
	return c.doRequest(req)
}

func (c *Client) doRequest(req *http.Request) ([]byte, error) {
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("DSP %s %s: HTTP %d %s",
			req.Method, req.URL.Path, resp.StatusCode, truncate(body, 400))
	}
	return body, nil
}

func urlEncode(s string) string {
	// Use Go's stdlib so we get correct handling of every
	// reserved character (e.g. ';' which Go's url.Parse strictly
	// rejects in queries since 1.17; '+' which gets parsed as a
	// space without encoding; '%' itself). Hand-rolled encoders
	// always end up missing something.
	return url.QueryEscape(s)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
