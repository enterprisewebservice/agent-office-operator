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

// toolsRestrictedCondition is the AgentWorkstation status condition that
// records "the operator rendered an enforced tool list for this agent"
// (spec.tools.restrictTo). It is the operator's memory of what it wrote:
// when the field is removed, the rendered `tools` object is deleted only
// if this condition is True.
const toolsRestrictedCondition = "ToolsRestricted"

// agentMergeParams is the JSON handed to agentMergeScript as its one
// argument. Passing data as JSON (not spliced into the source with
// Sprintf) keeps the script a constant the unit tests can run as shipped.
type agentMergeParams struct {
	// AgentEntry is the roster entry, WITH its id. The script drops the id
	// when the runtime keeps the roster as a keyed map.
	AgentEntry map[string]interface{} `json:"agentEntry"`
	// Profile is the agent's browser profile name.
	Profile string `json:"profile"`
	// ChannelID is the Discord channel id from the workstation spec, or "".
	ChannelID string `json:"channelId"`
	// AgentID is the agent's id (the workstation name).
	AgentID string `json:"agentId"`
	// DeclaredTools is spec.tools.allow: what the agent needs the gateway
	// to have configured. Additive, never a restriction.
	DeclaredTools []string `json:"declaredTools"`
	// SearchProvider is the web_search provider to seed when none is set.
	SearchProvider string `json:"searchProvider"`
	// RestrictTo is spec.tools.restrictTo: when non-empty the agent's
	// whole `tools` object is replaced by the enforced list.
	RestrictTo []string `json:"restrictTo,omitempty"`
	// RemoveRenderedTools asks the script to delete the agent's `tools`
	// object. Set only when the operator rendered one earlier (status
	// condition ToolsRestricted) and the spec no longer asks for it.
	RemoveRenderedTools bool `json:"removeRenderedTools,omitempty"`
}

// agentMergeScript merges one agent into the gateway's openclaw.json:
// its roster entry, its browser profile, the gateway-wide tool settings
// its spec declares, and its Discord binding. Run inside the gateway
// container as `node -e <rosterHelpersJS + agentMergeScript> <params>`.
//
// Loop-safety contract (the gateway watches this file and hot-reloads on
// every write): read, mutate in memory, compare, write only when the
// result differs. A converged agent prints NO_CHANGE and causes no file
// event.
//
// Entries the operator does not manage (a persona added by hand, with no
// AgentWorkstation) are never touched: only this agent's entry, this
// agent's bindings and wide catch-all bindings are rewritten.
const agentMergeScript = `
const fs = require("fs");
const CONFIG_PATH = "/home/node/.openclaw/openclaw.json";
const P = JSON.parse(process.argv[1]);
const agentEntry = P.agentEntry;
const profile = P.profile;
const channelId = P.channelId || "";
const agentId = P.agentId;
const declaredTools = Array.isArray(P.declaredTools) ? P.declaredTools : [];
const searchProvider = P.searchProvider;
const restrictTo = Array.isArray(P.restrictTo) && P.restrictTo.length ? P.restrictTo : null;
const removeRenderedTools = P.removeRenderedTools === true;

const raw = fs.readFileSync(CONFIG_PATH, "utf8");
const cfg = JSON.parse(raw);
const before = JSON.stringify(cfg);

// Roster: merge by agent id, in the format this runtime reads (the array
// agents.list up to 2026.7.x, the keyed map agents.entries from 2026.8.1).
//
// spec.tools.restrictTo is the ENFORCED tool list. While it is set the
// operator owns the agent's whole tools object: replace, never merge. A
// leftover alsoAllow next to allow makes OpenClaw reject the entire
// config ("agent tools cannot set both allow and alsoAllow in the same
// scope"). fs.workspaceOnly fences the read tool to the agent's own
// workspace. When the field is gone the object is deleted only if the
// operator rendered it; a tools block set by hand stays.
rosterUpsert(cfg, agentEntry, (entry) => {
  if (restrictTo) entry.tools = { allow: restrictTo, fs: { workspaceOnly: true } };
  else if (removeRenderedTools) delete entry.tools;
});

// nodeHost.browserProxy — enable + add profile (de-dup)
cfg.nodeHost = cfg.nodeHost || {};
cfg.nodeHost.browserProxy = cfg.nodeHost.browserProxy || {};
cfg.nodeHost.browserProxy.enabled = true;
const allow = Array.isArray(cfg.nodeHost.browserProxy.allowProfiles) ? cfg.nodeHost.browserProxy.allowProfiles : [];
if (!allow.includes(profile)) allow.push(profile);
cfg.nodeHost.browserProxy.allowProfiles = allow;

// web_search needs a PROVIDER, and declaring the tool never supplied
// one. spec.tools.allow was read in exactly one place — to write a
// line of documentation into WIKI.md — so an agent could list
// web_search, look correctly configured, and get "web_search is
// disabled or no provider is available" on every single call. The
// nl2sql newsroom ran that way for 38 hours: four cron schedules
// firing on time, every sweep reporting the provider missing, zero
// articles published.
//
// duckduckgo needs no API key. Up to 2026.7.x it is bundled; from
// 2026.8.1 it is the official plugin @openclaw/duckduckgo-plugin, which
// the image's start-up repair installs when this entry names it. An
// existing provider is never overwritten, so a deliberate Brave or
// Perplexity choice survives, and the plugin entry is created only when
// absent. Idempotent either way.
// Tools an agent declares that are NOT in the runtime's default
// profile have to be added additively. The default profile is
// "coding", which excludes group:ui — so browser and canvas are
// absent unless asked for, which is why the newsroom agents could be
// paired to a browser-capable node host and still answer
// BROWSER_TOOL_MISSING. alsoAllow adds without restricting: a plain
// allow list would strip the newsroom__* MCP tools these agents need.
// The runtime rejects allow and alsoAllow in the same scope.
const additive = declaredTools.filter(t => ["browser", "canvas", "screen", "terminal"].includes(t));
if (additive.length) {
  cfg.tools = cfg.tools || {};
  delete cfg.tools.allow;
  const also = Array.isArray(cfg.tools.alsoAllow) ? cfg.tools.alsoAllow : [];
  for (const t of additive) if (!also.includes(t)) also.push(t);
  cfg.tools.alsoAllow = also;
}

if (declaredTools.includes("web_search")) {
  cfg.tools = cfg.tools || {};
  cfg.tools.web = cfg.tools.web || {};
  cfg.tools.web.fetch = cfg.tools.web.fetch || {};
  cfg.tools.web.search = cfg.tools.web.search || {};
  if (!cfg.tools.web.search.provider) cfg.tools.web.search.provider = searchProvider;
  const prov = cfg.tools.web.search.provider;
  cfg.plugins = cfg.plugins || {};
  cfg.plugins.entries = cfg.plugins.entries || {};
  if (prov && !cfg.plugins.entries[prov]) cfg.plugins.entries[prov] = { enabled: true };
}

// bindings — strip stale-for-this-agent (any rule keyed on this
// agent gets replaced by the one fresh write below), strip wide
// catch-alls (transport-only with no peer/guild/team — they
// shadow specific routes), then re-add a single peer-specific
// entry if we have a real channel id from the AW spec.
function isWideCatchAll(b) {
  if (!b || !b.match) return true;
  const m = b.match;
  const hasPeer = m.peer && m.peer.id;
  const hasGuild = m.guildId;
  const hasTeam = m.teamId;
  const accountWild = !m.accountId || m.accountId === "*";
  return !hasPeer && !hasGuild && !hasTeam && accountWild;
}
let bindings = Array.isArray(cfg.bindings) ? cfg.bindings : [];
bindings = bindings.filter(b => {
  if (!b || !b.match) return false;
  if (b.agentId === agentId) return false;     // this agent's old rule(s) — re-added fresh below
  if (isWideCatchAll(b)) return false;         // wide catch-alls shadow specifics
  return true;
});
if (channelId) {
  bindings.push({
    agentId,
    match: {
      channel: "discord",
      accountId: "default",
      peer: { kind: "channel", id: channelId },
    },
  });
}
// Stable sort by (agentId, peer.id) for deterministic file content.
bindings.sort((a,b) => {
  const aKey = (a.agentId || "") + "\t" + ((a.match && a.match.peer && a.match.peer.id) || "");
  const bKey = (b.agentId || "") + "\t" + ((b.match && b.match.peer && b.match.peer.id) || "");
  return aKey.localeCompare(bKey);
});
cfg.bindings = bindings;

const after = JSON.stringify(cfg);
const fmtLabel = rosterKeyed(cfg) ? "entries" : "list";
if (after === before) {
  console.log("NO_CHANGE agent=" + agentId + " profile=" + profile + " channel=" + (channelId || "(none)") + " roster=" + fmtLabel);
} else {
  fs.writeFileSync(CONFIG_PATH, JSON.stringify(cfg, null, 2));
  console.log("MERGED agent=" + agentId + " profile=" + profile + " channel=" + (channelId || "(none)") + " bindings=" + bindings.length + " roster=" + fmtLabel);
}
`

// agentDeregisterScript removes one agent's footprint from the gateway's
// openclaw.json (roster entry, browser profile, bindings) and its
// workspace directory: the inverse of agentMergeScript. Run as
// `node -e <rosterHelpersJS + agentDeregisterScript> <agentId> <profile>`.
const agentDeregisterScript = `
const fs = require("fs");
const CONFIG_PATH = "/home/node/.openclaw/openclaw.json";
const WORKSPACES_DIR = "/home/node/.openclaw/workspaces/";
const agentId = process.argv[1];
const profile = process.argv[2];
const cfg = JSON.parse(fs.readFileSync(CONFIG_PATH, "utf8"));
const before = JSON.stringify(cfg);
rosterRemove(cfg, agentId);
if (cfg.nodeHost && cfg.nodeHost.browserProxy && Array.isArray(cfg.nodeHost.browserProxy.allowProfiles))
  cfg.nodeHost.browserProxy.allowProfiles = cfg.nodeHost.browserProxy.allowProfiles.filter(x => x !== profile);
if (Array.isArray(cfg.bindings))
  cfg.bindings = cfg.bindings.filter(b => !b || b.agentId !== agentId);
if (JSON.stringify(cfg) !== before) {
  fs.writeFileSync(CONFIG_PATH, JSON.stringify(cfg, null, 2));
  console.log("de-registered " + agentId);
} else {
  console.log("noop (already absent)");
}
try { fs.rmSync(WORKSPACES_DIR + agentId, { recursive: true, force: true }); } catch (e) {}
`
