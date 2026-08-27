package controller

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ociRef is a parsed oci:// artifact reference.
type ociRef struct {
	host   string // registry host[:port]
	repo   string // org/name path
	tag    string
	digest string // optional manifest digest pin ("sha256:...")
}

var wwwAuthKV = regexp.MustCompile(`(\w+)="([^"]*)"`)

func parseOCIRef(ref string) (ociRef, error) {
	r := strings.TrimPrefix(strings.TrimSpace(ref), "oci://")
	var out ociRef
	if at := strings.LastIndex(r, "@"); at >= 0 {
		out.digest = r[at+1:]
		r = r[:at]
	}
	slash := strings.Index(r, "/")
	if slash < 0 {
		return out, fmt.Errorf("oci ref %q has no repository path", ref)
	}
	out.host = r[:slash]
	rest := r[slash+1:]
	if colon := strings.LastIndex(rest, ":"); colon >= 0 {
		out.repo, out.tag = rest[:colon], rest[colon+1:]
	} else {
		out.repo, out.tag = rest, "latest"
	}
	if out.host == "" || out.repo == "" {
		return out, fmt.Errorf("oci ref %q is incomplete", ref)
	}
	return out, nil
}

// ociClient does token-dance GETs against one registry.
type ociClient struct {
	base  string // scheme://host
	http  *http.Client
	basic string // base64 user:pass, empty = anonymous
	token string
}

func newOCIClient(base string, insecure bool, basicAuth string) *ociClient {
	tr := &http.Transport{}
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &ociClient{base: base, http: &http.Client{Transport: tr, Timeout: 30 * time.Second}, basic: basicAuth}
}

func (c *ociClient) get(ctx context.Context, path, accept string) (int, http.Header, []byte, error) {
	do := func() (int, http.Header, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return 0, nil, nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		res, err := c.http.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer res.Body.Close()
		b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		return res.StatusCode, res.Header, b, err
	}
	st, hd, body, err := do()
	if err != nil || st != http.StatusUnauthorized {
		return st, hd, body, err
	}
	// token dance: parse the quoted k="v" challenge (scope contains commas)
	kv := map[string]string{}
	for _, m := range wwwAuthKV.FindAllStringSubmatch(hd.Get("Www-Authenticate"), -1) {
		kv[m[1]] = m[2]
	}
	realm := kv["realm"]
	if realm == "" {
		return st, hd, body, nil
	}
	tu := fmt.Sprintf("%s?service=%s&scope=%s", realm, kv["service"], kv["scope"])
	treq, err := http.NewRequestWithContext(ctx, http.MethodGet, tu, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if c.basic != "" {
		treq.Header.Set("Authorization", "Basic "+c.basic)
	}
	tres, err := c.http.Do(treq)
	if err != nil {
		return 0, nil, nil, err
	}
	defer tres.Body.Close()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(tres.Body, 1<<20)).Decode(&tok); err != nil || tok.Token == "" {
		return st, hd, body, fmt.Errorf("registry token dance failed (HTTP %d from realm)", tres.StatusCode)
	}
	c.token = tok.Token
	return do()
}

// basicFromPullSecret reads a dockerconfigjson Secret and returns the
// base64 user:pass entry for host (or the first entry as fallback).
func basicFromPullSecret(ctx context.Context, c client.Client, namespace, name, host string) (string, error) {
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sec); err != nil {
		return "", fmt.Errorf("pull secret %s/%s: %w", namespace, name, err)
	}
	raw, ok := sec.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return "", fmt.Errorf("pull secret %s/%s has no %s key", namespace, name, corev1.DockerConfigJsonKey)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("pull secret %s/%s: %w", namespace, name, err)
	}
	if e, ok := cfg.Auths[host]; ok {
		if e.Auth != "" {
			return e.Auth, nil
		}
		return base64.StdEncoding.EncodeToString([]byte(e.Username + ":" + e.Password)), nil
	}
	for _, e := range cfg.Auths {
		if e.Auth != "" {
			return e.Auth, nil
		}
	}
	return "", fmt.Errorf("pull secret %s/%s has no usable auth entry", namespace, name)
}

const (
	mtOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	mtOCIIndex    = "application/vnd.oci.image.index.v1+json"
	mtDockerV2    = "application/vnd.docker.distribution.manifest.v2+json"
)

// fetchOCISkillContent resolves an oci:// artifact to its SKILL.md body:
// manifest -> single content layer -> tar(+gzip) -> SKILL.md entry.
func fetchOCISkillContent(ctx context.Context, cl *ociClient, ref ociRef) (string, error) {
	want := ref.tag
	if ref.digest != "" {
		want = ref.digest
	}
	st, _, body, err := cl.get(ctx, fmt.Sprintf("/v2/%s/manifests/%s", ref.repo, want),
		strings.Join([]string{mtOCIManifest, mtOCIIndex, mtDockerV2}, ", "))
	if err != nil {
		return "", err
	}
	if st != http.StatusOK {
		return "", fmt.Errorf("manifest %s:%s: HTTP %d", ref.repo, want, st)
	}
	var man struct {
		MediaType string `json:"mediaType"`
		Layers    []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(body, &man); err != nil {
		return "", fmt.Errorf("manifest %s:%s: %w", ref.repo, want, err)
	}
	if man.MediaType == mtOCIIndex {
		return "", fmt.Errorf("%s:%s is a pack (image index) — install its member skills, not the index itself", ref.repo, want)
	}
	if len(man.Layers) == 0 {
		return "", fmt.Errorf("manifest %s:%s has no content layer", ref.repo, want)
	}
	st, _, blob, err := cl.get(ctx, fmt.Sprintf("/v2/%s/blobs/%s", ref.repo, man.Layers[0].Digest), "")
	if err != nil {
		return "", err
	}
	if st != http.StatusOK {
		return "", fmt.Errorf("layer blob %s: HTTP %d", man.Layers[0].Digest[:19], st)
	}
	var tr *tar.Reader
	if len(blob) > 1 && blob[0] == 0x1f && blob[1] == 0x8b {
		gz, err := gzip.NewReader(strings.NewReader(string(blob)))
		if err != nil {
			return "", fmt.Errorf("layer gunzip: %w", err)
		}
		tr = tar.NewReader(gz)
	} else {
		tr = tar.NewReader(strings.NewReader(string(blob)))
	}
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("layer tar: %w", err)
		}
		name := strings.TrimPrefix(hd.Name, "./")
		if name == "SKILL.md" || strings.HasSuffix(name, "/SKILL.md") {
			b, err := io.ReadAll(io.LimitReader(tr, 2<<20))
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
	}
	return "", fmt.Errorf("artifact %s:%s carries no SKILL.md", ref.repo, want)
}
