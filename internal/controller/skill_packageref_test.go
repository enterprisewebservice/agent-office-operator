package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

func TestParsePackageCoordinate(t *testing.T) {
	for _, tc := range []struct {
		in, name, ver string
		wantErr       bool
	}{
		{"platform-incident-triage:1.1.0", "platform-incident-triage", "1.1.0", false},
		{"agent-office/platform-incident-triage:1.1.0", "platform-incident-triage", "1.1.0", false},
		{"no-version", "", "", true},
		{":1.0.0", "", "", true},
	} {
		n, v, err := parsePackageCoordinate(tc.in)
		if tc.wantErr != (err != nil) {
			t.Fatalf("%q: err=%v", tc.in, err)
		}
		if !tc.wantErr && (n != tc.name || v != tc.ver) {
			t.Fatalf("%q: got %s %s", tc.in, n, v)
		}
	}
}

func TestResolvePackageRefDigest(t *testing.T) {
	body := "---\nname: x\n---\n# X\n"
	sum := sha256.Sum256([]byte(body))
	good := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/mindifact.json"):
			fmt.Fprintf(w, `{"content": "/x/1.0.0/skills/x.md"}`)
		case strings.HasSuffix(r.URL.Path, "/skills/x.md"):
			fmt.Fprint(w, body)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	// digest match resolves; sha256: prefix accepted
	got, err := resolvePackageRef(context.Background(), nil, "", &agentofficev1alpha1.PackageRefSource{
		Ref: "x:1.0.0", Registry: srv.URL, Digest: "sha256:" + good,
	})
	if err != nil || got != body {
		t.Fatalf("pinned resolve: err=%v got=%q", err, got)
	}

	// digest mismatch refuses delivery
	if _, err := resolvePackageRef(context.Background(), nil, "", &agentofficev1alpha1.PackageRefSource{
		Ref: "x:1.0.0", Registry: srv.URL, Digest: strings.Repeat("0", 64),
	}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("want digest refusal, got %v", err)
	}

	// pinned entries survive the server going away (immutable cache)
	srv.Close()
	if got, err := resolvePackageRef(context.Background(), nil, "", &agentofficev1alpha1.PackageRefSource{
		Ref: "x:1.0.0", Registry: srv.URL, Digest: good,
	}); err != nil || got != body {
		t.Fatalf("cached pinned resolve after server close: err=%v", err)
	}
}

func TestSkillDeclaresSource(t *testing.T) {
	mk := func(src agentofficev1alpha1.SkillSource) *agentofficev1alpha1.Skill {
		return &agentofficev1alpha1.Skill{Spec: agentofficev1alpha1.SkillSpec{Source: src}}
	}
	if skillDeclaresSource(mk(agentofficev1alpha1.SkillSource{})) {
		t.Fatal("pure-template skill must not count as declaring a source")
	}
	if !skillDeclaresSource(mk(agentofficev1alpha1.SkillSource{Inline: "x"})) {
		t.Fatal("inline counts")
	}
	if !skillDeclaresSource(mk(agentofficev1alpha1.SkillSource{PackageRef: &agentofficev1alpha1.PackageRefSource{Ref: "x:1"}})) {
		t.Fatal("packageRef counts")
	}
	if !skillDeclaresSource(mk(agentofficev1alpha1.SkillSource{ConfigMapRef: &agentofficev1alpha1.ConfigMapSource{Name: "c", Key: "k"}})) {
		t.Fatal("configMapRef counts")
	}
}

func TestParseOCIRef(t *testing.T) {
	r, err := parseOCIRef("oci://quay.example.com/org/agent-office-skill-x:1.1.0")
	if err != nil || r.host != "quay.example.com" || r.repo != "org/agent-office-skill-x" || r.tag != "1.1.0" || r.digest != "" {
		t.Fatalf("got %+v err=%v", r, err)
	}
	r, err = parseOCIRef("oci://q/o/r:1.0.0@sha256:abc")
	if err != nil || r.digest != "sha256:abc" || r.tag != "1.0.0" {
		t.Fatalf("digest pin: %+v err=%v", r, err)
	}
	if _, err := parseOCIRef("oci://hostonly"); err == nil {
		t.Fatal("want error for missing repo path")
	}
}

func ociTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(body)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestResolvePackageRefOCI(t *testing.T) {
	skillBody := "---\nname: x\n---\n# X via OCI\n"
	layer := ociTarGz(t, map[string]string{"SKILL.md": skillBody})
	layerDigest := "sha256:" + func() string { s := sha256.Sum256(layer); return hex.EncodeToString(s[:]) }()
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"%s","config":{"mediaType":"application/vnd.agentskills.skill.config.v1+json","digest":"sha256:0","size":2},"layers":[{"mediaType":"application/vnd.agentskills.skill.content.v1.tar+gzip","digest":"%s","size":%d}]}`, mtOCIManifest, layerDigest, len(layer))
	index := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"%s","manifests":[]}`, mtOCIIndex)

	authed := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			// like quay: anonymous token requests succeed too
			if r.Header.Get("Authorization") == "Basic dTpw" { // u:p
				authed = true
			}
			fmt.Fprint(w, `{"token":"tok123"}`)
		case strings.Contains(r.URL.Path, "/manifests/"):
			if r.Header.Get("Authorization") != "Bearer tok123" {
				w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="https://%s/token",service="reg",scope="repository:org/x:pull,push"`, r.Host))
				w.WriteHeader(401)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/pack") {
				w.Header().Set("Content-Type", mtOCIIndex)
				fmt.Fprint(w, index)
				return
			}
			w.Header().Set("Content-Type", mtOCIManifest)
			fmt.Fprint(w, manifest)
		case strings.Contains(r.URL.Path, "/blobs/"):
			w.Write(layer)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")

	sum := sha256.Sum256([]byte(skillBody))
	good := hex.EncodeToString(sum[:])

	// full path: token dance with basic auth, tar extraction, digest verify.
	// PullSecretName is exercised via the direct basic seam (cluster client
	// paths are covered by envtest elsewhere); here auth comes in preseeded.
	cl := newOCIClient(srv.URL, true, "dTpw")
	body, err := fetchOCISkillContent(context.Background(), cl, ociRef{host: host, repo: "org/x", tag: "1.0.0"})
	if err != nil || body != skillBody || !authed {
		t.Fatalf("fetch: err=%v authed=%v", err, authed)
	}

	src := &agentofficev1alpha1.PackageRefSource{Ref: "oci://" + host + "/org/x:1.0.0", Digest: good, InsecureSkipTLSVerify: true}
	got, err := resolvePackageRef(context.Background(), nil, "", src)
	if err != nil || got != skillBody {
		t.Fatalf("resolve: err=%v", err)
	}

	// tamper: wrong digest refused
	bad := &agentofficev1alpha1.PackageRefSource{Ref: "oci://" + host + "/org/x2:1.0.0", Digest: strings.Repeat("a", 64), InsecureSkipTLSVerify: true}
	if _, err := resolvePackageRef(context.Background(), nil, "", bad); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("want digest refusal, got %v", err)
	}

	// a pack index is not installable as a skill
	cl2 := newOCIClient(srv.URL, true, "dTpw")
	if _, err := fetchOCISkillContent(context.Background(), cl2, ociRef{host: host, repo: "org/x", tag: "pack"}); err == nil || !strings.Contains(err.Error(), "image index") {
		t.Fatalf("want index rejection, got %v", err)
	}
}
