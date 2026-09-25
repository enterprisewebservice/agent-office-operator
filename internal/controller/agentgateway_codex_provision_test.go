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
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/enterprisewebservice/agent-office-operator/internal/templates"
)

// The shipped codex-auth-sync script, run once (CODEX_SYNC_ONCE=1) under
// node against a fixture gateway workspace. A Codex hire never writes
// its own agent store, so the runtime never creates one and the seed
// pass had nothing to seed (runtab-help, 2026-09-25): these tests pin
// the provision pass that closes that gap — a store for every agent in
// agents.list, created by the runtime's own initializer when the image's
// plugin SDK offers it, by the embedded 2026.7.1 schema when it does not,
// and by nothing at all for a directory the operator never registered.

// codexSyncNode returns a node that can load node:sqlite — the
// primitive the script is built on (the gateway image runs Node 24) —
// or skips, the way the activity-script test skips without GNU find.
func codexSyncNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; the gateway image carries it")
	}
	if out, err := exec.Command(node, "-e", `require("node:sqlite")`).CombinedOutput(); err != nil {
		t.Skipf("node cannot load node:sqlite: %s", out)
	}
	return node
}

// codexSyncFixture is one gateway's worth of sidecar state under a temp
// dir: the Secret projection (src), the writable .codex/ target (dst),
// the workspace's openclaw.json and agents/ dir, and the script itself.
type codexSyncFixture struct {
	node, script, root, src, dst, agents, config string
	pid                                          string // openai:<email> the seed pass derives from auth.json
	expiresMs                                    int64  // access-token expiry auth.json carries, in ms
}

func newCodexSyncFixture(t *testing.T, listed []string) *codexSyncFixture {
	t.Helper()
	root := t.TempDir()
	f := &codexSyncFixture{
		node:   codexSyncNode(t),
		script: filepath.Join(root, "codex-auth-sync.mjs"),
		root:   root,
		src:    filepath.Join(root, "secret", "auth.json"),
		dst:    filepath.Join(root, "codex-home", "auth.json"),
		agents: filepath.Join(root, "workspace", "agents"),
		config: filepath.Join(root, "workspace", "openclaw.json"),
	}
	for _, d := range []string{filepath.Dir(f.src), filepath.Dir(f.dst), f.agents} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.script, []byte(templates.CodexAuthSyncScript), 0o644); err != nil {
		t.Fatal(err)
	}

	// auth.json with the shape the Dev Hub re-auth flow / ESO deliver:
	// a token pair whose access JWT carries exp and the plan claim, an
	// id JWT carrying the email that keys the profile id.
	exp := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	f.expiresMs = exp.Unix() * 1000
	f.pid = "openai:hire@example.test"
	jwt := func(claims map[string]interface{}) string {
		b, _ := json.Marshal(claims)
		return "h." + base64.RawURLEncoding.EncodeToString(b) + ".s"
	}
	auth := map[string]interface{}{
		"tokens": map[string]interface{}{
			"access_token": jwt(map[string]interface{}{
				"exp":                         exp.Unix(),
				"https://api.openai.com/auth": map[string]interface{}{"chatgpt_plan_type": "pro"},
			}),
			"refresh_token": "rt-from-secret",
			"id_token":      jwt(map[string]interface{}{"email": "hire@example.test"}),
			"account_id":    "acct-1",
		},
	}
	b, _ := json.Marshal(auth)
	if err := os.WriteFile(f.src, b, 0o600); err != nil {
		t.Fatal(err)
	}

	var list []map[string]string
	for _, id := range listed {
		list = append(list, map[string]string{"id": id, "name": id})
	}
	cfg, _ := json.Marshal(map[string]interface{}{"agents": map[string]interface{}{"list": list}})
	if err := os.WriteFile(f.config, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *codexSyncFixture) storePath(id string) string {
	return filepath.Join(f.agents, id, "agent", "openclaw-agent.sqlite")
}

// runOnce runs one sidecar tick and returns its log output. env extends
// the fixture's own overrides (sdk dir, tick mode).
func (f *codexSyncFixture) runOnce(t *testing.T, env map[string]string) string {
	t.Helper()
	cmd := exec.Command(f.node, f.script)
	cmd.Env = append(os.Environ(),
		"CODEX_SYNC_ONCE=1",
		"CODEX_AUTH_SRC="+f.src,
		"CODEX_AUTH_DST="+f.dst,
		"CODEX_AGENTS_DIR="+f.agents,
		"CODEX_CONFIG_PATH="+f.config,
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("codex-auth-sync exited %v:\n%s", err, out)
	}
	return string(out)
}

// nodeScript writes a helper module into the fixture and returns its path.
func (f *codexSyncFixture) nodeScript(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(f.root, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// storeDump is what the test reads back from one agent store.
type storeDump struct {
	Objects []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"objects"`
	UserVersion int                      `json:"user_version"`
	Meta        []map[string]interface{} `json:"meta"`
	Store       map[string]interface{}   `json:"store"`
	State       map[string]interface{}   `json:"state"`
}

func (f *codexSyncFixture) dump(t *testing.T, id string) storeDump {
	t.Helper()
	dumper := f.nodeScript(t, "dump.mjs", `
import { DatabaseSync } from "node:sqlite";
const db = new DatabaseSync(process.argv[2], { readOnly: true });
const has = (n) => !!db.prepare("SELECT name FROM sqlite_master WHERE type='table' AND name=?").get(n);
const out = {
  objects: db.prepare("SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name").all(),
  user_version: db.prepare("PRAGMA user_version").get().user_version,
  meta: has("schema_meta") ? db.prepare("SELECT * FROM schema_meta").all() : null,
  store: null,
  state: null,
};
if (has("auth_profile_store")) {
  const r = db.prepare("SELECT store_json FROM auth_profile_store WHERE store_key='primary'").get();
  out.store = r ? JSON.parse(r.store_json) : null;
}
if (has("auth_profile_state")) {
  const r = db.prepare("SELECT state_json FROM auth_profile_state WHERE state_key='primary'").get();
  out.state = r ? JSON.parse(r.state_json) : null;
}
db.close();
process.stdout.write(JSON.stringify(out));
`)
	out, err := exec.Command(f.node, dumper, f.storePath(id)).Output()
	if err != nil {
		t.Fatalf("dump %s: %v", id, err)
	}
	var d storeDump
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("dump %s: %v\n%s", id, err, out)
	}
	return d
}

// seedExistingStore lays down an already-initialized store (the three
// tables the seed pass touches plus the runtime's ownership row) holding
// the given store/state payloads — an agent that was provisioned and
// seeded on an earlier day.
func (f *codexSyncFixture) seedExistingStore(t *testing.T, id string, store, state map[string]interface{}) {
	t.Helper()
	seeder := f.nodeScript(t, "seed.mjs", `
import { DatabaseSync } from "node:sqlite";
import fs from "node:fs";
import path from "node:path";
const [, , dbPath, agentId, storeJSON, stateJSON] = process.argv;
fs.mkdirSync(path.dirname(dbPath), { recursive: true });
const db = new DatabaseSync(dbPath);
db.exec("CREATE TABLE schema_meta (meta_key TEXT NOT NULL PRIMARY KEY, role TEXT NOT NULL, schema_version INTEGER NOT NULL, agent_id TEXT, app_version TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);" +
  "CREATE TABLE auth_profile_store (store_key TEXT NOT NULL PRIMARY KEY, store_json TEXT NOT NULL, updated_at INTEGER NOT NULL);" +
  "CREATE TABLE auth_profile_state (state_key TEXT NOT NULL PRIMARY KEY, state_json TEXT NOT NULL, updated_at INTEGER NOT NULL);");
db.prepare("INSERT INTO schema_meta VALUES ('primary','agent',1,?,NULL,1,1)").run(agentId);
db.prepare("INSERT INTO auth_profile_store VALUES ('primary',?,1)").run(storeJSON);
db.prepare("INSERT INTO auth_profile_state VALUES ('primary',?,1)").run(stateJSON);
db.close();
`)
	sj, _ := json.Marshal(store)
	tj, _ := json.Marshal(state)
	if out, err := exec.Command(f.node, seeder, f.storePath(id), id, string(sj), string(tj)).CombinedOutput(); err != nil {
		t.Fatalf("seed %s: %v\n%s", id, err, out)
	}
}

// stubSDK writes a plugin-sdk stand-in whose ensureOpenClawAgentDatabaseSchema
// creates the tables the seed pass needs and stamps app_version with the
// options it was handed — proof the sidecar called the RUNTIME's
// initializer, with the right agent id and path, rather than its own DDL.
func (f *codexSyncFixture) stubSDK(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(f.root, "plugin-sdk")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.nodeScript(t, filepath.Join("plugin-sdk", "sqlite-runtime.js"), `
export function ensureOpenClawAgentDatabaseSchema(db, options) {
  db.exec("CREATE TABLE IF NOT EXISTS schema_meta (meta_key TEXT NOT NULL PRIMARY KEY, role TEXT NOT NULL, schema_version INTEGER NOT NULL, agent_id TEXT, app_version TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);" +
    "CREATE TABLE IF NOT EXISTS auth_profile_store (store_key TEXT NOT NULL PRIMARY KEY, store_json TEXT NOT NULL, updated_at INTEGER NOT NULL);" +
    "CREATE TABLE IF NOT EXISTS auth_profile_state (state_key TEXT NOT NULL PRIMARY KEY, state_json TEXT NOT NULL, updated_at INTEGER NOT NULL);");
  db.prepare("INSERT OR REPLACE INTO schema_meta VALUES ('primary','agent',1,?,?,1,1)").run(options.agentId, "stub-sdk:" + options.agentId + ":" + options.path);
}
`)
	return dir
}

func (f *codexSyncFixture) seededProfile(t *testing.T, d storeDump, id string) map[string]interface{} {
	t.Helper()
	profiles, _ := d.Store["profiles"].(map[string]interface{})
	p, _ := profiles[f.pid].(map[string]interface{})
	if p == nil {
		t.Fatalf("%s: store lacks the seeded profile %s: %v", id, f.pid, d.Store)
	}
	return p
}

func TestCodexAuthSyncProvisionsListedAgentsViaRuntimeInitializer(t *testing.T) {
	f := newCodexSyncFixture(t, []string{"new-hire", "empty-shell", "kept-fresh"})

	// empty-shell: the 0-byte file a DatabaseSync opened on a missing
	// path leaves behind — the "no such table: auth_profile_store" case.
	if err := os.MkdirAll(filepath.Dir(f.storePath("empty-shell")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.storePath("empty-shell"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// kept-fresh: provisioned earlier, holding an openai profile FRESHER
	// than auth.json plus another provider's profile — must survive both
	// passes byte for byte (never downgrade, preserve what we don't own).
	fresher := map[string]interface{}{
		"type": "oauth", "provider": "openai", "email": "hire@example.test",
		"access": "at-runtime-refreshed", "refresh": "rt-runtime-refreshed", "expires": f.expiresMs + 3600_000,
	}
	anthropic := map[string]interface{}{"type": "api_key", "provider": "anthropic", "key": "sk-ant-keep"}
	f.seedExistingStore(t, "kept-fresh",
		map[string]interface{}{"version": 1, "profiles": map[string]interface{}{f.pid: fresher, "anthropic:default": anthropic}},
		map[string]interface{}{"version": 1, "order": map[string]interface{}{"openai": []string{f.pid}}, "usageStats": map[string]interface{}{"turns": 7}},
	)
	// main: the runtime's implicit default agent — a directory with no
	// agents.list entry. Never provisioned.
	if err := os.MkdirAll(filepath.Join(f.agents, "main", "agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.agents, "main", "agent", "models.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := f.runOnce(t, map[string]string{"CODEX_OPENCLAW_SDK_DIR": f.stubSDK(t)})

	for _, id := range []string{"new-hire", "empty-shell"} {
		want := "provisioned agent store for " + id + " via the runtime initializer"
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
		if !strings.Contains(out, "seeded "+id+" -> "+f.pid) {
			t.Errorf("%s was not seeded in the same tick it was provisioned:\n%s", id, out)
		}
		st, err := os.Stat(f.storePath(id))
		if err != nil {
			t.Fatalf("%s: store missing: %v", id, err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s: store mode %o, want 0600", id, st.Mode().Perm())
		}
		d := f.dump(t, id)
		if len(d.Meta) != 1 || d.Meta[0]["agent_id"] != id || d.Meta[0]["app_version"] != "stub-sdk:"+id+":"+f.storePath(id) {
			t.Errorf("%s: schema_meta not written by the runtime initializer with the agent's id and path: %v", id, d.Meta)
		}
		p := f.seededProfile(t, d, id)
		if p["refresh"] != "rt-from-secret" || p["provider"] != "openai" || p["type"] != "oauth" || p["expires"] != float64(f.expiresMs) || p["chatgptPlanType"] != "pro" || p["accountId"] != "acct-1" {
			t.Errorf("%s: seeded profile = %v", id, p)
		}
		order, _ := d.State["order"].(map[string]interface{})
		lastGood, _ := d.State["lastGood"].(map[string]interface{})
		if !reflect.DeepEqual(order["openai"], []interface{}{f.pid}) || lastGood["openai"] != f.pid {
			t.Errorf("%s: auth_profile_state order/lastGood = %v / %v", id, order, lastGood)
		}
	}

	if strings.Contains(out, "kept-fresh") {
		t.Errorf("kept-fresh must be neither provisioned nor re-seeded:\n%s", out)
	}
	d := f.dump(t, "kept-fresh")
	if d.Meta[0]["app_version"] != nil {
		t.Errorf("kept-fresh: initializer ran on an initialized store: %v", d.Meta)
	}
	profiles, _ := d.Store["profiles"].(map[string]interface{})
	if got, _ := profiles[f.pid].(map[string]interface{}); got["refresh"] != "rt-runtime-refreshed" {
		t.Errorf("kept-fresh: the fresher openai profile was downgraded: %v", got)
	}
	if got, _ := profiles["anthropic:default"].(map[string]interface{}); got["key"] != "sk-ant-keep" {
		t.Errorf("kept-fresh: another provider's profile was lost: %v", profiles)
	}
	if stats, _ := d.State["usageStats"].(map[string]interface{}); stats["turns"] != float64(7) {
		t.Errorf("kept-fresh: state we don't own was rewritten: %v", d.State)
	}

	if _, err := os.Stat(f.storePath("main")); !os.IsNotExist(err) {
		t.Errorf("main has no agents.list entry and must not get a store (stat err=%v)", err)
	}

	got, _ := os.ReadFile(f.dst)
	want, _ := os.ReadFile(f.src)
	if string(got) != string(want) {
		t.Error("auth.json was not mirrored into .codex/")
	}
}

func TestCodexAuthSyncProvisionsWithEmbeddedSchemaWhenRuntimeSDKMissing(t *testing.T) {
	f := newCodexSyncFixture(t, []string{"new-hire", "Not.Normalized"})
	env := map[string]string{"CODEX_OPENCLAW_SDK_DIR": filepath.Join(f.root, "no-such-sdk")}

	out := f.runOnce(t, env)
	for _, want := range []string{
		"runtime sdk unavailable",
		"provisioned agent store for new-hire via the embedded openclaw-2026.7.1 schema",
		"seeded new-hire -> " + f.pid,
		`agents.list id "Not.Normalized" is not a normalized agent id`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if entries, _ := os.ReadDir(f.agents); len(entries) != 1 || entries[0].Name() != "new-hire" {
		t.Errorf("agents dir = %v, want only new-hire (an id the runtime would rename must not get a directory)", entries)
	}

	// The embedded copy must produce exactly what OpenClaw 2026.7.1's
	// generated schema produces (verified against a live store: the
	// same 9 tables, 8 named indexes, 6 triggers, user_version 1 and an
	// agent-owned schema_meta row).
	d := f.dump(t, "new-hire")
	byType := map[string][]string{}
	for _, o := range d.Objects {
		byType[o.Type] = append(byType[o.Type], o.Name)
	}
	for k := range byType {
		sort.Strings(byType[k])
	}
	wantObjects := map[string][]string{
		"table": {"auth_profile_state", "auth_profile_store", "cache_entries", "memory_embedding_cache", "memory_index_chunks",
			"memory_index_meta", "memory_index_sources", "memory_index_state", "schema_meta"},
		"index": {"idx_agent_cache_expiry", "idx_agent_cache_updated", "idx_memory_embedding_cache_updated_at", "idx_memory_index_chunks_path",
			"idx_memory_index_chunks_path_source", "idx_memory_index_chunks_source", "idx_memory_index_sources_source"},
		"trigger": {"memory_index_chunks_revision_after_delete", "memory_index_chunks_revision_after_insert", "memory_index_chunks_revision_after_update",
			"memory_index_sources_revision_after_delete", "memory_index_sources_revision_after_insert", "memory_index_sources_revision_after_update"},
	}
	if !reflect.DeepEqual(byType, wantObjects) {
		t.Errorf("embedded schema objects =\n%v\nwant\n%v", byType, wantObjects)
	}
	if d.UserVersion != 1 {
		t.Errorf("user_version = %d, want 1", d.UserVersion)
	}
	if len(d.Meta) != 1 {
		t.Fatalf("schema_meta rows = %v, want one", d.Meta)
	}
	m := d.Meta[0]
	if m["meta_key"] != "primary" || m["role"] != "agent" || m["schema_version"] != float64(1) || m["agent_id"] != "new-hire" || m["app_version"] != nil {
		t.Errorf("schema_meta = %v", m)
	}
	for _, k := range []string{"created_at", "updated_at"} {
		if ts, _ := m[k].(float64); ts < float64(time.Now().Add(-time.Hour).UnixMilli()) {
			t.Errorf("schema_meta.%s = %v, want a millisecond timestamp", k, m[k])
		}
	}
	if p := f.seededProfile(t, d, "new-hire"); p["refresh"] != "rt-from-secret" {
		t.Errorf("seeded profile = %v", p)
	}

	// Idempotent: a second tick finds nothing to provision or seed.
	again := f.runOnce(t, env)
	for _, banned := range []string{"provisioned", "seeded", "failed"} {
		if strings.Contains(again, banned) {
			t.Errorf("second tick was not a no-op (%q):\n%s", banned, again)
		}
	}
}

// A store the sidecar cannot read as a database is reported and left
// alone — clobbering it would destroy a credential lineage the agent
// may still be refreshing on its own.
func TestCodexAuthSyncLeavesUnreadableStoreAlone(t *testing.T) {
	f := newCodexSyncFixture(t, []string{"garbled"})
	if err := os.MkdirAll(filepath.Dir(f.storePath("garbled")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.storePath("garbled"), []byte("this is not a sqlite file at all, but it is not empty either"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := f.runOnce(t, map[string]string{"CODEX_OPENCLAW_SDK_DIR": f.stubSDK(t)})
	if !strings.Contains(out, "provision garbled failed") {
		t.Errorf("log lacks the provision failure:\n%s", out)
	}
	if strings.Contains(out, "provisioned agent store for garbled") {
		t.Errorf("an unreadable store was re-initialized:\n%s", out)
	}
	got, _ := os.ReadFile(f.storePath("garbled"))
	if !strings.HasPrefix(string(got), "this is not a sqlite file") {
		t.Errorf("unreadable store was rewritten: %q", got)
	}
}
