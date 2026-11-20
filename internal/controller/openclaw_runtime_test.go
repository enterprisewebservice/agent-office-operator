/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// --- the sidecar must not touch a sign-in store the gateway owns --------

func TestCodexAuthSyncIsHandsOffFromOpenClaw2026_9_5(t *testing.T) {
	f := newCodexSyncFixture(t, []string{"new-hire", "holds-marker"})

	// holds-marker: what an agent-local copy looks like on a gateway that
	// refreshed its sign-in into the shared store: an inert marker with
	// expires 1. To the 2026.7.x seed pass that is "older than auth.json",
	// and it would be overwritten with the Secret's stale token.
	marker := map[string]interface{}{
		"type": "oauth", "provider": "openai", "email": "hire@example.test",
		"access": "openclaw-oauth-refresh-fence:v1:x", "refresh": "openclaw-oauth-refresh-fence:v1:x", "expires": 1,
	}
	f.seedExistingStore(t, "holds-marker",
		map[string]interface{}{"version": 1, "profiles": map[string]interface{}{f.pid: marker}},
		map[string]interface{}{"version": 1, "order": map[string]interface{}{"openai": []string{f.pid}}},
	)
	before := f.dump(t, "holds-marker")

	out := f.runOnce(t, map[string]string{
		"CODEX_OPENCLAW_VERSION": "2026.10.33",
		"CODEX_OPENCLAW_SDK_DIR": f.stubSDK(t),
	})

	if !strings.Contains(out, "owns its sign-in store") {
		t.Fatalf("the sidecar must say it stands down:\n%s", out)
	}
	if _, err := os.Stat(f.storePath("new-hire")); !os.IsNotExist(err) {
		t.Fatalf("a store was created for new-hire on a gateway that owns its sign-in (stat err=%v)\n%s", err, out)
	}
	if after := f.dump(t, "holds-marker"); !reflect.DeepEqual(before, after) {
		t.Fatalf("the agent store was written:\nbefore %+v\nafter  %+v\n%s", before, after, out)
	}
	// The mirror of auth.json is the one thing it still does.
	src, err := os.ReadFile(f.src)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.ReadFile(f.dst)
	if err != nil {
		t.Fatalf("auth.json was not mirrored: %v\n%s", err, out)
	}
	if string(src) != string(dst) {
		t.Fatal("mirrored auth.json differs from the Secret's")
	}
}

func TestCodexAuthSyncStillWorksBelow2026_9_5(t *testing.T) {
	for _, version := range []string{"2026.7.1", "2026.9.4"} {
		t.Run(version, func(t *testing.T) {
			f := newCodexSyncFixture(t, []string{"new-hire"})
			out := f.runOnce(t, map[string]string{
				"CODEX_OPENCLAW_VERSION": version,
				"CODEX_OPENCLAW_SDK_DIR": f.stubSDK(t),
			})
			if strings.Contains(out, "owns its sign-in store") {
				t.Fatalf("the sidecar stood down on %s:\n%s", version, out)
			}
			d := f.dump(t, "new-hire")
			if f.seededProfile(t, d, f.pid)["refresh"] != "rt-from-secret" {
				t.Fatalf("new-hire was not seeded on %s: %+v\n%s", version, d.Store, out)
			}
		})
	}
}

func TestCodexAuthSyncReadsAKeyedRoster(t *testing.T) {
	// 2026.8.1 to 2026.9.4: the roster is already agents.entries while the
	// sidecar still provisions and seeds.
	f := newCodexSyncFixture(t, nil)
	cfg, _ := json.Marshal(map[string]interface{}{"agents": map[string]interface{}{
		"ownership": "explicit",
		"entries":   map[string]interface{}{"new-hire": map[string]interface{}{"name": "n"}, "second": map[string]interface{}{}},
	}})
	if err := os.WriteFile(f.config, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	out := f.runOnce(t, map[string]string{
		"CODEX_OPENCLAW_VERSION": "2026.8.5",
		"CODEX_OPENCLAW_SDK_DIR": f.stubSDK(t),
	})
	for _, id := range []string{"new-hire", "second"} {
		if _, err := os.Stat(f.storePath(id)); err != nil {
			t.Fatalf("agent %s from agents.entries was not provisioned: %v\n%s", id, err, out)
		}
	}
}

// --- browser node pin ----------------------------------------------------

func browserNode(t *testing.T, cfg map[string]interface{}) map[string]interface{} {
	t.Helper()
	gw, _ := cfg["gateway"].(map[string]interface{})
	nodes, _ := gw["nodes"].(map[string]interface{})
	b, _ := nodes["browser"].(map[string]interface{})
	return b
}

func TestConvergeScriptPinsTheBrowserNode(t *testing.T) {
	mustNode(t)
	script := extractConvergeScript(t)
	desired := subscriptionDesired()
	desired.BrowserNode = "fedora-black-zebra-36-newsroom"

	// A config where someone chose a browser mode by hand: the mode stays.
	cfg := map[string]interface{}{"gateway": map[string]interface{}{
		"nodes": map[string]interface{}{"browser": map[string]interface{}{"mode": "manual"}},
	}}
	got, marker := runConverge(t, script, cfg, desired)
	if !strings.Contains(marker, "RECONCILED_MODEL_AUTH") {
		t.Fatalf("first pass: %s", marker)
	}
	b := browserNode(t, got)
	if b["node"] != "fedora-black-zebra-36-newsroom" || b["mode"] != "manual" {
		t.Fatalf("gateway.nodes.browser = %v", b)
	}

	got2, marker := runConverge(t, script, got, desired)
	if marker != "NO_CHANGE" {
		t.Fatalf("second pass must be a no-op (a change restarts the gateway): %s", marker)
	}
	if !reflect.DeepEqual(got, got2) {
		t.Fatal("second pass changed the file")
	}

	// No nodeHostRef: an existing pin is not the operator's to remove.
	desired.BrowserNode = ""
	got3, _ := runConverge(t, script, got2, desired)
	if browserNode(t, got3)["node"] != "fedora-black-zebra-36-newsroom" {
		t.Fatalf("pin removed although the spec said nothing about it: %v", browserNode(t, got3))
	}
}

// --- scheduling preference ----------------------------------------------

func TestPreferWorkerNodesIsAPreferenceNotARequirement(t *testing.T) {
	a := preferWorkerNodes()
	if a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Fatal("a required rule would leave a compact (control-plane only) cluster with no node to run on")
	}
	terms := a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(terms) != 1 || len(terms[0].Preference.MatchExpressions) != 2 {
		t.Fatalf("unexpected preference: %+v", terms)
	}
	for _, e := range terms[0].Preference.MatchExpressions {
		if e.Operator != corev1.NodeSelectorOpDoesNotExist {
			t.Fatalf("expression %+v must be DoesNotExist", e)
		}
	}
}
