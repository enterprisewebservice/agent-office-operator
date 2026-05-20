/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

// Package wiki provides a single, atomic transaction abstraction for
// writes to a KnowledgeBase's git-mirrored repo.
//
// Why this exists: prior to v0.1.0 (Phase 0 of the autoresearch
// refactor), wiki writes were scattered across two ad-hoc helpers
// (`openWikiRepo` + `commitAndPush`) used by `wikiPushProposal` and
// `wikiPushResult`. Both helpers were wrapped in "best-effort"
// error handling at the call sites (`if err != nil { log.Info(...) }`),
// and the push step used a malformed RefSpec
// (`refs/heads/+HEAD:refs/heads/HEAD`) that silently no-op'd for the
// entire lifetime of the operator. Net effect: every project's
// `proposals/round-N.*` and `results/round-N.md` writes silently
// disappeared. No commits ever landed on the remote past the
// initial bootstrap.
//
// This package fixes the class of bug, not just the instance:
//
//   - One function (`Begin`) opens a transaction by cloning the wiki
//     repo via the cluster-shared GitHub App. App auth is the only
//     auth path now (the legacy per-agent PAT branch was removed in
//     v0.0.70).
//
//   - `Write` / `Delete` stage changes against the cloned worktree.
//
//   - `Commit` creates a single commit with the staged changes.
//     Returns `ErrNoChange` when there's nothing to commit so callers
//     don't have to special-case the no-op case.
//
//   - `Push` pushes the checked-out branch to its same name on the
//     remote. RefSpec is computed from `HEAD` (cloned in
//     SingleBranch mode), so the buried-`+` typo can't recur.
//
//   - `Close` removes the tempdir. Always safe to defer.
//
// Failures are never swallowed inside the package. Callers may
// choose to log + continue, but they have to make that decision
// explicitly with full error context — there is no quiet path.
package wiki

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gitobject "github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// TokenMinter mints a fresh App installation token + returns it as
// go-git BasicAuth (Username="x-access-token"). Decoupled as an
// interface so tests can inject a fake without spinning up a real
// GitHub App credential flow. The production wiring uses
// controller.GitHubAppBasicAuth from internal/controller.
type TokenMinter func(ctx context.Context, c client.Client) (*githttp.BasicAuth, error)

// Defaults that match the controller-side helpers. Tests override
// CommitterName/Email to make assertions deterministic; production
// callers can ignore.
const (
	DefaultCommitterName  = "autoresearch-operator"
	DefaultCommitterEmail = "autoresearch@agent-office.local"
)

// ErrNoChange is returned by Commit when the worktree is clean —
// i.e. every Write call was redundant with what's already on the
// branch. Callers should treat this as success (the desired state
// already exists on the remote).
var ErrNoChange = errors.New("wiki: no changes to commit")

// ErrNothingToPush is returned by Push when the local branch is
// already in sync with the remote. Also a success case.
var ErrNothingToPush = errors.New("wiki: nothing to push (already in sync)")

// Transaction represents an open clone of a KnowledgeBase's wiki
// repo. Single-use — once Push or Close has been called, create a
// new Transaction for further work.
type Transaction struct {
	repo   *git.Repository
	dir    string
	auth   *githttp.BasicAuth
	branch string

	// CommitterName / CommitterEmail can be overridden between
	// Begin and Commit. Default to the package constants.
	CommitterName  string
	CommitterEmail string

	// closed guards against double-Close removing an already-removed
	// tempdir. Pointer so the zero value is harmless.
	closed bool
}

// Begin clones the KnowledgeBase's git mirror to a fresh temp dir
// using a freshly-minted App installation token. Caller MUST defer
// `tx.Close()` to clean the tempdir.
//
// Returns an error if:
//   - KB has no `spec.gitMirror.url` set
//   - The GitHub App credential mint fails (Secret missing, key
//     malformed, GitHub returns non-201)
//   - The clone fails (network, auth, branch not found)
//
// The clone is shallow (depth=1) + single-branch so the typical
// reconcile-time wiki commit is cheap and the post-clone HEAD
// reliably points at refs/heads/<branch>.
func Begin(
	ctx context.Context,
	c client.Client,
	kb *agentofficev1alpha1.KnowledgeBase,
	mintToken TokenMinter,
) (*Transaction, error) {
	if kb == nil {
		return nil, fmt.Errorf("wiki.Begin: kb is nil")
	}
	if kb.Spec.GitMirror == nil {
		return nil, fmt.Errorf("wiki.Begin: KnowledgeBase %s/%s has no spec.gitMirror (nil)",
			kb.Namespace, kb.Name)
	}
	if kb.Spec.GitMirror.URL == "" {
		return nil, fmt.Errorf("wiki.Begin: KnowledgeBase %s/%s has empty spec.gitMirror.url",
			kb.Namespace, kb.Name)
	}
	if mintToken == nil {
		return nil, fmt.Errorf("wiki.Begin: TokenMinter is nil")
	}

	auth, err := mintToken(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("wiki.Begin: mint App token for %s/%s: %w",
			kb.Namespace, kb.Name, err)
	}
	if auth == nil || auth.Password == "" {
		return nil, fmt.Errorf("wiki.Begin: TokenMinter returned empty auth for %s/%s",
			kb.Namespace, kb.Name)
	}

	dir, err := os.MkdirTemp("", "wiki-tx-"+kb.Name+"-*")
	if err != nil {
		return nil, fmt.Errorf("wiki.Begin: mktemp: %w", err)
	}

	branch := kb.Spec.GitMirror.Branch
	if branch == "" {
		branch = "main"
	}

	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:           kb.Spec.GitMirror.URL,
		Auth:          auth,
		ReferenceName: plumbing.ReferenceName("refs/heads/" + branch),
		Depth:         1,
		SingleBranch:  true,
	})
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("wiki.Begin: clone %s (branch %s): %w",
			kb.Spec.GitMirror.URL, branch, err)
	}

	return &Transaction{
		repo:           repo,
		dir:            dir,
		auth:           auth,
		branch:         branch,
		CommitterName:  DefaultCommitterName,
		CommitterEmail: DefaultCommitterEmail,
	}, nil
}

// Read returns the contents of the file at `relPath` (relative to
// the wiki root). Returns (nil, nil) — not an error — if the file
// doesn't exist, so callers can use Read to check existence
// without an explicit Stat. Any other I/O error is surfaced.
func (tx *Transaction) Read(relPath string) ([]byte, error) {
	abs := filepath.Join(tx.dir, relPath)
	b, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("wiki.Read %s: %w", relPath, err)
	}
	return b, nil
}

// Write stages content at `relPath` under the wiki root. Parent
// directories are created with mode 0755. The file is written with
// mode 0644. Caller is responsible for ensuring `relPath` is a
// repo-relative path (no leading `/`, no `..` components).
//
// This does NOT commit; call Commit when you're done staging.
func (tx *Transaction) Write(relPath string, content []byte) error {
	if relPath == "" {
		return fmt.Errorf("wiki.Write: relPath is empty")
	}
	if filepath.IsAbs(relPath) {
		return fmt.Errorf("wiki.Write: relPath %q must be repo-relative", relPath)
	}
	abs := filepath.Join(tx.dir, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("wiki.Write: mkdir %s: %w", filepath.Dir(relPath), err)
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		return fmt.Errorf("wiki.Write %s: %w", relPath, err)
	}
	// Stage with git so Commit picks it up.
	w, err := tx.repo.Worktree()
	if err != nil {
		return fmt.Errorf("wiki.Write: worktree: %w", err)
	}
	if _, err := w.Add(relPath); err != nil {
		return fmt.Errorf("wiki.Write: stage %s: %w", relPath, err)
	}
	return nil
}

// Commit creates a single commit with all staged changes.
//
// If the worktree is clean (nothing to commit), returns ErrNoChange.
// Callers should treat that as success — the desired state already
// exists on the branch.
func (tx *Transaction) Commit(msg string) error {
	if msg == "" {
		return fmt.Errorf("wiki.Commit: msg is empty")
	}
	w, err := tx.repo.Worktree()
	if err != nil {
		return fmt.Errorf("wiki.Commit: worktree: %w", err)
	}
	status, err := w.Status()
	if err != nil {
		return fmt.Errorf("wiki.Commit: status: %w", err)
	}
	if status.IsClean() {
		return ErrNoChange
	}
	if _, err := w.Commit(msg, &git.CommitOptions{
		Author: &gitobject.Signature{
			Name:  tx.CommitterName,
			Email: tx.CommitterEmail,
			When:  time.Now(),
		},
	}); err != nil {
		return fmt.Errorf("wiki.Commit %q: %w", msg, err)
	}
	return nil
}

// Push pushes the checked-out branch to its same name on origin
// using a *correct* refspec computed from HEAD (clone was in
// SingleBranch mode, so HEAD reliably points at refs/heads/<branch>).
//
// Returns ErrNothingToPush if the remote is already in sync (also a
// success case). Any other push error is surfaced verbatim.
func (tx *Transaction) Push(ctx context.Context) error {
	head, err := tx.repo.Head()
	if err != nil {
		return fmt.Errorf("wiki.Push: read HEAD: %w", err)
	}
	if !head.Name().IsBranch() {
		return fmt.Errorf("wiki.Push: HEAD is not a branch (got %q)", head.Name())
	}
	branchRef := head.Name().String() // e.g. "refs/heads/main"
	refSpec := config.RefSpec("+" + branchRef + ":" + branchRef)

	err = tx.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		Auth:       tx.auth,
		RefSpecs:   []config.RefSpec{refSpec},
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return ErrNothingToPush
	}
	if err != nil {
		return fmt.Errorf("wiki.Push %s: %w", refSpec, err)
	}
	return nil
}

// CommitAndPush is the convenience path most callers want — stage
// nothing additional, just commit any pending stages and push.
// Returns nil for both ErrNoChange and ErrNothingToPush since those
// are success cases (the remote already reflects the intended
// state).
func (tx *Transaction) CommitAndPush(ctx context.Context, msg string) error {
	if err := tx.Commit(msg); err != nil {
		if errors.Is(err, ErrNoChange) {
			return nil
		}
		return err
	}
	if err := tx.Push(ctx); err != nil {
		if errors.Is(err, ErrNothingToPush) {
			return nil
		}
		return err
	}
	return nil
}

// Dir returns the on-disk path of the cloned wiki worktree. Useful
// for tests that want to assert file contents; production callers
// should prefer Read/Write so the package owns the file layout.
func (tx *Transaction) Dir() string { return tx.dir }

// Branch returns the branch name the transaction is bound to.
func (tx *Transaction) Branch() string { return tx.branch }

// Close removes the tempdir. Safe to call more than once and safe
// to defer immediately after a successful Begin (even if Push
// failed). Returns the tempdir-cleanup error, if any — callers
// usually ignore it.
func (tx *Transaction) Close() error {
	if tx.closed {
		return nil
	}
	tx.closed = true
	if tx.dir == "" {
		return nil
	}
	return os.RemoveAll(tx.dir)
}
