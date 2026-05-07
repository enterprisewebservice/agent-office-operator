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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// kbGitSyncCMName names the ConfigMap holding the sync script +
// bootstrap templates for a single KnowledgeBase. The gateway
// pod's git-sync sidecar mounts this CM at /sync and executes
// /sync/sync.sh.
func kbGitSyncCMName(kbName string) string {
	return kbName + "-wiki-gitsync"
}

// reconcileGitSyncConfigMap ensures the per-KB ConfigMap exists
// and is up to date when GitMirror is configured. The CM holds
// the sync script + a small bundle of bootstrap files
// (mkdocs.yml, catalog-info.yaml, .gitignore, an mkdocs hook
// that adds an "Open in Obsidian" badge to every TechDocs page,
// and a starter README) which the sync script copies into the
// wiki PVC the first time it runs against an empty repo.
//
// Idempotent: ConfigMap.Data is recomputed from the KB spec on
// every reconcile; controller-runtime's CreateOrUpdate suppresses
// no-op updates so we don't churn the gateway sidecar's mount.
func (r *KnowledgeBaseReconciler) reconcileGitSyncConfigMap(ctx context.Context, kb *agentofficev1alpha1.KnowledgeBase) error {
	if kb.Spec.GitMirror == nil {
		return nil // no-op when mirroring is disabled
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: kb.Namespace,
			Name:      kbGitSyncCMName(kb.Name),
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = map[string]string{
			"agentoffice.ai/knowledgebase": kb.Name,
			"app.kubernetes.io/managed-by": "agent-office-operator",
			"app.kubernetes.io/component":  "git-sync",
		}
		cm.Data = map[string]string{
			"sync.sh":              gitSyncScript,
			"mkdocs.yml":           renderMkdocsYAML(kb),
			"catalog-info.yaml":    renderCatalogInfoYAML(kb),
			"obsidian_link.py":     obsidianLinkHook,
			"gitignore":            kbGitignore,
			"README.md":            renderReadme(kb),
		}
		return controllerutil.SetControllerReference(kb, cm, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure git-sync CM %s/%s: %w", kb.Namespace, cm.Name, err)
	}
	return nil
}

// gitSyncContainer returns the sidecar container that syncs a
// single KnowledgeBase's wiki PVC with its git remote. Designed
// to be appended to the gateway pod's containers list — it
// shares the same wiki volume mount the openclaw container uses,
// so agents writing to the PVC and the sidecar reading from it
// are reading/writing the same files (no copy step).
//
// The container is plain alpine/git driving a shell script
// mounted from the per-KB ConfigMap. No Go service, no rsync —
// git itself is the sync layer, and conflicts on the rare
// occasion they happen abort the rebase and retry next cycle.
func gitSyncContainer(kb agentofficev1alpha1.KnowledgeBase) corev1.Container {
	cadence := defaultIfEmpty(kb.Spec.GitMirror.Cadence, "*/5 * * * *")
	branch := defaultIfEmpty(kb.Spec.GitMirror.Branch, "main")
	maxBlobMB := "50"

	envFrom := []corev1.EnvFromSource{}
	if kb.Spec.GitMirror.CredentialsSecretRef != "" {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: kb.Spec.GitMirror.CredentialsSecretRef,
				},
			},
		})
	}

	return corev1.Container{
		Name: "git-sync-" + kb.Name,
		// Fully qualified registry path — OpenShift enforces
		// short-names policy and rejects bare `alpine/git`.
		// Pinned to a known-good tag instead of `:latest` so
		// rollouts are reproducible across reconciles.
		Image:   "docker.io/alpine/git:v2.49.0",
		Command: []string{"/bin/sh", "/sync/sync.sh"},
		Env: []corev1.EnvVar{
			{Name: "WIKI_DIR", Value: kbMountPath(kb.Name)},
			{Name: "REMOTE_URL", Value: kb.Spec.GitMirror.URL},
			{Name: "REMOTE_BRANCH", Value: branch},
			{Name: "CADENCE", Value: cadence},
			{Name: "MAX_BLOB_MB", Value: maxBlobMB},
			{Name: "KB_NAME", Value: kb.Name},
			{Name: "KB_DISPLAY_NAME", Value: defaultIfEmpty(kb.Spec.DisplayName, kb.Name)},
			{Name: "KB_DESCRIPTION", Value: kb.Spec.Description},
		},
		EnvFrom: envFrom,
		VolumeMounts: []corev1.VolumeMount{
			// Shared wiki volume — same one the openclaw
			// container mounts. Both containers see the
			// PVC, no rsync between them.
			{Name: kbVolumeName(kb.Name), MountPath: kbMountPath(kb.Name)},
			// Sync script + bootstrap templates.
			{Name: gitSyncCMVolumeName(kb.Name), MountPath: "/sync", ReadOnly: true},
		},
	}
}

// gitSyncCMVolumeName is the pod-spec volume name for the per-KB
// git-sync ConfigMap mount. Stable across reconciles for
// idempotent volume diffing.
func gitSyncCMVolumeName(kbName string) string {
	return "gitsync-" + kbName
}

// gitSyncVolume is the pod-spec Volume entry pairing with
// gitSyncContainer's CM mount. Caller appends to the gateway
// pod's Volumes list.
func gitSyncVolume(kb agentofficev1alpha1.KnowledgeBase) corev1.Volume {
	mode := int32(0755)
	return corev1.Volume{
		Name: gitSyncCMVolumeName(kb.Name),
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: kbGitSyncCMName(kb.Name),
				},
				DefaultMode: &mode,
			},
		},
	}
}

// findKBsForGateway lists KnowledgeBases attached to the named
// gateway in the gateway's namespace. Helper used by both the KB
// reconciler (to know whose CM to manage) and the gateway
// reconciler (to know which sidecars/volumes to add).
func findKBsForGateway(ctx context.Context, c client.Client, namespace, gatewayName string) ([]agentofficev1alpha1.KnowledgeBase, error) {
	var list agentofficev1alpha1.KnowledgeBaseList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := make([]agentofficev1alpha1.KnowledgeBase, 0, len(list.Items))
	for _, kb := range list.Items {
		if kb.Spec.GatewayRef.Name == gatewayName {
			out = append(out, kb)
		}
	}
	return out, nil
}

// renderMkdocsYAML produces the wiki repo's mkdocs.yml — the
// TechDocs entry point. Includes:
//   - Material theme (default in TechDocs)
//   - The obsidian_link.py hook (renders the "Open in Obsidian"
//     badge on every page)
//   - Standard Markdown extensions for fenced code, admonitions,
//     and tables, all of which Obsidian also renders cleanly so
//     the same Markdown reads well in both places
//
// Written to the wiki PVC by the sync script ONLY if missing —
// users can edit the file in their Obsidian vault and push back;
// we don't clobber.
func renderMkdocsYAML(kb *agentofficev1alpha1.KnowledgeBase) string {
	displayName := kb.Spec.DisplayName
	if displayName == "" {
		displayName = kb.Name
	}
	return fmt.Sprintf(`site_name: %q
site_description: %q

# Backstage TechDocs renders this with the bundled mkdocs Material
# theme. Same Markdown also reads cleanly in Obsidian without any
# vault-specific config.
theme:
  name: material
  features:
    - navigation.instant
    - navigation.tracking
    - navigation.tabs
    - search.suggest

plugins:
  - search

# Adds an "Open in Obsidian" badge at the foot of every rendered
# page that deep-links to obsidian://open?vault=<vault>&file=<path>.
# Harmless on machines without Obsidian — the link just doesn't do
# anything. Vault name defaults to the KB name; users can change
# it in their local Obsidian vault settings without breaking the
# link (Obsidian matches by vault name across machines).
hooks:
  - obsidian_link.py

extra:
  obsidian_vault: %q
  kb_name: %q

markdown_extensions:
  - admonition
  - attr_list
  - md_in_html
  - tables
  - toc:
      permalink: true
  - pymdownx.details
  - pymdownx.superfences
  - pymdownx.snippets

nav:
  - Home: README.md
  - Activity Log: log.md
  - Index: _index.md
`, displayName, kb.Spec.Description, kb.Name, kb.Name)
}

// renderCatalogInfoYAML produces the wiki repo's
// catalog-info.yaml — Backstage's component descriptor. Once
// imported into Backstage's catalog, RHDH builds and serves the
// TechDocs from the same git repo that Obsidian users clone.
func renderCatalogInfoYAML(kb *agentofficev1alpha1.KnowledgeBase) string {
	displayName := kb.Spec.DisplayName
	if displayName == "" {
		displayName = kb.Name
	}
	return fmt.Sprintf(`apiVersion: backstage.io/v1alpha1
kind: Component
metadata:
  name: %s-wiki
  title: %q
  description: %q
  annotations:
    backstage.io/techdocs-ref: dir:.
  tags:
    - knowledge-base
    - agent-office
spec:
  type: documentation
  lifecycle: production
  owner: user:default/deanpeterson
`, kb.Name, displayName, kb.Spec.Description)
}

// renderReadme is the wiki's landing page. Tells human readers
// what the wiki is, who maintains it, and how to view / edit it.
func renderReadme(kb *agentofficev1alpha1.KnowledgeBase) string {
	displayName := kb.Spec.DisplayName
	if displayName == "" {
		displayName = kb.Name
	}
	return fmt.Sprintf(`# %s

%s

This wiki is maintained by AI agents on the cluster. The agents file new
findings, summarize sources, and answer questions against the accumulated
content. You can read it three ways:

- **Backstage TechDocs** — published web view, search, navigation. The
  "Open in Obsidian" badge at the foot of each page deep-links to your
  local Obsidian vault.
- **Obsidian** — clone this repo locally and open the directory as a vault.
  You get graph view, Dataview tables, Marp slides, and the rest of the
  Obsidian feature set against the same Markdown the agents wrote.
- **The cluster gateway pod** — agents read/write the source files on the
  wiki PVC at ` + "`/home/node/.openclaw/wiki/" + kb.Name + "/`" + `, which is
  bidirectionally synced to this git repo every few minutes.

## Wiki layout

- ` + "`raw/clips/`" + ` — unsorted captures (filed by the ` + "`wiki-clip`" + ` skill)
- ` + "`topics/<area>/`" + ` — curated articles (filed by the ` + "`wiki-write`" + ` skill)
- ` + "`concepts/`" + ` — canonical concept pages
- ` + "`queries/`" + ` — Q&A snapshots from past sessions
- ` + "`_index.md`" + ` — top-level manifest, kept fresh by the linter
- ` + "`log.md`" + ` — chronological activity feed,
  ` + "`## [YYYY-MM-DD] <op> | <subject>`" + ` per entry

## How to contribute from your laptop

1. ` + "`git clone <this-repo>`" + ` somewhere on your machine.
2. Open the directory as an Obsidian vault.
3. Edit, paste, clip — Obsidian saves to the same Markdown files agents
   read.
4. ` + "`git push`" + ` — within a few minutes the cluster sidecar pulls your
   changes and agents see them on their next session.

The cluster's ` + "`KnowledgeBase/%s`" + ` resource owns this repo. Don't change the
wiki's structure here without coordinating — agents have skill prompts that
expect the layout above.
`, displayName, kb.Spec.Description, kb.Name)
}

// kbGitignore is the default .gitignore for a wiki repo. Excludes
// per-cycle git artifacts, Obsidian-internal cache, and a small
// allowance for OS metadata. The sync script appends to this list
// dynamically when it sees blobs > MAX_BLOB_MB so a careless
// large-file commit never poisons the repo's clone speed.
const kbGitignore = `# Operator-managed wiki .gitignore.

# Obsidian local-only state; the vault config (.obsidian/) IS
# committed when present so cross-machine settings stay
# consistent, but workspace.json (per-machine layout) is not.
.obsidian/workspace*.json
.obsidian/cache
.obsidian/plugins/*/data.json

# OS metadata
.DS_Store
Thumbs.db

# Common per-cycle agent scratch
*.tmp
*.bak
*.swp

# git-sync sidecar appends large-blob exclusions below this line.
`

// obsidianLinkHook is the mkdocs hook that adds an
// "Open in Obsidian" badge to the footer of every TechDocs page.
// Only stdlib Python; no extra dependencies needed in the
// TechDocs builder image.
//
// Mechanism: mkdocs runs hook scripts before rendering each
// page. We inject a markdown horizontal rule + an HTML anchor
// pointing at obsidian://open?vault=<vault>&file=<src-path>.
// On a machine with Obsidian installed, the OS routes the URL
// to Obsidian and the corresponding note opens. On a machine
// without it, the link is harmless — the browser just no-ops.
const obsidianLinkHook = `"""mkdocs hook: append an "Open in Obsidian" badge to every page.

Reads ` + "`extra.obsidian_vault`" + ` and ` + "`extra.kb_name`" + ` from mkdocs.yml.
The vault name should match the user's local Obsidian vault name —
Obsidian matches by display name, not absolute path, so any user
who clones the wiki repo and adds it as a vault with the matching
name gets working deep-links.
"""

import urllib.parse


def on_page_markdown(markdown, page, config, files):
    extra = config.get("extra", {}) or {}
    vault = extra.get("obsidian_vault", "")
    if not vault:
        return markdown

    # Source path relative to the docs dir (e.g. "topics/inference/foo.md").
    rel = page.file.src_path
    if not rel:
        return markdown

    qs = urllib.parse.urlencode({"vault": vault, "file": rel})
    href = f"obsidian://open?{qs}"

    badge = (
        "\n\n---\n\n"
        f'<p style="text-align: right; opacity: 0.75; font-size: 0.85em;">'
        f'<a href="{href}" title="Open this note in Obsidian (deep-link)">'
        "📓 Open in Obsidian"
        "</a></p>\n"
    )
    return markdown + badge
`

// gitSyncScript is the per-cycle script run by the git-sync
// sidecar. POSIX shell only (alpine/git uses BusyBox), so no
// bash-isms. Annotated heavily because brittle ops here become
// debugging sessions later.
const gitSyncScript = `#!/bin/sh
# Operator-managed git-sync for KnowledgeBase wiki PVCs.
# Reads env: WIKI_DIR REMOTE_URL REMOTE_BRANCH CADENCE MAX_BLOB_MB KB_NAME
# Auth env (from credentialsSecretRef Secret): GIT_TOKEN

set -e

: "${WIKI_DIR:?WIKI_DIR not set}"
: "${REMOTE_URL:?REMOTE_URL not set}"
: "${REMOTE_BRANCH:=main}"
: "${CADENCE:=*/5 * * * *}"
: "${MAX_BLOB_MB:=50}"
: "${KB_NAME:?KB_NAME not set}"

# Cadence here is a number of seconds, not cron syntax. CRDs
# accept cron-shaped strings for human readability; we map a few
# common ones inline. (A real cron would need a proper CronJob;
# the current shape suits a single sidecar with a sleep loop.)
case "$CADENCE" in
  "*/1 * * * *")  CADENCE_SEC=60 ;;
  "*/5 * * * *")  CADENCE_SEC=300 ;;
  "*/15 * * * *") CADENCE_SEC=900 ;;
  "*/30 * * * *") CADENCE_SEC=1800 ;;
  "0 * * * *")    CADENCE_SEC=3600 ;;
  *)              CADENCE_SEC=300 ;;
esac

log() { echo "[git-sync $(date -u +%Y-%m-%dT%H:%M:%SZ)] $*"; }

# Auth setup. We expect a 'token' env from the secretRef holding
# a GitHub Personal Access Token (or fine-grained token) with
# repo:write scope. Inject as a credential-helper line so git
# uses it transparently for any matching host.
mkdir -p "$HOME"
git config --global user.email "agents@agent-office.local"
git config --global user.name "Agent Office"
git config --global pull.rebase true
git config --global rebase.autoStash true

if [ -n "$GIT_TOKEN" ]; then
  # 'x-access-token' user works for fine-grained tokens; for
  # classic PATs the username can be anything non-empty.
  HOST_PATH=$(echo "$REMOTE_URL" | sed -e 's|^https://||')
  echo "https://x-access-token:${GIT_TOKEN}@${HOST_PATH}" > "$HOME/.git-credentials"
  git config --global credential.helper store
fi

cd "$WIKI_DIR"

# Bootstrap on first run: either pull from existing remote or
# create initial commit.
if [ ! -d .git ]; then
  log "no .git in $WIKI_DIR; initializing"
  git init -b "$REMOTE_BRANCH" >/dev/null
  git remote add origin "$REMOTE_URL"
  if git ls-remote --exit-code origin "$REMOTE_BRANCH" >/dev/null 2>&1; then
    log "remote has $REMOTE_BRANCH; pulling existing content"
    git fetch origin "$REMOTE_BRANCH"
    # The wiki PVC may already have agent-written files (skill
    # SKILL_*.md, raw/clips/, etc). Merge remote content with a
    # rebase so any pre-existing local content rides on top.
    git add -A
    if ! git diff --quiet --cached; then
      git commit -m "agents: pre-bootstrap state ($KB_NAME)"
    fi
    git rebase "origin/$REMOTE_BRANCH" || {
      log "WARN rebase conflict on bootstrap; aborting and resetting to remote (preserving local in $WIKI_DIR.local-conflict)"
      git rebase --abort 2>/dev/null || true
      cp -r "$WIKI_DIR" "$WIKI_DIR.local-conflict.$(date -u +%Y%m%dT%H%M%SZ)" 2>/dev/null || true
      git reset --hard "origin/$REMOTE_BRANCH"
    }
  else
    log "remote is empty; will create initial commit on first sync cycle"
  fi
fi

# Bootstrap files: write only if missing, never overwrite. Lets
# users edit mkdocs.yml in Obsidian + push without us clobbering.
write_if_missing() {
  src="$1"
  dst="$2"
  if [ ! -f "$dst" ]; then
    cp "$src" "$dst"
    log "bootstrapped $dst"
  fi
}

write_if_missing "/sync/mkdocs.yml"        "$WIKI_DIR/mkdocs.yml"
write_if_missing "/sync/catalog-info.yaml" "$WIKI_DIR/catalog-info.yaml"
write_if_missing "/sync/obsidian_link.py"  "$WIKI_DIR/obsidian_link.py"
write_if_missing "/sync/README.md"         "$WIKI_DIR/README.md"
write_if_missing "/sync/gitignore"         "$WIKI_DIR/.gitignore"

# Main sync loop. One cycle per CADENCE seconds.
while true; do
  cd "$WIKI_DIR"

  # 1. Pull remote with rebase. Conflicts abort the rebase and
  #    we retry next cycle — agent's local state on the PVC is
  #    preserved.
  if [ -d .git ]; then
    if ! git fetch origin "$REMOTE_BRANCH" 2>/dev/null; then
      log "fetch failed (network/auth); will retry next cycle"
      sleep "$CADENCE_SEC"
      continue
    fi
    if git rev-parse --verify "origin/$REMOTE_BRANCH" >/dev/null 2>&1; then
      git pull --rebase origin "$REMOTE_BRANCH" 2>/dev/null || {
        log "WARN pull --rebase conflict; aborting and retrying next cycle"
        git rebase --abort 2>/dev/null || true
        sleep "$CADENCE_SEC"
        continue
      }
    fi
  fi

  # 2. Append oversized blobs to .gitignore so they never enter
  #    history. POSIX find: use ` + "`-size +<N>M`" + `; alpine has
  #    busybox find which supports this.
  if [ -f .gitignore ]; then
    LARGE=$(find . -type f -size +"${MAX_BLOB_MB}"M -not -path "./.git/*" 2>/dev/null | sed 's|^\./||')
    for f in $LARGE; do
      # Trim leading slash and quote-safety; we know our own
      # paths so just append literal lines.
      if ! grep -qxF "$f" .gitignore 2>/dev/null; then
        echo "$f" >> .gitignore
        log "excluded oversize blob: $f (> ${MAX_BLOB_MB}M)"
      fi
    done
  fi

  # 3. Stage + commit + push if there's something to push.
  git add -A
  if ! git diff --quiet --cached 2>/dev/null; then
    git commit -m "agents: $(date -u +%Y-%m-%dT%H:%M:%SZ)" >/dev/null
    if git push origin "$REMOTE_BRANCH" 2>&1; then
      log "pushed cycle"
    else
      log "WARN push failed; will retry next cycle (likely remote moved between fetch + push)"
    fi
  fi

  sleep "$CADENCE_SEC"
done
`
