package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// packageRefCache memoizes resolved packageRef content. Digest-pinned
// entries are immutable and never expire; undigested entries expire on
// the registry federation TTL so a republished version converges.
var packageRefCache = struct {
	mu sync.Mutex
	m  map[string]packageRefEntry
}{m: map[string]packageRefEntry{}}

type packageRefEntry struct {
	content string
	fetched time.Time
	pinned  bool
}

// defaultRegistryBase returns the first federated registry base URL
// from AGENT_REGISTRY_URLS, tolerating both base ("https://r/v1") and
// index ("https://r/v1/index.json") forms.
func defaultRegistryBase() string {
	raw := strings.TrimSpace(os.Getenv("AGENT_REGISTRY_URLS"))
	for _, u := range strings.Split(raw, ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		u = strings.TrimSuffix(u, "/index.json")
		return strings.TrimSuffix(u, "/")
	}
	return ""
}

// parsePackageCoordinate splits "name:version" (an optional
// "namespace/" prefix is dropped — registry content paths key on name
// and version only).
func parsePackageCoordinate(ref string) (name, version string, err error) {
	c := strings.TrimSpace(ref)
	if i := strings.LastIndex(c, "/"); i >= 0 {
		c = c[i+1:]
	}
	name, version, ok := strings.Cut(c, ":")
	if !ok || name == "" || version == "" {
		return "", "", fmt.Errorf("packageRef.ref %q is not name:version", ref)
	}
	return name, version, nil
}

// resolvePackageRef fetches the SKILL.md body for a registry
// coordinate, verifying the declared digest when present. The
// artifact manifest's own content URL is authoritative; when the
// manifest cannot be read, the conventional content path is tried.
func resolvePackageRef(ctx context.Context, src *agentofficev1alpha1.PackageRefSource) (string, error) {
	name, version, err := parsePackageCoordinate(src.Ref)
	if err != nil {
		return "", err
	}
	base := strings.TrimSuffix(strings.TrimSpace(src.Registry), "/")
	if base == "" {
		base = defaultRegistryBase()
	}
	if base == "" {
		return "", fmt.Errorf("packageRef %q: no registry set and AGENT_REGISTRY_URLS is empty", src.Ref)
	}

	wantDigest := strings.TrimPrefix(strings.TrimSpace(src.Digest), "sha256:")
	cacheKey := base + "|" + name + "|" + version + "|" + wantDigest

	packageRefCache.mu.Lock()
	if e, ok := packageRefCache.m[cacheKey]; ok && (e.pinned || time.Since(e.fetched) < registryTTL) {
		packageRefCache.mu.Unlock()
		return e.content, nil
	}
	packageRefCache.mu.Unlock()

	contentURL := fmt.Sprintf("%s/%s/%s/skills/%s.md", base, name, version, name)
	if manRaw, err := fetchText(ctx, fmt.Sprintf("%s/%s/%s/mindifact.json", base, name, version)); err == nil {
		var man struct {
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(manRaw), &man) == nil && man.Content != "" {
			if bu, err := url.Parse(base + "/"); err == nil {
				if cu, err := url.Parse(man.Content); err == nil {
					contentURL = bu.ResolveReference(cu).String()
				}
			}
		}
	}

	body, err := fetchText(ctx, contentURL)
	if err != nil {
		return "", fmt.Errorf("packageRef %s: fetch %s: %w", src.Ref, contentURL, err)
	}
	sum := sha256.Sum256([]byte(body))
	got := hex.EncodeToString(sum[:])
	if wantDigest != "" && !strings.EqualFold(got, wantDigest) {
		return "", fmt.Errorf("packageRef %s: content digest %s does not match pinned digest %s — refusing to deliver", src.Ref, got, wantDigest)
	}

	packageRefCache.mu.Lock()
	packageRefCache.m[cacheKey] = packageRefEntry{content: body, fetched: time.Now(), pinned: wantDigest != ""}
	packageRefCache.mu.Unlock()
	return body, nil
}
