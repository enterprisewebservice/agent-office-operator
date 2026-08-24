/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"

	"github.com/enterprisewebservice/agent-office-operator/internal/templates"
)

// The sidecar must run the seed loop under the GATEWAY's own image —
// that image is the one place node:sqlite is guaranteed to match the
// runtime that owns the agent stores — and must be able to reach every
// path the script touches: the Secret projection, the writable .codex/
// emptyDir, the workspace PVC holding the per-agent sqlite stores, and
// the config CM carrying the script itself.
func TestCodexAuthSyncContainerWiring(t *testing.T) {
	const img = "example.test/openclaw:v9"
	c := codexAuthSyncContainer(img)

	if c.Image != img {
		t.Errorf("sidecar image = %q, want the gateway image %q", c.Image, img)
	}
	if len(c.Command) != 2 || c.Command[0] != "node" || c.Command[1] != codexAuthSyncScriptPath {
		t.Errorf("sidecar command = %v, want [node %s]", c.Command, codexAuthSyncScriptPath)
	}

	wantMounts := map[string]string{
		"codex-auth-src": "/var/lib/codex-auth",
		"codex-home":     "/home/node/.codex",
		"workspace":      "/home/node/.openclaw",
		"config":         codexAuthSyncScriptDir,
	}
	got := map[string]string{}
	for _, m := range c.VolumeMounts {
		got[m.Name] = m.MountPath
	}
	for name, path := range wantMounts {
		if got[name] != path {
			t.Errorf("mount %q at %q, want %q", name, got[name], path)
		}
	}
}

// Guard the embed: a renamed or mis-pathed script file would compile
// to an empty (or wrong) CM key and the sidecar would crash-loop on a
// missing file. Marker strings pin the parts the operator's contract
// depends on: the store tables, the agent-store path layout, and the
// two files the mirror step copies between.
func TestCodexAuthSyncScriptEmbedded(t *testing.T) {
	s := templates.CodexAuthSyncScript
	for _, marker := range []string{
		"node:sqlite",
		"auth_profile_store",
		"auth_profile_state",
		"/home/node/.openclaw/agents",
		"/var/lib/codex-auth/auth.json",
		"/home/node/.codex/auth.json",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded codex-auth-sync.mjs lacks %q", marker)
		}
	}
	if h := codexAuthSyncScriptHash(); len(h) != 12 {
		t.Errorf("script hash = %q, want 12 hex chars", h)
	}
}
