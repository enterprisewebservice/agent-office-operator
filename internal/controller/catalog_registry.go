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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Registry federation: /catalog/packs also serves artifacts published to
// external mindifact registries, marked as not installed.
//
// Before this, the catalog could only see objects that already existed
// on the cluster, so a pack had to be hand-copied into every consumer
// before anyone could find it. Searching for something you have not
// installed yet — the entire point of a registry — was impossible.
//
// A registry is a static file tree at predictable coordinates (Maven's
// design, and mindifact.ai's):
//
//	GET <base>/index.json → {registry, apiVersion, count, artifacts:[…]}
//
// Configure with AGENT_REGISTRY_URLS (comma-separated index URLs).
// Unset ⇒ no federation, and /catalog/packs behaves exactly as before,
// so a cluster with no egress is unaffected.
//
// Results are cached for registryTTL. The index is a handful of KB and
// the composer hits it on every keystroke-driven reload; re-fetching per
// request would put a public site in the path of the create wizard.
// A fetch failure serves the last good copy and, failing that, nothing:
// the local catalog must never go dark because a remote registry did.
const registryTTL = 5 * time.Minute

type registryArtifact struct {
	Namespace   string   `json:"namespace"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Manifest    string   `json:"manifest"`
	Content     string   `json:"content,omitempty"`
	OCI         string   `json:"oci,omitempty"`
	Digest      string   `json:"digest,omitempty"`
	Site        string   `json:"site,omitempty"`
	Member      string   `json:"member,omitempty"`
	Members     []string `json:"members,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	// Requires is heterogeneous across publishers: a list of strings for
	// pack-level deps, or objects for resource deps. Kept raw and
	// interpreted by the consumer.
	Requires json.RawMessage `json:"requires,omitempty"`
}

type registryIndex struct {
	Registry   string             `json:"registry"`
	APIVersion string             `json:"apiVersion"`
	Artifacts  []registryArtifact `json:"artifacts"`
}

type registryCache struct {
	mu      sync.Mutex
	fetched time.Time
	packs   []catalogPack
}

var remoteRegistry = &registryCache{}

// registryPacks returns federated artifacts as catalogPacks. Never
// returns an error: a registry that is slow, down, or serving garbage
// degrades to "no remote packs", never to a broken catalog.
func registryPacks(ctx context.Context) []catalogPack {
	raw := strings.TrimSpace(os.Getenv("AGENT_REGISTRY_URLS"))
	if raw == "" {
		return nil
	}
	remoteRegistry.mu.Lock()
	defer remoteRegistry.mu.Unlock()
	if time.Since(remoteRegistry.fetched) < registryTTL && remoteRegistry.packs != nil {
		return remoteRegistry.packs
	}

	var out []catalogPack
	for _, u := range strings.Split(raw, ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		idx, err := fetchRegistryIndex(ctx, u)
		if err != nil {
			continue
		}
		base := u
		if i := strings.LastIndex(u, "/"); i > 0 {
			base = u[:i]
		}
		for _, a := range idx.Artifacts {
			out = append(out, remoteToPack(idx.Registry, base, a))
		}
	}
	if out == nil && remoteRegistry.packs != nil {
		return remoteRegistry.packs // serve the last good copy
	}
	remoteRegistry.fetched = time.Now()
	remoteRegistry.packs = out
	return out
}

func fetchRegistryIndex(ctx context.Context, url string) (*registryIndex, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry %s: HTTP %d", url, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var idx registryIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

func remoteToPack(registry, base string, a registryArtifact) catalogPack {
	abs := func(p string) string {
		if p == "" || strings.HasPrefix(p, "http") {
			return p
		}
		// index.json paths are site-absolute (/v1/…); base ends at /v1
		if strings.HasPrefix(p, "/") {
			if i := strings.Index(base, "://"); i > 0 {
				if j := strings.Index(base[i+3:], "/"); j > 0 {
					return base[:i+3+j] + p
				}
			}
			return base + p
		}
		return base + "/" + p
	}

	// Pack-level `requires` naming a resource becomes a dependency the
	// composer already knows how to render (available=false: a remote
	// artifact's needs are, by definition, not verified on this cluster).
	var deps []catalogDependency
	if len(a.Requires) > 0 {
		var objs []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(a.Requires, &objs); err == nil {
			for _, o := range objs {
				if o.Kind != "" && o.Name != "" {
					deps = append(deps, catalogDependency{Kind: o.Kind, Name: o.Name})
				}
			}
		}
	}

	desc := a.Description
	if a.Kind == "meta-pack" && len(a.Members) > 0 {
		desc = fmt.Sprintf("%s (meta-pack: %s)", desc, strings.Join(a.Members, ", "))
	}

	return catalogPack{
		Type:         "skill", // composable unit; kind is carried in Registry/Origin
		Name:         a.Name,
		DisplayName:  a.Name,
		Description:  desc,
		Version:      a.Version,
		Tier:         "mindifact",
		Dependencies: deps,
		Registry:     registry,
		Namespace:    a.Namespace,
		ArtifactKind: a.Kind,
		Member:       a.Member,
		Members:      a.Members,
		Skills:       a.Skills,
		Manifest:     abs(a.Manifest),
		ContentURL:   abs(a.Content),
		OCI:          a.OCI,
		Installed:    false,
	}
}
