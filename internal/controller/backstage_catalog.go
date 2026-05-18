/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// Backstage catalog HTTP handler.
//
// v0.0.55: surface the RHOAI Model Registry as Backstage `Resource`
// entities so RHDH's catalog (and the karpathy-research-agent
// template's `baseModel` EntityPicker) can see what models are
// available cluster-wide without anyone hand-curating a list.
//
// The Model Registry IS the source of truth: every ModelRegistry
// CR's REST API contents are converted to Backstage entities on each
// request. Backstage's catalog ingestion polls this endpoint (default
// 100s); the dropdown stays in sync with what's actually registered.
//
// Endpoint shape:
//
//   GET /backstage/catalog.yaml
//   200 OK
//   Content-Type: application/yaml
//   <multidocument YAML with one Resource per ModelVersion>
//
// Authentication: none (the catalog data is metadata, not secrets —
// model names + URIs are not sensitive on this internal cluster).
// If that changes, add a Bearer-token check here and have RHDH's
// app-config pass the token via a Secret.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// modelRegistryGVR locates the RHOAI v1beta1 ModelRegistry CRs.
// (Note: distinct from the v1alpha1 ModelRegistry component CR in
// components.platform.opendatahub.io — see autoresearchproject_dsp.go
// resolveTrainerImage for the same distinction.)
var modelRegistryGVR = schema.GroupVersionKind{
	Group:   "modelregistry.opendatahub.io",
	Version: "v1beta1",
	Kind:    "ModelRegistryList",
}

// +kubebuilder:rbac:groups=modelregistry.opendatahub.io,resources=modelregistries,verbs=get;list;watch

// BackstageCatalogHandler implements net/http.Handler. The operator
// wires it into the manager's HTTP server in main.go.
type BackstageCatalogHandler struct {
	Client     client.Client
	HTTPClient *http.Client // for talking to MR REST APIs in-cluster
}

// NewBackstageCatalogHandler returns a handler with sane defaults.
func NewBackstageCatalogHandler(c client.Client) *BackstageCatalogHandler {
	return &BackstageCatalogHandler{
		Client: c,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
			Timeout: 15 * time.Second,
		},
	}
}

// ServeHTTP is the entry point. Lists all ModelRegistry instances
// cluster-wide, queries each, and writes Backstage entities to the
// response.
func (h *BackstageCatalogHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/backstage/catalog.yaml" && r.URL.Path != "/backstage/catalog" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	entities, err := h.collect(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("# backstage catalog collect failed: %v\n", err), http.StatusInternalServerError)
		return
	}
	out := renderBackstageYAML(entities)

	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "max-age=30")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, out)
}

// modelCatalogServiceURL is the in-cluster URL of RHOAI's Model
// Catalog REST API. v0.0.56 surfaces these as Backstage Resource
// entities too so the karpathy template's baseModel dropdown shows
// both registered ModelVersions AND the Red Hat-curated catalog.
const modelCatalogServiceURL = "https://model-catalog.rhoai-model-registries.svc.cluster.local:8443"

// saTokenPath is the standard projected SA token path the operator
// uses to authenticate to in-cluster REST services. Declared as a
// var rather than a const so unit tests can point it at a fixture
// without monkey-patching os.ReadFile.
var saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// collect walks every ModelRegistry CR + the Model Catalog REST API
// and gathers their models. Returns one entity per (RegisteredModel,
// ModelVersion) pair AND one per Model Catalog entry. Both sources
// are best-effort: a failure in one doesn't blank the other.
func (h *BackstageCatalogHandler) collect(ctx context.Context) ([]backstageEntity, error) {
	mrList := &unstructured.UnstructuredList{}
	mrList.SetGroupVersionKind(modelRegistryGVR)
	if err := h.Client.List(ctx, mrList); err != nil {
		// No instances installed yet? Return an empty catalog rather
		// than an error so Backstage doesn't drop the location.
		if isCRDMissingErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list ModelRegistry: %w", err)
	}

	var out []backstageEntity
	for _, mr := range mrList.Items {
		ns := mr.GetNamespace()
		name := mr.GetName()
		entities, err := h.fetchEntitiesFor(ctx, ns, name)
		if err != nil {
			// One bad instance shouldn't blank the whole catalog.
			// Emit a single placeholder so the user sees it surfaced
			// somewhere instead of silently missing.
			out = append(out, backstageEntity{
				Kind:        "Resource",
				Name:        sanitizeName(fmt.Sprintf("%s-%s-error", ns, name)),
				Title:       fmt.Sprintf("ModelRegistry %s/%s (error)", ns, name),
				Description: fmt.Sprintf("collection failed: %v", err),
				Tags:        []string{"model-registry", "error"},
				SpecType:    "ml-model-registry",
				Lifecycle:   "experimental",
				Owner:       "user:default/deanpeterson",
			})
			continue
		}
		out = append(out, entities...)
	}

	// v0.0.56: also enumerate the Model Catalog. Best-effort —
	// if the catalog isn't reachable, isn't installed, or denies
	// our SA token, we log and continue with just the Registry
	// entities (don't blank the whole dropdown over one bad source).
	catalogEntities, catErr := h.fetchModelCatalogEntities(ctx)
	if catErr != nil {
		// Surface as a single placeholder entity so the operator log
		// + Backstage catalog both reflect the broken source.
		out = append(out, backstageEntity{
			Kind:        "Resource",
			Name:        "model-catalog-unreachable",
			Title:       "RHOAI Model Catalog (unreachable)",
			Description: fmt.Sprintf("collection failed: %v", catErr),
			Tags:        []string{"model-catalog", "error"},
			SpecType:    "ml-model-catalog",
			Lifecycle:   "experimental",
			Owner:       "user:default/deanpeterson",
		})
	} else {
		out = append(out, catalogEntities...)
	}

	return out, nil
}

// fetchModelCatalogEntities queries RHOAI's Model Catalog REST API
// and emits one Backstage Resource entity per catalog model. The
// catalog has its own schema (different from ModelRegistry's), so
// this helper does the conversion separately rather than trying to
// share mvToBackstageResource.
//
// Auth: in-cluster Bearer token from the operator's SA. The catalog
// uses kube-rbac-proxy in front; the operator SA needs at least
// `get` on whatever resource the proxy guards (typically the
// catalog's own service or a marker permission). v0.0.55's separate
// ClusterRoleBinding for modelregistries doesn't cover this — the
// operator pod might get 401/403 until appropriate RBAC is granted.
// In that case we log and skip rather than fail the whole endpoint.
func (h *BackstageCatalogHandler) fetchModelCatalogEntities(ctx context.Context) ([]backstageEntity, error) {
	token, terr := os.ReadFile(saTokenPath)
	if terr != nil {
		// Not running in-cluster or token not mounted — return empty,
		// not an error. Lets unit tests and local dev still work.
		return nil, nil
	}

	url := modelCatalogServiceURL + "/api/model_catalog/v1alpha1/models?pageSize=200"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))

	resp, err := h.HTTPClient.Do(req)
	if err != nil {
		// Catalog not installed (DNS fail) is the common case on
		// clusters without the Model Catalog component. Don't error;
		// just emit nothing.
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		// Auth gap. Surface as an error so the user knows to grant
		// RBAC; the caller renders this as a "(unreachable)"
		// placeholder Resource so it's visible in the dropdown.
		return nil, fmt.Errorf("model-catalog auth denied (HTTP %d) — "+
			"the operator's SA likely needs additional RBAC on the "+
			"model-catalog service", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("model-catalog returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var listResp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		return nil, fmt.Errorf("decode catalog response: %w", err)
	}

	out := make([]backstageEntity, 0, len(listResp.Items))
	for _, m := range listResp.Items {
		ent := catalogModelToBackstageResource(m)
		if ent.Name != "" {
			out = append(out, ent)
		}
	}
	return out, nil
}

// catalogModelToBackstageResource converts one Model Catalog item to
// a Backstage Resource entity. The catalog schema is different from
// ModelRegistry's (no registered_model + model_version split — each
// item IS a model), so the metadata mapping is its own logic.
//
// Key fields we surface:
//   - name + source_id     → unique Backstage entity name
//   - description / readme → entity description
//   - tasks, license       → tags
//   - provider             → annotation
//
// We don't currently look up a download URI from the catalog; the
// trainer's `from_pretrained()` is expected to resolve the HF id
// derived from the catalog name (most catalog entries match HF id
// for Granite/Llama/etc.). When the operator's pipeline learns to
// pull catalog artifacts directly, we'll surface that URI here too.
func catalogModelToBackstageResource(m map[string]any) backstageEntity {
	name, _ := m["name"].(string)
	if name == "" {
		return backstageEntity{}
	}
	sourceID, _ := m["source_id"].(string)
	desc, _ := m["description"].(string)
	provider, _ := m["provider"].(string)
	license, _ := m["license"].(string)
	licenseLink, _ := m["licenseLink"].(string)
	logo, _ := m["logo"].(string)

	// `tasks` is a list of strings; pick the first as the headline.
	primaryTask := ""
	if rawTasks, ok := m["tasks"].([]any); ok && len(rawTasks) > 0 {
		if t, ok := rawTasks[0].(string); ok {
			primaryTask = t
		}
	}
	// `language` likewise.
	primaryLang := ""
	if rawLangs, ok := m["language"].([]any); ok && len(rawLangs) > 0 {
		if l, ok := rawLangs[0].(string); ok {
			primaryLang = l
		}
	}

	tags := []string{"model", "model-catalog"}
	if sourceID != "" {
		tags = append(tags, "source-"+sanitizeName(sourceID))
	}
	if primaryTask != "" {
		tags = append(tags, sanitizeName(primaryTask))
	}

	annos := map[string]string{
		"agentoffice.ai/model-source":    "model-catalog",
		"agentoffice.ai/catalog-name":    name,
		"agentoffice.ai/catalog-source":  sourceID,
		// model-uri: catalog model names usually match the HF id (e.g.
		// "granite-3.1-8b-lab-v1"). Surface as a candidate URI; the
		// template's catalog:fetch resolution + the trainer will use
		// it as the base_model HF id.
		"agentoffice.ai/model-uri": "huggingface://" + name,
	}
	if provider != "" {
		annos["agentoffice.ai/provider"] = provider
	}
	if license != "" {
		annos["agentoffice.ai/license"] = license
	}
	if licenseLink != "" {
		annos["agentoffice.ai/license-link"] = licenseLink
	}
	if logo != "" {
		annos["agentoffice.ai/logo"] = logo
	}
	if primaryLang != "" {
		annos["agentoffice.ai/language"] = primaryLang
	}

	return backstageEntity{
		Kind: "Resource",
		// Prefix with `catalog-` so it can't collide with a Registry
		// entity that happens to share a sanitized name. The
		// EntityPicker uses these as opaque refs anyway.
		Name:        sanitizeName("catalog-" + name),
		Title:       name,
		Description: desc,
		Tags:        tags,
		Annotations: annos,
		SpecType:    "ml-model",
		Owner:       "user:default/deanpeterson",
		Lifecycle:   "production",
	}
}

// fetchEntitiesFor queries one ModelRegistry's REST API for its
// registered models + their versions and emits one Resource per
// (RegisteredModel, ModelVersion) pair. v0.0.52's deriveModelRegistryTarget
// picks the right Service name pattern (instance:8080 with a
// legacy-fallback to instance-rest:8443) — we reuse the same logic.
func (h *BackstageCatalogHandler) fetchEntitiesFor(ctx context.Context, ns, instance string) ([]backstageEntity, error) {
	baseURL, err := h.modelRegistryRESTURL(ctx, ns, instance)
	if err != nil {
		return nil, err
	}

	// 1. registered models
	rmList, err := h.getJSON(ctx, baseURL+"/api/model_registry/v1alpha3/registered_models?pageSize=100")
	if err != nil {
		return nil, fmt.Errorf("registered_models: %w", err)
	}
	regModels, _ := rmList["items"].([]any)

	// 2. model versions (one big list across all registered models)
	mvList, err := h.getJSON(ctx, baseURL+"/api/model_registry/v1alpha3/model_versions?pageSize=500")
	if err != nil {
		return nil, fmt.Errorf("model_versions: %w", err)
	}
	versions, _ := mvList["items"].([]any)

	// Build a quick id → RegisteredModel map.
	rmByID := map[string]map[string]any{}
	for _, m := range regModels {
		if rm, ok := m.(map[string]any); ok {
			if id, ok := rm["id"].(string); ok {
				rmByID[id] = rm
			}
		}
	}

	var out []backstageEntity
	for _, v := range versions {
		mv, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if state, _ := mv["state"].(string); strings.EqualFold(state, "ARCHIVED") {
			continue // skip archived versions
		}
		rmID, _ := mv["registeredModelId"].(string)
		rm := rmByID[rmID]
		if rm == nil {
			continue // orphaned ModelVersion
		}

		ent := mvToBackstageResource(instance, rm, mv)
		out = append(out, ent)
	}
	return out, nil
}

// modelRegistryRESTURL resolves the in-cluster URL of the
// ModelRegistry's REST proxy. Mirrors deriveModelRegistryTarget's
// service-name probing: try `<instance>:8080` first, then
// `<instance>-rest:8443` for older RHOAI shapes.
func (h *BackstageCatalogHandler) modelRegistryRESTURL(ctx context.Context, ns, instance string) (string, error) {
	candidates := []struct {
		svc  string
		port int
	}{
		{instance, 8080},
		{instance + "-rest", 8443},
	}
	for _, c := range candidates {
		probe := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", c.svc, ns, c.port)
		if c.port == 8443 {
			probe = fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", c.svc, ns, c.port)
		}
		// Just a HEAD on the root to confirm the service is reachable.
		req, _ := http.NewRequestWithContext(ctx, "HEAD", probe+"/", nil)
		resp, err := h.HTTPClient.Do(req)
		if err == nil {
			resp.Body.Close()
			return probe, nil
		}
	}
	return "", fmt.Errorf("no reachable ModelRegistry REST service for %s/%s "+
		"(tried %s:8080 and %s-rest:8443)", ns, instance, instance, instance)
}

// getJSON does a GET + JSON decode.
func (h *BackstageCatalogHandler) getJSON(ctx context.Context, url string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateMsg(string(body), 200))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	return out, nil
}

// backstageEntity is a small subset of Backstage's catalog Entity
// schema — enough for our use case (Resource + Component).
type backstageEntity struct {
	Kind        string
	Name        string
	Title       string
	Description string
	Tags        []string
	Annotations map[string]string
	SpecType    string
	Owner       string
	Lifecycle   string
}

// mvToBackstageResource turns one (RegisteredModel, ModelVersion)
// pair into a backstageEntity. The entity's `metadata.name` must be
// k8s-DNS-compatible (no slashes, dots, etc.); we sanitize.
//
// We surface custom_properties.uri/adapter_uri/hf_id when present so
// the consumer (template's EntityPicker → trainer params) has the
// concrete pull location.
func mvToBackstageResource(registryInstance string, rm, mv map[string]any) backstageEntity {
	rmName, _ := rm["name"].(string)
	mvName, _ := mv["name"].(string)
	desc, _ := mv["description"].(string)
	if desc == "" {
		desc, _ = rm["description"].(string)
	}

	customProps, _ := mv["customProperties"].(map[string]any)
	uri := extractCustomString(customProps, "uri", "adapter_uri", "hf_id")
	kind := extractCustomString(customProps, "kind") // base-model | fine-tuned-adapter | ...
	evalLoss := extractCustomString(customProps, "eval_loss")
	kept := extractCustomString(customProps, "kept")
	baseModel := extractCustomString(customProps, "base_model")

	if kind == "" {
		// Best-effort guess.
		if uri != "" && strings.HasPrefix(uri, "quay") || strings.HasPrefix(uri, "oci://") {
			kind = "fine-tuned-adapter"
		} else {
			kind = "base-model"
		}
	}

	tags := []string{"model", kind}
	if registryInstance != "" {
		tags = append(tags, "registry-"+sanitizeName(registryInstance))
	}

	annos := map[string]string{
		"agentoffice.ai/model-registry":  registryInstance,
		"agentoffice.ai/registered-model": rmName,
		"agentoffice.ai/model-version":    mvName,
	}
	if uri != "" {
		annos["agentoffice.ai/model-uri"] = uri
	}
	if baseModel != "" {
		annos["agentoffice.ai/base-model"] = baseModel
	}
	if evalLoss != "" {
		annos["agentoffice.ai/eval-loss"] = evalLoss
	}
	if kept != "" {
		annos["agentoffice.ai/kept"] = kept
	}

	title := rmName
	if mvName != "" && mvName != rmName {
		title = rmName + " @ " + mvName
	}

	return backstageEntity{
		Kind:        "Resource",
		Name:        sanitizeName(rmName + "-" + mvName),
		Title:       title,
		Description: desc,
		Tags:        tags,
		Annotations: annos,
		SpecType:    "ml-model",
		Owner:       "user:default/deanpeterson",
		Lifecycle:   "production",
	}
}

// extractCustomString walks customProperties looking for the first
// key in the priority list whose value is a non-empty string. MR's
// schema wraps these as `{string_value: "...", metadataType: "..."}`.
func extractCustomString(props map[string]any, keys ...string) string {
	if props == nil {
		return ""
	}
	for _, k := range keys {
		raw, ok := props[k]
		if !ok {
			continue
		}
		obj, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := obj["string_value"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// sanitizeName lowercases + replaces all non-[a-z0-9-] with `-` and
// strips leading/trailing `-`. Backstage entity names must match
// RFC 1123 DNS label rules. Two distinct MR names mapping to the
// same sanitized form would collide; that's acceptable for the
// near term and surfaces as a duplicate-name validation error in
// Backstage rather than a silent overwrite.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "model"
	}
	return out
}

// renderBackstageYAML serializes the entities as a Backstage-compatible
// multi-doc YAML. Each Resource gets its own `---` separator. Hand-
// rolled rather than using sigs.k8s.io/yaml to keep the output
// deterministic (alphabetical keys per object, no extra fields).
func renderBackstageYAML(entities []backstageEntity) string {
	var b strings.Builder
	b.WriteString("# Auto-generated by agent-office-operator's BackstageCatalogHandler.\n")
	b.WriteString("# Source of truth: every ModelRegistry CR's REST API.\n")
	b.WriteString("# Refreshed on every request; Backstage catalog ingestion\n")
	b.WriteString("# typically polls this URL every ~100s.\n")
	if len(entities) == 0 {
		b.WriteString("# (no ModelVersion entries found in any ModelRegistry instance)\n")
	}
	for _, e := range entities {
		b.WriteString("---\n")
		b.WriteString("apiVersion: backstage.io/v1alpha1\n")
		b.WriteString("kind: " + e.Kind + "\n")
		b.WriteString("metadata:\n")
		b.WriteString("  name: " + e.Name + "\n")
		if e.Title != "" {
			b.WriteString("  title: " + yamlQuote(e.Title) + "\n")
		}
		if e.Description != "" {
			b.WriteString("  description: " + yamlQuote(e.Description) + "\n")
		}
		if len(e.Tags) > 0 {
			b.WriteString("  tags:\n")
			for _, t := range e.Tags {
				b.WriteString("    - " + t + "\n")
			}
		}
		if len(e.Annotations) > 0 {
			b.WriteString("  annotations:\n")
			// stable order for determinism
			for _, k := range sortedKeys(e.Annotations) {
				b.WriteString("    " + k + ": " + yamlQuote(e.Annotations[k]) + "\n")
			}
		}
		b.WriteString("spec:\n")
		b.WriteString("  type: " + e.SpecType + "\n")
		b.WriteString("  owner: " + e.Owner + "\n")
		if e.Lifecycle != "" {
			b.WriteString("  lifecycle: " + e.Lifecycle + "\n")
		}
	}
	return b.String()
}

// yamlQuote wraps a string in double quotes and escapes embedded
// quotes/backslashes. Good enough for human-readable description /
// title fields; not robust for arbitrary YAML content.
func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

// sortedKeys returns a deterministic ordering of map keys. Avoids
// pulling sort just for this; keys are small and we sort with a
// trivial insertion sort.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion sort — fewer than ~10 keys typically.
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j-1] > out[j] {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}

// isCRDMissingErr is a tiny helper — we treat "no such CRD" as an
// empty catalog rather than an error so Backstage doesn't drop the
// Location on a cluster that hasn't installed RHOAI Model Registry.
func isCRDMissingErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no kind \"ModelRegistry\" is registered") ||
		strings.Contains(msg, "no matches for kind \"ModelRegistry\"") ||
		strings.Contains(msg, "the server could not find the requested resource")
}
