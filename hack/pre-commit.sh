#!/usr/bin/env bash
# Pre-commit hook for agent-office-operator.
#
# Run `make install-hooks` once after cloning to symlink this into
# .git/hooks/pre-commit. After that, every `git commit` runs preflight
# and refuses to proceed if any structural check fails.
#
# WHY THIS EXISTS: see Makefile `preflight` target — fifteen-hours-of-
# debugging incidents that were caused by silent drift between files
# that all had to agree (Makefile VERSION vs CSV vs catalog vs tekton
# vs trainer-image-pin). Each desync produced a far-downstream error
# message that pointed nowhere near the actual problem.
#
# Skip the hook (for emergencies only — fix the underlying issue ASAP):
#   git commit --no-verify

set -e

# Honor the user's choice if they're already inside a noverify path.
if [ "${SKIP_PREFLIGHT:-0}" = "1" ]; then
  echo "[pre-commit] SKIP_PREFLIGHT=1 — skipping preflight checks"
  exit 0
fi

# Only run preflight if any of the files preflight actually checks are
# staged. Avoids slowing down unrelated commits (e.g. README edits).
TOUCHED=$(git diff --cached --name-only)
RELEVANT_PATTERN='^(Makefile|bundle/manifests/agent-office-operator\.clusterserviceversion\.yaml|catalog/agent-office-operator/catalog\.yaml|config/manager/kustomization\.yaml|\.tekton/operator-(image|bundle|catalog)-on-push\.yaml|internal/controller/(pipeline\.yaml|autoresearchproject_controller\.go)|scripts/autoresearch-pipeline/pipeline\.yaml)$'

if ! echo "$TOUCHED" | grep -qE "$RELEVANT_PATTERN"; then
  echo "[pre-commit] no preflight-relevant files staged — skipping"
  exit 0
fi

echo "[pre-commit] running 'make preflight' (set SKIP_PREFLIGHT=1 to bypass)"
echo
if ! make preflight; then
  echo
  echo "[pre-commit] preflight FAILED — refusing to commit."
  echo "             fix the issues above, re-stage, and try again."
  echo "             emergency bypass: SKIP_PREFLIGHT=1 git commit ..."
  exit 1
fi
