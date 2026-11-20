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
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These tests run the SHIPPED in-pod scripts with plain node against
// fixture configs. The runtime version the scripts would read from
// /app/package.json is supplied through OPENCLAW_VERSION_OVERRIDE.

const (
	ocLegacy = "2026.7.1"   // roster = agents.list
	ocKeyed  = "2026.10.33" // roster = agents.entries
)

const rosterConfigPathLine = `const CONFIG_PATH = "/home/node/.openclaw/openclaw.json";`

// runRosterScript runs helpers+script on cfg and returns stdout plus the
// config as it is on disk afterwards.
func runRosterScript(t *testing.T, script, version string, cfg map[string]interface{}, args ...string) (string, map[string]interface{}) {
	t.Helper()
	mustNode(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	redirected := strings.Replace(script, rosterConfigPathLine, `const CONFIG_PATH = process.env.TEST_CFG_PATH;`, 1)
	if redirected == script {
		t.Fatal("could not redirect the config path; the script's CONFIG_PATH line changed")
	}
	redirected = strings.Replace(redirected, `const WORKSPACES_DIR = "/home/node/.openclaw/workspaces/";`,
		`const WORKSPACES_DIR = process.env.TEST_WS_DIR + "/";`, 1)
	cmd := exec.Command("node", append([]string{"-e", rosterHelpersJS + redirected}, args...)...)
	cmd.Env = []string{
		"TEST_CFG_PATH=" + cfgPath,
		"TEST_WS_DIR=" + filepath.Join(dir, "workspaces"),
		"OPENCLAW_VERSION_OVERRIDE=" + version,
		"PATH=" + os.Getenv("PATH"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(after, &got); err != nil {
		t.Fatalf("config is not JSON after the script: %v\n%s", err, after)
	}
	return strings.TrimSpace(string(out)), got
}

func mergeArgs(t *testing.T, p agentMergeParams) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return string(b)
}

func helpDeskParams() agentMergeParams {
	return agentMergeParams{
		AgentEntry: map[string]interface{}{
			"id":        "runtab-help",
			"name":      "Runtab help desk",
			"workspace": "/home/node/.openclaw/workspaces/runtab-help",
			"model":     map[string]interface{}{"primary": "openai/gpt-6.2-sol"},
			"identity":  map[string]interface{}{"name": "Runtab help desk", "emoji": "🛟"},
		},
		Profile:        "runtab-help",
		AgentID:        "runtab-help",
		DeclaredTools:  []string{"web_fetch"},
		SearchProvider: "duckduckgo",
	}
}

// communityPersona is an entry set by hand on the gateway: it has no
// AgentWorkstation and must come through every operator pass untouched.
func communityPersona(withID bool) map[string]interface{} {
	e := map[string]interface{}{
		"name":      "Runtab Help",
		"workspace": "/home/node/.openclaw/workspaces/runtab-community",
		"model":     map[string]interface{}{"primary": "openai/gpt-6.2-sol"},
		"tools": map[string]interface{}{
			"allow": []interface{}{"governed__runtab_knowledge", "governed__runtab_release", "memory_get"},
		},
	}
	if withID {
		e["id"] = "runtab-community"
	}
	return e
}

func guildBinding() map[string]interface{} {
	return map[string]interface{}{
		"agentId": "runtab-community",
		"match":   map[string]interface{}{"channel": "discord", "accountId": "default", "guildId": "1573065900876111872"},
	}
}

func agentsOf(t *testing.T, cfg map[string]interface{}) map[string]interface{} {
	t.Helper()
	a, ok := cfg["agents"].(map[string]interface{})
	if !ok {
		t.Fatalf("config has no agents object: %v", cfg)
	}
	return a
}

// --- 2026.7.x: the array roster must behave exactly as before ---------

func TestAgentMerge_LegacyRosterInsertUpdateAndNoChange(t *testing.T) {
	cfg := map[string]interface{}{
		"agents":   map[string]interface{}{"defaults": map[string]interface{}{"model": map[string]interface{}{"primary": "openai/gpt-5.6-sol"}}, "list": []interface{}{communityPersona(true)}},
		"bindings": []interface{}{guildBinding()},
	}
	out, got := runRosterScript(t, agentMergeScript, ocLegacy, cfg, mergeArgs(t, helpDeskParams()))
	if !strings.HasPrefix(out, "MERGED agent=runtab-help") || !strings.Contains(out, "roster=list") {
		t.Fatalf("first pass: %s", out)
	}
	a := agentsOf(t, got)
	if _, keyed := a["entries"]; keyed {
		t.Fatalf("2026.7.x must not get agents.entries: %v", a)
	}
	list := a["list"].([]interface{})
	if len(list) != 2 {
		t.Fatalf("want 2 agents in list, got %d: %v", len(list), list)
	}
	if !reflect.DeepEqual(list[0], communityPersona(true)) {
		t.Fatalf("hand-made persona changed:\n got %v\nwant %v", list[0], communityPersona(true))
	}
	if list[1].(map[string]interface{})["id"] != "runtab-help" {
		t.Fatalf("operator entry not appended with its id: %v", list[1])
	}
	if _, has := a["ownership"]; has {
		t.Fatalf("2026.7.x must not get agents.ownership: %v", a)
	}
	if !reflect.DeepEqual(got["bindings"], []interface{}{guildBinding()}) {
		t.Fatalf("hand-made guild binding changed: %v", got["bindings"])
	}

	out, got2 := runRosterScript(t, agentMergeScript, ocLegacy, got, mergeArgs(t, helpDeskParams()))
	if !strings.HasPrefix(out, "NO_CHANGE agent=runtab-help") {
		t.Fatalf("second pass must be a no-op: %s", out)
	}
	if !reflect.DeepEqual(got, got2) {
		t.Fatal("second pass changed the file")
	}

	p := helpDeskParams()
	p.AgentEntry["model"] = map[string]interface{}{"primary": "openai/gpt-5.6-sol"}
	out, got3 := runRosterScript(t, agentMergeScript, ocLegacy, got2, mergeArgs(t, p))
	if !strings.HasPrefix(out, "MERGED") {
		t.Fatalf("model change must be merged: %s", out)
	}
	e := agentsOf(t, got3)["list"].([]interface{})[1].(map[string]interface{})
	if e["model"].(map[string]interface{})["primary"] != "openai/gpt-5.6-sol" {
		t.Fatalf("model not updated: %v", e)
	}
}

// --- 2026.8.1+: the keyed roster --------------------------------------

func TestAgentMerge_KeyedRosterOnFreshConfig(t *testing.T) {
	// A seed config with no roster at all.
	cfg := map[string]interface{}{
		"agents":   map[string]interface{}{"defaults": map[string]interface{}{"model": map[string]interface{}{"primary": "openai/gpt-6.2-sol"}}},
		"bindings": []interface{}{},
	}
	out, got := runRosterScript(t, agentMergeScript, ocKeyed, cfg, mergeArgs(t, helpDeskParams()))
	if !strings.HasPrefix(out, "MERGED") || !strings.Contains(out, "roster=entries") {
		t.Fatalf("first pass: %s", out)
	}
	a := agentsOf(t, got)
	if _, has := a["list"]; has {
		t.Fatalf("a keyed roster must not carry agents.list (its presence alone is refused): %v", a)
	}
	entries := a["entries"].(map[string]interface{})
	e, ok := entries["runtab-help"].(map[string]interface{})
	if !ok {
		t.Fatalf("agent missing from agents.entries: %v", entries)
	}
	if _, has := e["id"]; has {
		t.Fatalf("an entry in agents.entries must not carry id (unknown key): %v", e)
	}
	if e["workspace"] != "/home/node/.openclaw/workspaces/runtab-help" {
		t.Fatalf("workspace lost: %v", e)
	}
	if _, has := a["ownership"]; has {
		t.Fatalf("one agent needs no ownership stamp: %v", a)
	}

	out, got2 := runRosterScript(t, agentMergeScript, ocKeyed, got, mergeArgs(t, helpDeskParams()))
	if !strings.HasPrefix(out, "NO_CHANGE") {
		t.Fatalf("second pass must be a no-op: %s", out)
	}
	if !reflect.DeepEqual(got, got2) {
		t.Fatal("second pass changed the file")
	}
}

func TestAgentMerge_KeyedRosterSecondAgentStampsOwnership(t *testing.T) {
	cfg := map[string]interface{}{
		"agents": map[string]interface{}{"entries": map[string]interface{}{"runtab-community": communityPersona(false)}},
	}
	_, got := runRosterScript(t, agentMergeScript, ocKeyed, cfg, mergeArgs(t, helpDeskParams()))
	a := agentsOf(t, got)
	if a["ownership"] != "explicit" {
		t.Fatalf("two agents require agents.ownership=explicit, got %v", a["ownership"])
	}
	entries := a["entries"].(map[string]interface{})
	if !reflect.DeepEqual(entries["runtab-community"], communityPersona(false)) {
		t.Fatalf("hand-made persona changed:\n got %v\nwant %v", entries["runtab-community"], communityPersona(false))
	}
}

func TestAgentMerge_KeyedRosterIsUsedOnceMigratedWhateverTheVersion(t *testing.T) {
	// agents.entries present = already migrated. Even with an unknown
	// runtime version the script must not write a legacy list next to it.
	cfg := map[string]interface{}{
		"agents": map[string]interface{}{"ownership": "explicit", "entries": map[string]interface{}{
			"runtab-community": communityPersona(false),
			"runtab-help":      map[string]interface{}{"name": "old name", "workspace": "/home/node/.openclaw/workspaces/runtab-help"},
		}},
	}
	out, got := runRosterScript(t, agentMergeScript, "", cfg, mergeArgs(t, helpDeskParams()))
	if !strings.Contains(out, "roster=entries") {
		t.Fatalf("want keyed roster: %s", out)
	}
	a := agentsOf(t, got)
	if _, has := a["list"]; has {
		t.Fatalf("legacy list written next to agents.entries: %v", a)
	}
	if a["entries"].(map[string]interface{})["runtab-help"].(map[string]interface{})["name"] != "Runtab help desk" {
		t.Fatalf("entry not updated: %v", a["entries"])
	}
}

func TestAgentMerge_KeyedRosterFoldsInAndDropsAStrayList(t *testing.T) {
	// What an older operator would have left behind on a migrated gateway:
	// agents.list beside agents.entries. Nothing may be lost, and the key
	// must go.
	stray := map[string]interface{}{"id": "late-arrival", "name": "Late", "workspace": "/home/node/.openclaw/workspaces/late-arrival"}
	cfg := map[string]interface{}{
		"agents": map[string]interface{}{
			"entries": map[string]interface{}{"runtab-community": communityPersona(false)},
			"list":    []interface{}{stray, communityPersona(true)},
		},
	}
	_, got := runRosterScript(t, agentMergeScript, ocKeyed, cfg, mergeArgs(t, helpDeskParams()))
	a := agentsOf(t, got)
	if _, has := a["list"]; has {
		t.Fatalf("agents.list survived: %v", a)
	}
	entries := a["entries"].(map[string]interface{})
	for _, id := range []string{"runtab-community", "late-arrival", "runtab-help"} {
		if _, ok := entries[id]; !ok {
			t.Fatalf("agent %s lost: %v", id, entries)
		}
	}
	if _, has := entries["late-arrival"].(map[string]interface{})["id"]; has {
		t.Fatalf("folded-in entry kept its id: %v", entries["late-arrival"])
	}
	if !reflect.DeepEqual(entries["runtab-community"], communityPersona(false)) {
		t.Fatal("an entry already in agents.entries must win over the list copy")
	}
}

func TestAgentMerge_EmptyLegacyListIsRemovedOnAKeyedRuntime(t *testing.T) {
	// The pre-v1.7.86 seed config carried `"list": []`.
	cfg := map[string]interface{}{"agents": map[string]interface{}{"list": []interface{}{}}}
	_, got := runRosterScript(t, agentMergeScript, ocKeyed, cfg, mergeArgs(t, helpDeskParams()))
	a := agentsOf(t, got)
	if _, has := a["list"]; has {
		t.Fatalf("empty agents.list survived on a keyed runtime: %v", a)
	}
	if _, ok := a["entries"].(map[string]interface{})["runtab-help"]; !ok {
		t.Fatalf("agent not registered: %v", a)
	}
}

func TestAgentMerge_PopulatedLegacyListIsLeftForDoctor(t *testing.T) {
	// A populated list on a new runtime means Doctor has not migrated yet.
	// Doctor's migration also moves bindings and owners, so the operator
	// must not pre-empt it with a half migration.
	cfg := map[string]interface{}{"agents": map[string]interface{}{"list": []interface{}{communityPersona(true)}}}
	out, got := runRosterScript(t, agentMergeScript, ocKeyed, cfg, mergeArgs(t, helpDeskParams()))
	if !strings.Contains(out, "roster=list") {
		t.Fatalf("want the legacy list kept: %s", out)
	}
	if _, has := agentsOf(t, got)["entries"]; has {
		t.Fatalf("agents.entries created beside a populated legacy list: %v", got)
	}
}

// --- spec.tools.restrictTo ---------------------------------------------

func TestAgentMerge_RestrictToRendersExactlyThatObject(t *testing.T) {
	for _, version := range []string{ocLegacy, ocKeyed} {
		t.Run(version, func(t *testing.T) {
			cfg := map[string]interface{}{}
			p := helpDeskParams()
			p.RestrictTo = []string{"read", "governed__runtab_account", "governed__runtab_release"}
			_, got := runRosterScript(t, agentMergeScript, version, cfg, mergeArgs(t, p))
			want := map[string]interface{}{
				"allow": []interface{}{"read", "governed__runtab_account", "governed__runtab_release"},
				"fs":    map[string]interface{}{"workspaceOnly": true},
			}
			e := rosterEntry(t, got, "runtab-help")
			if !reflect.DeepEqual(e["tools"], want) {
				t.Fatalf("tools:\n got %v\nwant %v", e["tools"], want)
			}
			out, got2 := runRosterScript(t, agentMergeScript, version, got, mergeArgs(t, p))
			if !strings.HasPrefix(out, "NO_CHANGE") {
				t.Fatalf("second pass must be a no-op: %s", out)
			}
			if !reflect.DeepEqual(got, got2) {
				t.Fatal("second pass changed the file")
			}
		})
	}
}

func TestAgentMerge_RestrictToReplacesAHandSetToolsObject(t *testing.T) {
	// The operator owns the WHOLE object while the field is set: a leftover
	// alsoAllow next to allow makes OpenClaw reject the entire config.
	cfg := map[string]interface{}{"agents": map[string]interface{}{"list": []interface{}{
		map[string]interface{}{"id": "runtab-help", "tools": map[string]interface{}{"alsoAllow": []interface{}{"browser"}, "allow": []interface{}{"exec"}}},
	}}}
	p := helpDeskParams()
	p.RestrictTo = []string{"read"}
	_, got := runRosterScript(t, agentMergeScript, ocLegacy, cfg, mergeArgs(t, p))
	want := map[string]interface{}{"allow": []interface{}{"read"}, "fs": map[string]interface{}{"workspaceOnly": true}}
	if e := rosterEntry(t, got, "runtab-help"); !reflect.DeepEqual(e["tools"], want) {
		t.Fatalf("tools:\n got %v\nwant %v", e["tools"], want)
	}
}

func TestAgentMerge_RemovedRestrictToDeletesOnlyWhatTheOperatorRendered(t *testing.T) {
	for _, version := range []string{ocLegacy, ocKeyed} {
		t.Run(version, func(t *testing.T) {
			p := helpDeskParams()
			p.RestrictTo = []string{"read"}
			_, rendered := runRosterScript(t, agentMergeScript, version, map[string]interface{}{}, mergeArgs(t, p))

			// Field removed after the operator rendered it: the key goes.
			p = helpDeskParams()
			p.RemoveRenderedTools = true
			_, got := runRosterScript(t, agentMergeScript, version, rendered, mergeArgs(t, p))
			if _, has := rosterEntry(t, got, "runtab-help")["tools"]; has {
				t.Fatalf("rendered tools object not removed: %v", rosterEntry(t, got, "runtab-help"))
			}

			// Field never set, tools set by hand: untouched.
			handSet := map[string]interface{}{"allow": []interface{}{"governed__runtab_release"}}
			p = helpDeskParams()
			_, base := runRosterScript(t, agentMergeScript, version, map[string]interface{}{}, mergeArgs(t, p))
			rosterEntry(t, base, "runtab-help")["tools"] = handSet
			_, got = runRosterScript(t, agentMergeScript, version, base, mergeArgs(t, p))
			if !reflect.DeepEqual(rosterEntry(t, got, "runtab-help")["tools"], handSet) {
				t.Fatalf("hand-set tools changed although restrictTo was never set: %v", rosterEntry(t, got, "runtab-help")["tools"])
			}
		})
	}
}

func TestAgentMerge_SiblingWithoutAWorkstationIsUntouchedByRestrictTo(t *testing.T) {
	cfg := map[string]interface{}{
		"agents":   map[string]interface{}{"ownership": "explicit", "entries": map[string]interface{}{"runtab-community": communityPersona(false)}},
		"bindings": []interface{}{guildBinding()},
	}
	p := helpDeskParams()
	p.RestrictTo = []string{"read", "governed__runtab_account"}
	_, got := runRosterScript(t, agentMergeScript, ocKeyed, cfg, mergeArgs(t, p))
	if !reflect.DeepEqual(rosterEntry(t, got, "runtab-community"), communityPersona(false)) {
		t.Fatalf("hand-made sibling changed: %v", rosterEntry(t, got, "runtab-community"))
	}
	if !reflect.DeepEqual(got["bindings"], []interface{}{guildBinding()}) {
		t.Fatalf("hand-made guild binding changed: %v", got["bindings"])
	}
}

// rosterEntry returns the agent's entry from whichever roster format the
// config uses.
func rosterEntry(t *testing.T, cfg map[string]interface{}, id string) map[string]interface{} {
	t.Helper()
	a := agentsOf(t, cfg)
	if entries, ok := a["entries"].(map[string]interface{}); ok {
		e, ok := entries[id].(map[string]interface{})
		if !ok {
			t.Fatalf("agent %s not in agents.entries: %v", id, entries)
		}
		return e
	}
	list, _ := a["list"].([]interface{})
	for _, x := range list {
		if e, ok := x.(map[string]interface{}); ok && e["id"] == id {
			return e
		}
	}
	t.Fatalf("agent %s not in the roster: %v", id, a)
	return nil
}

// --- de-register --------------------------------------------------------

func TestAgentDeregister_LegacyAndKeyed(t *testing.T) {
	binding := map[string]interface{}{
		"agentId": "runtab-help",
		"match":   map[string]interface{}{"channel": "discord", "accountId": "default", "peer": map[string]interface{}{"kind": "channel", "id": "1"}},
	}
	legacy := map[string]interface{}{
		"agents":   map[string]interface{}{"list": []interface{}{communityPersona(true), map[string]interface{}{"id": "runtab-help"}}},
		"nodeHost": map[string]interface{}{"browserProxy": map[string]interface{}{"enabled": true, "allowProfiles": []interface{}{"runtab-help", "other"}}},
		"bindings": []interface{}{guildBinding(), binding},
	}
	out, got := runRosterScript(t, agentDeregisterScript, ocLegacy, legacy, "runtab-help", "runtab-help")
	if out != "de-registered runtab-help" {
		t.Fatalf("legacy: %s", out)
	}
	if l := agentsOf(t, got)["list"].([]interface{}); len(l) != 1 || !reflect.DeepEqual(l[0], communityPersona(true)) {
		t.Fatalf("legacy list after removal: %v", l)
	}
	if !reflect.DeepEqual(got["bindings"], []interface{}{guildBinding()}) {
		t.Fatalf("bindings after removal: %v", got["bindings"])
	}
	if p := got["nodeHost"].(map[string]interface{})["browserProxy"].(map[string]interface{})["allowProfiles"]; !reflect.DeepEqual(p, []interface{}{"other"}) {
		t.Fatalf("allowProfiles after removal: %v", p)
	}

	keyed := map[string]interface{}{
		"agents": map[string]interface{}{"ownership": "explicit", "entries": map[string]interface{}{
			"runtab-community": communityPersona(false), "runtab-help": map[string]interface{}{"name": "x"},
		}},
		"bindings": []interface{}{guildBinding(), binding},
	}
	_, got = runRosterScript(t, agentDeregisterScript, ocKeyed, keyed, "runtab-help", "runtab-help")
	entries := agentsOf(t, got)["entries"].(map[string]interface{})
	if len(entries) != 1 || !reflect.DeepEqual(entries["runtab-community"], communityPersona(false)) {
		t.Fatalf("keyed roster after removal: %v", entries)
	}

	// Second run: nothing left to do.
	out, _ = runRosterScript(t, agentDeregisterScript, ocKeyed, got, "runtab-help", "runtab-help")
	if out != "noop (already absent)" {
		t.Fatalf("second removal: %s", out)
	}
}

func TestAgentDeregister_LastKeyedAgentRemovesTheRoster(t *testing.T) {
	// "agents.entries must contain at least one configured agent": an
	// emptied keyed roster has to disappear, not stay as {}.
	cfg := map[string]interface{}{"agents": map[string]interface{}{
		"defaults": map[string]interface{}{"model": map[string]interface{}{"primary": "openai/gpt-6.2-sol"}},
		"entries":  map[string]interface{}{"runtab-help": map[string]interface{}{"name": "x"}},
	}}
	_, got := runRosterScript(t, agentDeregisterScript, ocKeyed, cfg, "runtab-help", "runtab-help")
	a := agentsOf(t, got)
	if _, has := a["entries"]; has {
		t.Fatalf("empty agents.entries left behind: %v", a)
	}
	if _, has := a["defaults"]; !has {
		t.Fatalf("agents.defaults lost: %v", a)
	}
}

// --- version parsing ----------------------------------------------------

func TestParseOpenClawVersion(t *testing.T) {
	cases := []struct {
		in          string
		ok          bool
		keyed, owns bool
	}{
		{"2026.7.1", true, false, false},
		{"2026.7.1-2", true, false, false},
		{"v2026.7.35", true, false, false},
		{"2026.8.1", true, true, false},
		{"2026.9.4", true, true, false},
		{"2026.9.5", true, true, true},
		{"2026.10.33", true, true, true},
		{"2026.11.10-beta.1", true, true, true},
		{"2027.1.1", true, true, true},
		{"latest", false, false, false},
		{"", false, false, false},
	}
	for _, c := range cases {
		v, ok := parseOpenClawVersion(c.in)
		if ok != c.ok {
			t.Fatalf("%q: parsed=%v want %v", c.in, ok, c.ok)
		}
		if v.keyedRoster() != c.keyed || v.gatewayOwnsAuthStore() != c.owns {
			t.Fatalf("%q: keyed=%v owns=%v, want %v %v", c.in, v.keyedRoster(), v.gatewayOwnsAuthStore(), c.keyed, c.owns)
		}
	}
	if (openclawVersion{}).AtLeast(2000, 1, 1) {
		t.Fatal("an unknown version must not pass any gate")
	}
}

func TestResolveOpenClawVersion_ReadsTheLastLine(t *testing.T) {
	// The runtime may print a warning before the answer.
	v := resolveOpenClawVersion(nil, func([]string) (string, error) {
		return "Config: missing env var \"X\"\n2026.10.33\n", nil
	})
	if v.String() != "2026.10.33" || !v.keyedRoster() {
		t.Fatalf("got %v", v)
	}
}

// --- retry after a failed tool-server write -------------------------------

func TestAgentRequeueIntervalIsShortWhileToolServersArePending(t *testing.T) {
	if got := agentRequeueInterval(false); got != agentReconcileInterval {
		t.Fatalf("ordinary pass: got %s, want %s", got, agentReconcileInterval)
	}
	got := agentRequeueInterval(true)
	if got != mcpRetryInterval {
		t.Fatalf("pending tool servers: got %s, want %s", got, mcpRetryInterval)
	}
	// The point of the retry: a gateway that restarted under the write
	// must not serve the previous credential until the next ordinary pass.
	if got >= time.Minute || got >= agentReconcileInterval {
		t.Fatalf("retry after a failed tool-server write is %s; it must be well under a minute", got)
	}
}

func TestAgentRunningMessageKeepsThePendingToolServerNote(t *testing.T) {
	plain := agentRunningMessage("runtab-help", "runtab-help", "no Discord binding", "")
	if strings.Contains(plain, "pending") {
		t.Fatalf("nothing is pending, yet the message says so: %q", plain)
	}
	pending := agentRunningMessage("runtab-help", "runtab-help", "no Discord binding", "compare mcp servers: container not found")
	if !strings.HasPrefix(pending, plain) || !strings.Contains(pending, "MCP setup pending: compare mcp servers") {
		t.Fatalf("the pending note was lost: %q", pending)
	}
}
