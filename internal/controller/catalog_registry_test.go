package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestRegistryFederation: artifacts a registry publishes are searchable
// before they are installed — the entire reason a registry exists — and
// a registry outage never darkens the local catalog.
func TestRegistryFederation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"registry":"mindifact.ai","apiVersion":"v1","artifacts":[
		 {"namespace":"meshforge","name":"parkforge-terrain-strokes","version":"1.6.0","kind":"skill",
		  "description":"Edit the ground in the Unreal park","manifest":"/v1/x/1.6.0/mindifact.json",
		  "content":"/v1/x/1.6.0/skills/terrain-strokes.md",
		  "requires":[{"kind":"mcpServer","name":"unreal"}]}]}`)
	}))
	defer srv.Close()

	t.Setenv("AGENT_REGISTRY_URLS", srv.URL+"/v1/index.json")
	remoteRegistry.fetched = time.Time{}
	remoteRegistry.packs = nil

	got := registryPacks(context.Background())
	if len(got) != 1 {
		t.Fatalf("want 1 federated pack, got %d", len(got))
	}
	p := got[0]
	if p.Installed {
		t.Errorf("remote artifact must be marked not installed: %+v", p)
	}
	if p.Registry != "mindifact.ai" || p.Namespace != "meshforge" {
		t.Errorf("coordinates lost: %+v", p)
	}
	if len(p.Dependencies) != 1 || p.Dependencies[0].Name != "unreal" {
		t.Errorf("requires not surfaced as a dependency: %+v", p.Dependencies)
	}
	if p.Manifest != srv.URL+"/v1/x/1.6.0/mindifact.json" {
		t.Errorf("manifest not absolutized: %q", p.Manifest)
	}

	// unset ⇒ no federation, catalog behaves as before
	os.Unsetenv("AGENT_REGISTRY_URLS")
	remoteRegistry.fetched = time.Time{}
	remoteRegistry.packs = nil
	if n := len(registryPacks(context.Background())); n != 0 {
		t.Errorf("no registry configured should federate nothing, got %d", n)
	}
}

// TestRegistryOutageServesLastGood: a dead registry must not empty the
// composer's list mid-session.
func TestRegistryOutageServesLastGood(t *testing.T) {
	t.Setenv("AGENT_REGISTRY_URLS", "http://127.0.0.1:1/v1/index.json")
	remoteRegistry.fetched = time.Time{}
	remoteRegistry.packs = []catalogPack{{Name: "cached", Registry: "mindifact.ai"}}
	got := registryPacks(context.Background())
	if len(got) != 1 || got[0].Name != "cached" {
		t.Errorf("outage should serve the last good copy, got %+v", got)
	}
}
