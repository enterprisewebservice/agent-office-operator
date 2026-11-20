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
	"regexp"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
)

// This file is everything the operator knows about WHICH OpenClaw a
// gateway runs. Until v1.7.86 it knew nothing: every script wrote the
// 2026.7.x config shape. OpenClaw changed that shape in ways a 2026.7.x
// writer cannot survive, so the operator now asks the runtime that will
// read the file.
//
// The version is read from the image itself (/app/package.json inside
// the gateway container), never from the image tag: tags are whatever a
// registry mirror calls them (`:latest`, a digest, `v2026.7.1`).
//
// What changes with the version (all checked against the 2026.10.33
// documentation and built schema on 20 Nov 2026):
//
//   - 2026.8.1 replaced the array `agents.list` (entries carry `id`) with
//     the keyed map `agents.entries.<id>` (an `id` inside an entry is an
//     unknown key) and requires `agents.ownership: "explicit"` for two or
//     more agents. A file that still HAS an `agents.list` key is refused
//     by ordinary config reads: hot reload is skipped, and every CLI call
//     (`openclaw agent`, `cron list`, `config validate`) exits 1. At the
//     next start the image's own `doctor --fix` deletes the list when
//     `agents.entries` exists, and agents that were only in the list are
//     gone. See rosterHelpersJS.
//
//   - 2026.9.5 made the gateway process the single owner of every
//     OpenClaw database. `openclaw models auth list` became an offline
//     command (refused while the gateway runs), `~/.codex/auth.json` is no
//     longer imported, and a rotated OAuth token is kept in the gateway's
//     shared store with inert markers in the peers. The codex-auth-sync
//     sidecar, which writes those tables directly every 30 s, would read
//     a marker as "older" and overwrite a live sign-in with the Secret's
//     stale copy. See gatewayOwnsAuthStore.

// openclawVersion is an OpenClaw calendar version, YYYY.M.P (2026.10.33).
// The zero value means "unknown" and answers false to every gate, which
// keeps the 2026.7.x behaviour.
type openclawVersion struct {
	Year, Month, Patch int
	Raw                string
}

var openclawVersionRe = regexp.MustCompile(`^v?(\d{4})\.(\d{1,2})\.(\d{1,4})`)

// parseOpenClawVersion reads the leading YYYY.M.P of a version string and
// ignores any suffix (`2026.7.1-2`, `2026.11.10-beta.1`).
func parseOpenClawVersion(s string) (openclawVersion, bool) {
	s = strings.TrimSpace(s)
	m := openclawVersionRe.FindStringSubmatch(s)
	if m == nil {
		return openclawVersion{}, false
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	p, _ := strconv.Atoi(m[3])
	return openclawVersion{Year: y, Month: mo, Patch: p, Raw: s}, true
}

// IsZero reports an unknown version.
func (v openclawVersion) IsZero() bool { return v.Year == 0 }

// AtLeast reports v >= y.m.p. An unknown version is never at least anything.
func (v openclawVersion) AtLeast(y, m, p int) bool {
	if v.IsZero() {
		return false
	}
	if v.Year != y {
		return v.Year > y
	}
	if v.Month != m {
		return v.Month > m
	}
	return v.Patch >= p
}

// String is the version as the image reports it, or "unknown".
func (v openclawVersion) String() string {
	if v.IsZero() {
		return "unknown"
	}
	return v.Raw
}

// keyedRoster: the roster is `agents.entries`, not `agents.list`.
func (v openclawVersion) keyedRoster() bool { return v.AtLeast(2026, 8, 1) }

// gatewayOwnsAuthStore: the gateway is the only writer of sign-in state.
// The sidecar must not touch SQLite, and `models auth list` cannot be
// asked of a running gateway.
func (v openclawVersion) gatewayOwnsAuthStore() bool { return v.AtLeast(2026, 9, 5) }

// openclawVersionCmd prints the runtime's version from inside the gateway
// container. `node -p` on the image's own package.json: no OpenClaw
// command is involved, so it works whatever state the config is in.
var openclawVersionCmd = []string{"node", "-p", `require("/app/package.json").version`}

// openclawVersionCache remembers the answer per pod. A pod's image never
// changes, so one exec per pod is enough; entries for dead pods are a few
// bytes each and vanish with the operator process.
var openclawVersionCache sync.Map // pod UID -> openclawVersion

// cachedOpenClawVersion returns the version recorded for pod, if any.
func cachedOpenClawVersion(pod *corev1.Pod) (openclawVersion, bool) {
	if pod == nil {
		return openclawVersion{}, false
	}
	if v, ok := openclawVersionCache.Load(pod.UID); ok {
		return v.(openclawVersion), true
	}
	return openclawVersion{}, false
}

// resolveOpenClawVersion asks the pod once and caches the answer. exec is
// the caller's own "run this in the openclaw container" helper, so both
// reconcilers can use it. A failed or unparseable answer is NOT cached
// (the pod may simply not be ready) and yields the zero version.
func resolveOpenClawVersion(pod *corev1.Pod, exec func(cmd []string) (string, error)) openclawVersion {
	if v, ok := cachedOpenClawVersion(pod); ok {
		return v
	}
	out, err := exec(openclawVersionCmd)
	if err != nil {
		return openclawVersion{}
	}
	// The answer is the last non-empty line: anything the runtime prints
	// first (a warning about a missing env var, say) comes before it.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	v, ok := parseOpenClawVersion(lines[len(lines)-1])
	if !ok {
		return openclawVersion{}
	}
	if pod != nil {
		openclawVersionCache.Store(pod.UID, v)
	}
	return v
}

// rosterHelpersJS is prepended to every in-pod script that reads or
// writes the agent roster in openclaw.json. It decides the format from
// the file and from the runtime in the same container, so the Go side
// never has to.
//
// Rules:
//   - `agents.entries` present            -> keyed (already migrated).
//   - a POPULATED `agents.list`, no entries -> legacy. On 2026.7.x that is
//     the format; on 2026.8.1+ it means Doctor has not migrated yet, and
//     the list is Doctor's to migrate (it also moves bindings and owners).
//   - neither                             -> by runtime version.
//
// In keyed mode the `agents.list` key is always removed, after anything
// still in it has been folded into `agents.entries`: its presence alone,
// even as `[]`, makes 2026.8.1+ refuse the file.
//
// OPENCLAW_VERSION_OVERRIDE exists for the unit tests, which run these
// scripts with plain `node` outside an OpenClaw image.
const rosterHelpersJS = `
function ocRuntimeVersion() {
  if (process.env.OPENCLAW_VERSION_OVERRIDE) return process.env.OPENCLAW_VERSION_OVERRIDE;
  try { return String(require("/app/package.json").version || ""); } catch (e) { return ""; }
}
function ocVersionAtLeast(y, m, p) {
  const mm = /^v?(\d+)\.(\d+)\.(\d+)/.exec(ocRuntimeVersion());
  if (!mm) return false;
  const have = [Number(mm[1]), Number(mm[2]), Number(mm[3])];
  const want = [y, m, p];
  for (let i = 0; i < 3; i++) { if (have[i] !== want[i]) return have[i] > want[i]; }
  return true;
}
function isPlainObject(v) { return v !== null && typeof v === "object" && !Array.isArray(v); }
function rosterKeyed(cfg) {
  const a = isPlainObject(cfg.agents) ? cfg.agents : {};
  if (isPlainObject(a.entries)) return true;
  if (Array.isArray(a.list) && a.list.length > 0) return false;
  return ocVersionAtLeast(2026, 8, 1);
}
function rosterIds(cfg) {
  const a = isPlainObject(cfg.agents) ? cfg.agents : {};
  if (isPlainObject(a.entries)) return Object.keys(a.entries);
  if (Array.isArray(a.list)) return a.list.filter((x) => x && typeof x.id === "string").map((x) => x.id);
  return [];
}
function rosterGet(cfg, id) {
  const a = isPlainObject(cfg.agents) ? cfg.agents : {};
  if (isPlainObject(a.entries)) return isPlainObject(a.entries[id]) ? a.entries[id] : undefined;
  if (Array.isArray(a.list)) return a.list.find((x) => x && x.id === id);
  return undefined;
}
// rosterUpsert merges entry (which carries entry.id) over the agent's
// existing entry, keeping any key the operator does not own (a hand-set
// tools block, say). mutate, when given, is applied to the merged entry
// before it is stored: the place to delete a key.
function rosterUpsert(cfg, entry, mutate) {
  if (!isPlainObject(cfg.agents)) cfg.agents = {};
  const a = cfg.agents;
  if (rosterKeyed(cfg)) {
    const entries = isPlainObject(a.entries) ? a.entries : {};
    if (Array.isArray(a.list)) {
      for (const old of a.list) {
        if (!old || typeof old.id !== "string" || entries[old.id]) continue;
        const { id: _oldId, ...rest } = old;
        entries[old.id] = rest;
      }
    }
    if ("list" in a) delete a.list;
    const { id, ...rest } = entry;
    const merged = { ...(isPlainObject(entries[id]) ? entries[id] : {}), ...rest };
    if (mutate) mutate(merged);
    entries[id] = merged;
    a.entries = entries;
    const ids = Object.keys(entries);
    const marked = ids.filter((k) => entries[k] && entries[k].default === true);
    if (ids.length > 1 && marked.length === 0 && a.ownership !== "explicit") a.ownership = "explicit";
    return;
  }
  const list = Array.isArray(a.list) ? a.list : [];
  const i = list.findIndex((x) => x && x.id === entry.id);
  const merged = i >= 0 ? { ...list[i], ...entry } : { ...entry };
  if (mutate) mutate(merged);
  if (i >= 0) list[i] = merged; else list.push(merged);
  a.list = list;
}
// rosterRemove drops the agent from whichever format the file is in. An
// emptied keyed roster is removed altogether, with its ownership stamp:
// 2026.8.1+ rejects "agents.entries" with no agent in it, and a file
// without a roster is the valid fresh state.
function rosterRemove(cfg, id) {
  if (!isPlainObject(cfg.agents)) return;
  const a = cfg.agents;
  if (isPlainObject(a.entries) && id in a.entries) {
    delete a.entries[id];
    if (Object.keys(a.entries).length === 0) {
      delete a.entries;
      delete a.ownership;
    }
  }
  if (Array.isArray(a.list)) a.list = a.list.filter((x) => !x || x.id !== id);
}
`
