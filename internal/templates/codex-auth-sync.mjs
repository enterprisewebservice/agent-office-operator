// codex-auth-sync — the gateway pod's Codex credential loop, rendered
// into the gateway config CM by the operator and run as the
// codex-auth-sync sidecar (same OpenClaw image as the main container,
// which is what guarantees `node:sqlite` matches the runtime that owns
// the stores).
//
// Two jobs, every tick:
//
//  1. Mirror /var/lib/codex-auth/auth.json (the Secret projection the
//     kubelet keeps fresh as ESO / the Dev Hub re-auth flow updates it)
//     into the writable /home/node/.codex/auth.json — the path OpenClaw
//     and codex-acp actually read. Same contract as the shell loop this
//     replaces: copy on content change only.
//
//  2. Seed pass: every logical agent keeps its OWN copy of the shared
//     Codex-subscription OAuth in
//     /home/node/.openclaw/agents/<id>/agent/openclaw-agent.sqlite
//     (auth_profile_store / auth_profile_state, key 'primary').
//     Those copies were seeded once at provisioning and never again —
//     on 2026-08-23 the access token expired, a refresh hiccuped,
//     OpenClaw pruned the profiles, and nothing re-seeded: 16h dead
//     desk while the pod held a perfectly good credential in
//     auth.json. This pass closes that hop: whenever an agent store
//     lacks a usable openai OAuth profile, or auth.json carries a
//     strictly newer token, upsert store + state from auth.json.
//
// Invariants:
//  - Never create an agent DB: a missing sqlite file means the agent is
//    still being provisioned; DatabaseSync would happily create an
//    empty shell that confuses that flow.
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
//   CODEX_SYNC_TICK_MS default 30000
//   CODEX_SYNC_ONCE  set to 1 to run one tick and exit
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { DatabaseSync } from "node:sqlite";

const SRC = process.env.CODEX_AUTH_SRC ?? "/var/lib/codex-auth/auth.json";
const DST = process.env.CODEX_AUTH_DST ?? "/home/node/.codex/auth.json";
const AGENTS_DIR = process.env.CODEX_AGENTS_DIR ?? "/home/node/.openclaw/agents";
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
// moves on to the next agent, and a mid-provision DB whose tables don't
// exist yet lands here too (transient; the next tick retries).
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
    const dbPath = path.join(AGENTS_DIR, id, "agent", "openclaw-agent.sqlite");
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

const tick = () => {
  try {
    syncAuthJSON();
  } catch (err) {
    log(`auth.json sync failed: ${err?.message ?? err}`);
  }
  try {
    seedPass();
  } catch (err) {
    log(`seed pass failed: ${err?.message ?? err}`);
  }
};

log(`up (tick=${TICK_MS / 1000}s, src=${SRC}, agents=${AGENTS_DIR})`);
tick();
if (process.env.CODEX_SYNC_ONCE !== "1") {
  setInterval(tick, TICK_MS);
}
