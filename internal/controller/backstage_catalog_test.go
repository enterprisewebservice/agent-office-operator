/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withFakeSAToken writes a temp file with the given token contents
// and points saTokenPath at it for the duration of the test.
// Returns a restore function that resets the path.
func withFakeSAToken(t *testing.T, token string) func() {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte(token), 0600); err != nil {
		t.Fatalf("write fake SA token: %v", err)
	}
	orig := saTokenPath
	saTokenPath = p
	return func() {
		saTokenPath = orig
	}
}

// TestCatalogModelToBackstageResource covers the mapping from a
// raw Model Catalog item (one entry from
// `/api/model_catalog/v1alpha1/models`) into a backstageEntity.
// v0.0.56: needed because the Catalog schema is different from
// ModelRegistry's — each item IS a model (no registered/version
// split), and the entity name must be prefixed `catalog-` so it
// can never collide with a Registry-source entity.
func TestCatalogModelToBackstageResource(t *testing.T) {
	in := map[string]any{
		"name":         "granite-3.1-8b-lab-v1",
		"source_id":    "redhat_ai_models",
		"description":  "Granite 3.1 8B LAB instruct-tuned model.",
		"provider":     "Red Hat",
		"license":      "apache-2.0",
		"licenseLink":  "https://www.apache.org/licenses/LICENSE-2.0",
		"logo":         "data:image/svg+xml;base64,...",
		"tasks":        []any{"text-generation", "instruction-following"},
		"language":     []any{"en", "es"},
	}
	got := catalogModelToBackstageResource(in)

	if !strings.HasPrefix(got.Name, "catalog-") {
		t.Errorf("entity name must be prefixed `catalog-` to avoid Registry collisions; got %q", got.Name)
	}
	if got.Title != "granite-3.1-8b-lab-v1" {
		t.Errorf("title = %q, want %q", got.Title, "granite-3.1-8b-lab-v1")
	}
	if got.SpecType != "ml-model" {
		t.Errorf("specType = %q, want ml-model", got.SpecType)
	}
	if got.Lifecycle != "production" {
		t.Errorf("lifecycle = %q, want production", got.Lifecycle)
	}

	// Annotations: model-source = model-catalog, model-uri carries
	// the HF-style id, provider + license surfaced.
	if got.Annotations["agentoffice.ai/model-source"] != "model-catalog" {
		t.Errorf("model-source annotation = %q", got.Annotations["agentoffice.ai/model-source"])
	}
	if got.Annotations["agentoffice.ai/model-uri"] != "huggingface://granite-3.1-8b-lab-v1" {
		t.Errorf("model-uri annotation = %q", got.Annotations["agentoffice.ai/model-uri"])
	}
	if got.Annotations["agentoffice.ai/provider"] != "Red Hat" {
		t.Errorf("provider annotation = %q", got.Annotations["agentoffice.ai/provider"])
	}
	if got.Annotations["agentoffice.ai/license"] != "apache-2.0" {
		t.Errorf("license annotation = %q", got.Annotations["agentoffice.ai/license"])
	}
	if got.Annotations["agentoffice.ai/language"] != "en" {
		t.Errorf("language annotation = %q (want first item only)", got.Annotations["agentoffice.ai/language"])
	}

	// Tags include source-* derived from source_id + first task.
	wantTagSubstrings := []string{"model", "model-catalog", "source-redhat-ai-models", "text-generation"}
	for _, s := range wantTagSubstrings {
		found := false
		for _, tag := range got.Tags {
			if tag == s {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected tag %q not present in %v", s, got.Tags)
		}
	}
}

// TestCatalogModelToBackstageResource_Empty: a row without a name
// must drop out so we don't emit Backstage entities with bogus refs.
func TestCatalogModelToBackstageResource_Empty(t *testing.T) {
	got := catalogModelToBackstageResource(map[string]any{
		"description": "missing name field",
	})
	if got.Name != "" {
		t.Errorf("expected empty entity for nameless catalog row, got %+v", got)
	}
}

// TestFetchModelCatalogEntities_NoToken covers the local-dev /
// unit-test path where the SA token isn't mounted — the inner
// helper must return (nil, nil) so the catalog endpoint still
// serves the Registry portion without erroring.
func TestFetchModelCatalogEntities_NoToken(t *testing.T) {
	h := &BackstageCatalogHandler{
		HTTPClient: &http.Client{
			Timeout: 1 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		},
	}
	// Point at a URL that should never resolve — but with no SA
	// token, we never get that far.
	ents, err := h.fetchModelCatalogEntitiesFromURL(
		context.Background(),
		"https://nonexistent.invalid.local",
	)
	if err != nil {
		t.Logf("returned err (acceptable on dev hosts with a real SA token): %v", err)
	}
	if len(ents) > 0 {
		t.Errorf("expected 0 entities with no token, got %d", len(ents))
	}
}

// TestFetchModelCatalogEntities_HTTPMock points the handler at an
// httptest server that returns a canned Model Catalog response,
// verifying we (a) send a Bearer auth header, and (b) translate the
// item list into backstageEntity values.
//
// We bypass the SA-token read by stubbing a transport that
// short-circuits the cluster URL to the test server.
func TestFetchModelCatalogEntities_HTTPMock(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("expected Bearer auth header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"name":      "granite-3.1-8b-lab-v1",
					"source_id": "redhat_ai_models",
					"tasks":     []any{"text-generation"},
					"language":  []any{"en"},
				},
				{
					"name":      "llama-3.1-8b-instruct",
					"source_id": "redhat_ai_validated_models",
				},
			},
		})
	}))
	defer server.Close()

	// Redirect the catalog URL to our test server.
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.InsecureSkipVerify = true
	h := &BackstageCatalogHandler{
		HTTPClient: &http.Client{
			Timeout:   3 * time.Second,
			Transport: redirectTransport{base: transport, target: server.URL},
		},
	}

	// Pretend we have an SA token by overriding saTokenPath through
	// the test helper.
	restore := withFakeSAToken(t, "test-bearer-token")
	defer restore()

	// Call the inner helper directly so we don't need a fake Route CR
	// — the outer fetchModelCatalogEntities resolves the URL via a
	// Route lookup on h.Client, which is nil in this test.
	ents, err := h.fetchModelCatalogEntitiesFromURL(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ents) != 2 {
		t.Fatalf("expected 2 entities, got %d", len(ents))
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name, "catalog-") {
			t.Errorf("entity %q missing `catalog-` prefix (Registry collision risk)", e.Name)
		}
		if e.SpecType != "ml-model" {
			t.Errorf("entity %q wrong specType: %q", e.Name, e.SpecType)
		}
	}
}

// redirectTransport rewrites every outgoing request to point at a
// fixed test server URL while keeping the original path intact.
type redirectTransport struct {
	base   *http.Transport
	target string
}

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Replace scheme + host with the test server's.
	newReq := req.Clone(req.Context())
	parsedTarget, err := parseTargetURL(rt.target)
	if err != nil {
		return nil, err
	}
	newReq.URL.Scheme = parsedTarget.scheme
	newReq.URL.Host = parsedTarget.host
	return rt.base.RoundTrip(newReq)
}

type targetURL struct {
	scheme string
	host   string
}

func parseTargetURL(s string) (targetURL, error) {
	// quick parse: scheme://host[:port]
	idx := strings.Index(s, "://")
	if idx < 0 {
		return targetURL{}, http.ErrAbortHandler
	}
	scheme := s[:idx]
	host := s[idx+3:]
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	return targetURL{scheme: scheme, host: host}, nil
}
