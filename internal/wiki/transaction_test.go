/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package wiki

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gitobject "github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// Tests exercise Transaction against a local filesystem "remote".
// We need to test the COMMIT + PUSH path end-to-end because the
// refspec bug that motivated this package only manifested on push.
// No real GitHub, no auth — go-git's PlainClone works fine against
// a bare repo on disk.

// makeBareRemote creates a temp bare repo seeded with one initial
// commit on branch `main` (file `README.md`). Returns the path
// that callers should treat as the "remote URL".
func makeBareRemote(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "remote.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	// To push to a bare repo we need a non-bare clone that we
	// commit + push from once to give the bare repo an initial
	// `main` branch.
	seed := filepath.Join(tmp, "seed")
	seedRepo, err := git.PlainClone(seed, false, &git.CloneOptions{URL: bare})
	if err != nil {
		// PlainClone of an empty bare repo errors with
		// "remote repository is empty"; that's the expected
		// state. Fall through to PlainInit instead.
		seedRepo, err = git.PlainInit(seed, false)
		if err != nil {
			t.Fatalf("init seed: %v", err)
		}
		if _, err := seedRepo.CreateRemote(&config.RemoteConfig{
			Name: "origin",
			URLs: []string{bare},
		}); err != nil {
			t.Fatalf("seed add remote: %v", err)
		}
	}

	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	w, err := seedRepo.Worktree()
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	if _, err := w.Add("README.md"); err != nil {
		t.Fatalf("seed stage: %v", err)
	}
	if _, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &gitobject.Signature{Name: "t", Email: "t@x", When: time.Now()},
	}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	if err := seedRepo.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{"+refs/heads/master:refs/heads/main"},
	}); err != nil {
		// Some go-git versions default to `master`, others to
		// `main`. Try the other direction.
		if err2 := seedRepo.Push(&git.PushOptions{
			RemoteName: "origin",
			RefSpecs:   []config.RefSpec{"+refs/heads/main:refs/heads/main"},
		}); err2 != nil {
			t.Fatalf("seed push: %v / %v", err, err2)
		}
	}
	return bare
}

// fakeKB returns a KnowledgeBase pointing at the given local URL.
func fakeKB(url string) *agentofficev1alpha1.KnowledgeBase {
	return &agentofficev1alpha1.KnowledgeBase{
		Spec: agentofficev1alpha1.KnowledgeBaseSpec{
			GitMirror: &agentofficev1alpha1.KnowledgeBaseGitMirror{
				URL:    url,
				Branch: "main",
			},
		},
	}
}

// noopMinter is a TokenMinter that returns harmless empty-looking
// BasicAuth. Local filesystem clone doesn't actually validate
// credentials, so this is enough.
func noopMinter(_ context.Context, _ client.Client) (*githttp.BasicAuth, error) {
	return &githttp.BasicAuth{Username: "x-access-token", Password: "fake"}, nil
}

func TestTransaction_WriteCommitPush_RoundTrip(t *testing.T) {
	remote := makeBareRemote(t)
	kb := fakeKB(remote)

	ctx := context.Background()
	tx, err := Begin(ctx, nil, kb, noopMinter)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Close()

	if err := tx.Write("proposals/round-7.yaml", []byte("lora_rank: 16\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tx.Write("proposals/round-7.md", []byte("# round 7\n")); err != nil {
		t.Fatalf("Write md: %v", err)
	}
	if err := tx.CommitAndPush(ctx, "propose: round 7"); err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}

	// Verify the bare remote received the commit by cloning it
	// to a second working dir and inspecting. We pass an explicit
	// ReferenceName because t.TempDir-based bare repos don't have
	// HEAD set up to point at a default branch — go-git's clone
	// otherwise probes for "master" then errors.
	verifyDir := t.TempDir()
	if _, err := git.PlainClone(verifyDir, false, &git.CloneOptions{
		URL:           remote,
		ReferenceName: plumbing.ReferenceName("refs/heads/main"),
		SingleBranch:  true,
	}); err != nil {
		t.Fatalf("verify clone: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(verifyDir, "proposals/round-7.yaml")); err != nil {
		t.Fatalf("verify read yaml: %v", err)
	} else if string(b) != "lora_rank: 16\n" {
		t.Fatalf("verify yaml content: got %q", string(b))
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "proposals/round-7.md")); err != nil {
		t.Fatalf("verify md missing: %v", err)
	}
}

func TestTransaction_Commit_NoChangeReturnsSentinel(t *testing.T) {
	remote := makeBareRemote(t)
	kb := fakeKB(remote)

	tx, err := Begin(context.Background(), nil, kb, noopMinter)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Close()

	// No Write calls. Commit should return ErrNoChange — not a
	// generic error, not nil. Callers depend on this to detect
	// idempotency without inspecting message strings.
	err = tx.Commit("nothing")
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("expected ErrNoChange, got %v", err)
	}
}

func TestTransaction_CommitAndPush_NoChangeIsSuccess(t *testing.T) {
	remote := makeBareRemote(t)
	kb := fakeKB(remote)

	tx, err := Begin(context.Background(), nil, kb, noopMinter)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Close()

	// Writing the file with the SAME content that the seed already
	// has produces a no-op stage → ErrNoChange → CommitAndPush
	// returns nil (treat as success).
	if err := tx.Write("README.md", []byte("# seed\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tx.CommitAndPush(context.Background(), "idempotent"); err != nil {
		t.Fatalf("CommitAndPush of no-op: %v", err)
	}
}

func TestTransaction_Read_MissingFileIsNilNil(t *testing.T) {
	remote := makeBareRemote(t)
	kb := fakeKB(remote)

	tx, err := Begin(context.Background(), nil, kb, noopMinter)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Close()

	b, err := tx.Read("does/not/exist.md")
	if err != nil {
		t.Fatalf("Read missing: expected nil error, got %v", err)
	}
	if b != nil {
		t.Fatalf("Read missing: expected nil bytes, got %q", string(b))
	}
}

func TestTransaction_Read_ExistingFile(t *testing.T) {
	remote := makeBareRemote(t)
	kb := fakeKB(remote)

	tx, err := Begin(context.Background(), nil, kb, noopMinter)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Close()

	b, err := tx.Read("README.md")
	if err != nil {
		t.Fatalf("Read README: %v", err)
	}
	if string(b) != "# seed\n" {
		t.Fatalf("Read README content: got %q", string(b))
	}
}

func TestTransaction_Write_RejectsAbsolutePath(t *testing.T) {
	remote := makeBareRemote(t)
	kb := fakeKB(remote)

	tx, err := Begin(context.Background(), nil, kb, noopMinter)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Close()

	if err := tx.Write("/etc/passwd", []byte("x")); err == nil {
		t.Fatal("expected error on absolute path, got nil")
	}
}

func TestBegin_EmptyURL(t *testing.T) {
	kb := &agentofficev1alpha1.KnowledgeBase{}
	_, err := Begin(context.Background(), nil, kb, noopMinter)
	if err == nil {
		t.Fatal("expected error on empty URL, got nil")
	}
}

func TestBegin_MintFailureSurfaces(t *testing.T) {
	remote := makeBareRemote(t)
	kb := fakeKB(remote)

	failingMinter := func(_ context.Context, _ client.Client) (*githttp.BasicAuth, error) {
		return nil, errors.New("Secret missing")
	}
	_, err := Begin(context.Background(), nil, kb, failingMinter)
	if err == nil {
		t.Fatal("expected mint failure to surface, got nil")
	}
}

func TestClose_Idempotent(t *testing.T) {
	remote := makeBareRemote(t)
	kb := fakeKB(remote)

	tx, err := Begin(context.Background(), nil, kb, noopMinter)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tx.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
