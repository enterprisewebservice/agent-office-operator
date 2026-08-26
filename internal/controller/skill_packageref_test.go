package controller

import (
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
	for _, tc := range []struct{ in, name, ver string; wantErr bool }{
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
	got, err := resolvePackageRef(context.Background(), &agentofficev1alpha1.PackageRefSource{
		Ref: "x:1.0.0", Registry: srv.URL, Digest: "sha256:" + good,
	})
	if err != nil || got != body {
		t.Fatalf("pinned resolve: err=%v got=%q", err, got)
	}

	// digest mismatch refuses delivery
	if _, err := resolvePackageRef(context.Background(), &agentofficev1alpha1.PackageRefSource{
		Ref: "x:1.0.0", Registry: srv.URL, Digest: strings.Repeat("0", 64),
	}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("want digest refusal, got %v", err)
	}

	// pinned entries survive the server going away (immutable cache)
	srv.Close()
	if got, err := resolvePackageRef(context.Background(), &agentofficev1alpha1.PackageRefSource{
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
