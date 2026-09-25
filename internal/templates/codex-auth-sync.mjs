// codex-auth-sync — the gateway pod's Codex credential loop, rendered
// into the gateway config CM by the operator and run as the
// codex-auth-sync sidecar (same OpenClaw image as the main container,
// which is what guarantees `node:sqlite` matches the runtime that owns
// the stores).
//
// Three jobs, every tick:
//
//  1. Mirror /var/lib/codex-auth/auth.json (the Secret projection the
//     kubelet keeps fresh as ESO / the Dev Hub re-auth flow updates it)
//     into the writable /home/node/.codex/auth.json — the path OpenClaw
//     and codex-acp actually read. Same contract as the shell loop this
//     replaces: copy on content change only.
//
//  2. Provision pass (v1.7.79): every agent the operator has registered
//     in openclaw.json agents.list[] gets its per-agent store,
//     /home/node/.openclaw/agents/<id>/agent/openclaw-agent.sqlite,
//     as soon as it is missing. OpenClaw creates that store lazily, on
//     the agent's FIRST WRITE to it (an OAuth login or a token refresh
//     the runtime persists itself) — reads never create it. A Codex
//     hire never writes: its credential arrives from outside (job 3),
//     so the store never appeared, job 3 found nothing to seed, and the
//     agent's first turn died with `No API key found for provider
//     "openai"` (runtab-help, 2026-09-25: the agent dir held only
//     models.json). The store is created by the runtime's OWN
//     initializer — ensureOpenClawAgentDatabaseSchema from the image's
//     public plugin SDK (/app/dist/plugin-sdk/sqlite-runtime.js), the
//     same ensureAgentSchema the gateway runs on that first write — so
//     the DDL, user_version and schema_meta ownership row are whatever
//     THIS runtime says they are. Only when that entry is missing does
//     an embedded copy of the runtime's generated schema (2026.7.1)
//     stand in, and the log says so.
//
//  3. Seed pass: every logical agent keeps its OWN copy of the shared
//     Codex-subscription OAuth in that store (auth_profile_store /
//     auth_profile_state, key 'primary'). Those copies were seeded once
//     at provisioning and never again — on 2026-08-23 the access token
//     expired, a refresh hiccuped, OpenClaw pruned the profiles, and
//     nothing re-seeded: 16h dead desk while the pod held a perfectly
//     good credential in auth.json. This pass closes that hop: whenever
//     an agent store lacks a usable openai OAuth profile, or auth.json
//     carries a strictly newer token, upsert store + state from
//     auth.json.
//
// Invariants:
//  - A store is created only for an agent named in agents.list (the
//    operator's registration of record) and only through the runtime's
//    initializer or its schema — never by copying another agent's file
//    (a shared refresh-token lineage strands both). Directories with no
//    agents.list entry (`main`, a de-registered agent) are left alone.
//  - Never downgrade: an expired-but-refreshable profile counts as
//    usable (refreshing is OpenClaw's job — the live fleet runs for
//    hours past `expires` on refresh alone), and OpenClaw's own
//    refresh may hold a NEWER token than the Secret. Only write when
//    the store has nothing usable or auth.json is strictly fresher.
//  - Preserve what we don't own: other providers' profiles in
//    store_json, usageStats etc. in state_json.
//
// Env overrides (tests + ad-hoc use — `oc exec` with CODEX_SYNC_ONCE=1
// replaces the hand-run seed scripts in the newsdesk repos):
//   CODEX_AUTH_SRC   default /var/lib/codex-auth/auth.json
//   CODEX_AUTH_DST   default /home/node/.codex/auth.json
//   CODEX_AGENTS_DIR default /home/node/.openclaw/agents
//   CODEX_CONFIG_PATH default /home/node/.openclaw/openclaw.json
//   CODEX_OPENCLAW_SDK_DIR default /app/dist/plugin-sdk
//   CODEX_SYNC_TICK_MS default 30000
//   CODEX_SYNC_ONCE  set to 1 to run one tick and exit
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { pathToFileURL } from "node:url";
import { DatabaseSync } from "node:sqlite";

const SRC = process.env.CODEX_AUTH_SRC ?? "/var/lib/codex-auth/auth.json";
const DST = process.env.CODEX_AUTH_DST ?? "/home/node/.codex/auth.json";
const AGENTS_DIR = process.env.CODEX_AGENTS_DIR ?? "/home/node/.openclaw/agents";
const CONFIG_PATH = process.env.CODEX_CONFIG_PATH ?? "/home/node/.openclaw/openclaw.json";
const SDK_DIR = process.env.CODEX_OPENCLAW_SDK_DIR ?? "/app/dist/plugin-sdk";
const TICK_MS = Number(process.env.CODEX_SYNC_TICK_MS ?? 30000);

const log = (msg) => console.log(`codex-auth-sync: ${msg}`);

const claims = (jwt) => {
  try {
    return JSON.parse(Buffer.from(String(jwt).split(".")[1], "base64url").toString());
  } catch {
    return {};
  }
};

// profileFromAuthJSON parses the synced auth.json into the auth-store
// profile shape OpenClaw uses, or null when the file is absent,
// unreadable, or lacks a token pair (nothing usable to seed).
function profileFromAuthJSON() {
  let a;
  try {
    a = JSON.parse(fs.readFileSync(SRC, "utf8"));
  } catch {
    return null;
  }
  const tk = a.tokens ?? {};
  if (!tk.access_token || !tk.refresh_token) return null;
  const idc = claims(tk.id_token);
  const acc = claims(tk.access_token);
  const email = idc.email ?? acc["https://api.openai.com/profile"]?.email ?? "unknown";
  const plan =
    idc["https://api.openai.com/auth"]?.chatgpt_plan_type ??
    acc["https://api.openai.com/auth"]?.chatgpt_plan_type ??
    "unknown";
  const expires = acc.exp ? acc.exp * 1000 : Date.now() + 6 * 864e5;
  return {
    pid: `openai:${email}`,
    profile: {
      type: "oauth",
      provider: "openai",
      email,
      access: tk.access_token,
      refresh: tk.refresh_token,
      accountId: tk.account_id ?? idc["https://api.openai.com/auth"]?.chatgpt_account_id ?? "",
      chatgptPlanType: plan,
      expires,
    },
  };
}

// syncAuthJSON mirrors the Secret projection into the writable .codex/
// emptyDir when the content changed. In-memory sha means a restarted
// sidecar re-copies once — same-content overwrite, harmless.
let lastSha = "";
function syncAuthJSON() {
  let data;
  try {
    data = fs.readFileSync(SRC);
  } catch {
    return false; // Secret not created yet (projection is Optional)
  }
  const sha = crypto.createHash("sha256").update(data).digest("hex");
  if (sha === lastSha) return false;
  fs.writeFileSync(DST, data, { mode: 0o600 });
  fs.chmodSync(DST, 0o600); // writeFileSync applies mode on create only
  lastSha = sha;
  log(`refreshed auth.json (sha256=${sha})`);
  if (!profileFromAuthJSON()) {
    log("auth.json has no usable token pair; seed pass idle (re-auth via the Dev Hub Codex card)");
  }
  return true;
}

// ---- provision pass ------------------------------------------------

// EMBEDDED_AGENT_SCHEMA_SQL is a verbatim copy of OpenClaw 2026.7.1's
// generated per-agent schema (src/state/openclaw-agent-schema.generated.ts,
// shipped in dist/openclaw-agent-db-*.js). It is the FALLBACK only: the
// provision pass prefers the runtime's own initializer, which carries the
// schema of whatever runtime the pod actually runs. When this copy is
// used on a newer runtime, that runtime's ensureAgentSchema re-runs its
// own CREATE ... IF NOT EXISTS on first open and upserts schema_meta, so
// a store created from here is upgraded in place rather than rejected.
const EMBEDDED_SCHEMA_VERSION = "openclaw-2026.7.1";
const EMBEDDED_SCHEMA_USER_VERSION = 1;
const EMBEDDED_AGENT_SCHEMA_SQL = `CREATE TABLE IF NOT EXISTS schema_meta (
  meta_key TEXT NOT NULL PRIMARY KEY,
  role TEXT NOT NULL,
  schema_version INTEGER NOT NULL,
  agent_id TEXT,
  app_version TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS cache_entries (
  scope TEXT NOT NULL,
  key TEXT NOT NULL,
  value_json TEXT,
  blob BLOB,
  expires_at INTEGER,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (scope, key)
);

CREATE INDEX IF NOT EXISTS idx_agent_cache_expiry
  ON cache_entries(scope, expires_at, key)
  WHERE expires_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_agent_cache_updated
  ON cache_entries(scope, updated_at DESC, key);

CREATE TABLE IF NOT EXISTS auth_profile_store (
  store_key TEXT NOT NULL PRIMARY KEY,
  store_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_profile_state (
  state_key TEXT NOT NULL PRIMARY KEY,
  state_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS memory_index_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS memory_index_sources (
  path TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT 'memory',
  hash TEXT NOT NULL,
  mtime INTEGER NOT NULL,
  size INTEGER NOT NULL,
  PRIMARY KEY (path, source)
);

CREATE TABLE IF NOT EXISTS memory_index_chunks (
  id TEXT PRIMARY KEY,
  path TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT 'memory',
  start_line INTEGER NOT NULL,
  end_line INTEGER NOT NULL,
  hash TEXT NOT NULL,
  model TEXT NOT NULL,
  text TEXT NOT NULL,
  embedding TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS memory_embedding_cache (
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  provider_key TEXT NOT NULL,
  hash TEXT NOT NULL,
  embedding TEXT NOT NULL,
  dims INTEGER,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (provider, model, provider_key, hash)
);

CREATE TABLE IF NOT EXISTS memory_index_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  revision INTEGER NOT NULL
);

INSERT OR IGNORE INTO memory_index_state (id, revision) VALUES (1, 0);

CREATE TRIGGER IF NOT EXISTS memory_index_sources_revision_after_insert
AFTER INSERT ON memory_index_sources
BEGIN
  UPDATE memory_index_state SET revision = revision + 1 WHERE id = 1;
END;

CREATE TRIGGER IF NOT EXISTS memory_index_sources_revision_after_update
AFTER UPDATE ON memory_index_sources
BEGIN
  UPDATE memory_index_state SET revision = revision + 1 WHERE id = 1;
END;

CREATE TRIGGER IF NOT EXISTS memory_index_sources_revision_after_delete
AFTER DELETE ON memory_index_sources
BEGIN
  UPDATE memory_index_state SET revision = revision + 1 WHERE id = 1;
END;

CREATE TRIGGER IF NOT EXISTS memory_index_chunks_revision_after_insert
AFTER INSERT ON memory_index_chunks
BEGIN
  UPDATE memory_index_state SET revision = revision + 1 WHERE id = 1;
END;

CREATE TRIGGER IF NOT EXISTS memory_index_chunks_revision_after_update
AFTER UPDATE ON memory_index_chunks
BEGIN
  UPDATE memory_index_state SET revision = revision + 1 WHERE id = 1;
END;

CREATE TRIGGER IF NOT EXISTS memory_index_chunks_revision_after_delete
AFTER DELETE ON memory_index_chunks
BEGIN
  UPDATE memory_index_state SET revision = revision + 1 WHERE id = 1;
END;

CREATE INDEX IF NOT EXISTS idx_memory_embedding_cache_updated_at
  ON memory_embedding_cache(updated_at);

CREATE INDEX IF NOT EXISTS idx_memory_index_sources_source
  ON memory_index_sources(source);

CREATE INDEX IF NOT EXISTS idx_memory_index_chunks_path_source
  ON memory_index_chunks(path, source);

CREATE INDEX IF NOT EXISTS idx_memory_index_chunks_path
  ON memory_index_chunks(path);

CREATE INDEX IF NOT EXISTS idx_memory_index_chunks_source
  ON memory_index_chunks(source);
`;

// AGENT_ID_RE is the lowercase form of the runtime's VALID_ID_RE
// (session-key: /^[a-z0-9][a-z0-9_-]{0,63}$/i). normalizeAgentId
// lowercases and squeezes anything else to '-', so an id that is not
// already in this form would resolve to a DIFFERENT directory than the
// one named after it — such ids are skipped, never normalized here.
const AGENT_ID_RE = /^[a-z0-9][a-z0-9_-]{0,63}$/;

// listedAgentIds returns agents.list[].id from openclaw.json. The
// operator writes that list (AgentWorkstation reconcile via `openclaw
// config set`), so it is the authoritative roster of agents this
// gateway hosts. [] when the file is absent or not valid JSON at this
// instant (the runtime rewrites it in place; the next tick retries).
function listedAgentIds() {
  let cfg;
  try {
    cfg = JSON.parse(fs.readFileSync(CONFIG_PATH, "utf8"));
  } catch {
    return [];
  }
  const list = Array.isArray(cfg?.agents?.list) ? cfg.agents.list : [];
  const ids = [];
  for (const entry of list) {
    const id = typeof entry?.id === "string" ? entry.id.trim() : "";
    if (!id || ids.includes(id)) continue;
    if (!AGENT_ID_RE.test(id)) {
      log(`agents.list id ${JSON.stringify(id)} is not a normalized agent id; not provisioning it`);
      continue;
    }
    ids.push(id);
  }
  return ids;
}

const agentStorePath = (id) => path.join(AGENTS_DIR, id, "agent", "openclaw-agent.sqlite");

// storeInitialized reports whether the agent store exists and carries
// the runtime's schema_meta table. A missing file or a 0-byte shell
// (what a DatabaseSync opened on a missing path leaves behind — the
// "no such table: auth_profile_store" seed failures) is uninitialized.
// Throws when the file cannot be read as a database; the caller logs
// and leaves it alone rather than clobbering it.
function storeInitialized(dbPath) {
  let st;
  try {
    st = fs.statSync(dbPath);
  } catch {
    return false;
  }
  if (st.size === 0) return false;
  const db = new DatabaseSync(dbPath, { readOnly: true });
  try {
    db.exec("PRAGMA busy_timeout = 5000");
    return !!db.prepare("SELECT name FROM sqlite_master WHERE type='table' AND name='schema_meta'").get();
  } finally {
    db.close();
  }
}

// runtimeInitializer resolves the runtime's own agent-store initializer
// once per process: ensureOpenClawAgentDatabaseSchema(db, {agentId,
// path}) from the image's public plugin SDK — a stable, unhashed entry
// point, unlike the dist chunks it wraps. null when the entry is
// missing or lacks the export (a runtime whose layout moved); the
// provision pass then uses the embedded schema and says so.
let initializerPromise;
function runtimeInitializer() {
  initializerPromise ??= (async () => {
    const entry = path.join(SDK_DIR, "sqlite-runtime.js");
    try {
      const mod = await import(pathToFileURL(entry).href);
      if (typeof mod.ensureOpenClawAgentDatabaseSchema === "function") {
        return mod.ensureOpenClawAgentDatabaseSchema;
      }
      log(`${entry} exports no ensureOpenClawAgentDatabaseSchema; new agent stores use the embedded ${EMBEDDED_SCHEMA_VERSION} schema`);
    } catch (err) {
      log(`runtime sdk unavailable at ${entry} (${err?.message ?? err}); new agent stores use the embedded ${EMBEDDED_SCHEMA_VERSION} schema`);
    }
    return null;
  })();
  return initializerPromise;
}

// createStore initializes one agent store in place. With the runtime's
// initializer the runtime owns everything — DDL, user_version, the
// schema_meta ownership row, directory and file modes (0700/0600). The
// fallback replays those same steps from the embedded copy. Idempotent:
// every statement is IF NOT EXISTS / upsert, so a tick that dies
// half-way is finished by the next one.
function createStore(dbPath, agentId, init) {
  fs.mkdirSync(path.dirname(dbPath), { recursive: true, mode: 0o700 });
  const db = new DatabaseSync(dbPath);
  try {
    db.exec("PRAGMA busy_timeout = 5000");
    if (init) {
      init(db, { agentId, path: dbPath });
    } else {
      db.exec(EMBEDDED_AGENT_SCHEMA_SQL);
      db.exec(`PRAGMA user_version = ${EMBEDDED_SCHEMA_USER_VERSION}`);
      const now = Date.now();
      db.prepare(
        "INSERT INTO schema_meta (meta_key, role, schema_version, agent_id, app_version, created_at, updated_at) " +
          "VALUES ('primary', 'agent', ?, ?, NULL, ?, ?) " +
          "ON CONFLICT(meta_key) DO UPDATE SET role = 'agent', schema_version = excluded.schema_version, " +
          "agent_id = excluded.agent_id, app_version = NULL, updated_at = excluded.updated_at",
      ).run(EMBEDDED_SCHEMA_USER_VERSION, agentId, now, now);
    }
  } finally {
    db.close();
  }
  fs.chmodSync(dbPath, 0o600);
}

// provisionPass creates the store for every listed agent that lacks
// one. Runs before the seed pass so a brand-new store is seeded in the
// same tick — the hire answers its first turn instead of failing it.
async function provisionPass() {
  const ids = listedAgentIds();
  let init;
  for (const id of ids) {
    const dbPath = agentStorePath(id);
    try {
      if (storeInitialized(dbPath)) continue;
      if (init === undefined) init = await runtimeInitializer();
      createStore(dbPath, id, init);
      log(`provisioned agent store for ${id} via ${init ? "the runtime initializer" : `the embedded ${EMBEDDED_SCHEMA_VERSION} schema`}`);
    } catch (err) {
      log(`provision ${id} failed: ${err?.message ?? err}`);
    }
  }
}

// ---- seed pass ---------------------------------------------------------

// openAIState scans a store for openai OAuth profiles and reports
// whether any is usable (access+refresh present — expiry deliberately
// ignored, see invariants) plus the freshest expiry among them.
function openAIState(store) {
  let usable = false;
  let maxExpires = 0;
  for (const p of Object.values(store.profiles ?? {})) {
    if (!p || p.provider !== "openai" || p.type !== "oauth") continue;
    if (!p.access || !p.refresh) continue;
    usable = true;
    maxExpires = Math.max(maxExpires, Number(p.expires) || 0);
  }
  return { usable, maxExpires };
}

// seedAgent upserts the shared profile into one agent store. Returns
// true when it wrote, false when the store was already as fresh as
// auth.json. Throws on sqlite/schema/JSON trouble — the caller logs and
// moves on to the next agent (the next tick retries).
function seedAgent(dbPath, pid, profile) {
  const db = new DatabaseSync(dbPath);
  try {
    db.exec("PRAGMA busy_timeout = 5000"); // OpenClaw holds this DB open; don't fail on a brief lock
    const now = Date.now();
    const row = db.prepare("SELECT store_json FROM auth_profile_store WHERE store_key='primary'").get();
    const store = row ? JSON.parse(row.store_json) : { version: 1, profiles: {} };
    const { usable, maxExpires } = openAIState(store);
    if (usable && profile.expires <= maxExpires) return false;
    store.profiles = { ...(store.profiles ?? {}), [pid]: profile };
    if (row) {
      db.prepare("UPDATE auth_profile_store SET store_json=?, updated_at=? WHERE store_key='primary'").run(
        JSON.stringify(store),
        now,
      );
    } else {
      db.prepare("INSERT INTO auth_profile_store (store_key, store_json, updated_at) VALUES ('primary',?,?)").run(
        JSON.stringify(store),
        now,
      );
    }
    const srow = db.prepare("SELECT state_json FROM auth_profile_state WHERE state_key='primary'").get();
    const st = srow ? JSON.parse(srow.state_json) : { version: 1 };
    st.order = { ...(st.order ?? {}), openai: [pid] };
    st.lastGood = { ...(st.lastGood ?? {}), openai: pid };
    if (srow) {
      db.prepare("UPDATE auth_profile_state SET state_json=?, updated_at=? WHERE state_key='primary'").run(
        JSON.stringify(st),
        now,
      );
    } else {
      db.prepare("INSERT INTO auth_profile_state (state_key, state_json, updated_at) VALUES ('primary',?,?)").run(
        JSON.stringify(st),
        now,
      );
    }
    return true;
  } finally {
    db.close();
  }
}

function seedPass() {
  const parsed = profileFromAuthJSON();
  if (!parsed) return;
  let ids = [];
  try {
    ids = fs.readdirSync(AGENTS_DIR);
  } catch {
    return; // workspace not initialized yet
  }
  for (const id of ids) {
    const dbPath = agentStorePath(id);
    if (!fs.existsSync(dbPath)) continue;
    try {
      if (seedAgent(dbPath, parsed.pid, parsed.profile)) {
        log(`seeded ${id} -> ${parsed.pid} (expires ${new Date(parsed.profile.expires).toISOString()})`);
      }
    } catch (err) {
      log(`seed ${id} failed: ${err?.message ?? err}`);
    }
  }
}

// ---- loop ---------------------------------------------------------------

let ticking = false;
async function tick() {
  if (ticking) return; // a tick stalled on a sqlite lock must not stack up behind itself
  ticking = true;
  try {
    try {
      syncAuthJSON();
    } catch (err) {
      log(`auth.json sync failed: ${err?.message ?? err}`);
    }
    try {
      await provisionPass();
    } catch (err) {
      log(`provision pass failed: ${err?.message ?? err}`);
    }
    try {
      seedPass();
    } catch (err) {
      log(`seed pass failed: ${err?.message ?? err}`);
    }
  } finally {
    ticking = false;
  }
}

log(`up (tick=${TICK_MS / 1000}s, src=${SRC}, agents=${AGENTS_DIR}, config=${CONFIG_PATH})`);
await tick();
if (process.env.CODEX_SYNC_ONCE !== "1") {
  setInterval(tick, TICK_MS);
}
