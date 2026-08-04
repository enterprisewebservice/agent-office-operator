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
	"encoding/json"
	"os"
	"strings"
	"testing"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// TestExtractJSONDocumentOnRealCLIOutput uses REAL captured stdout from
// `openclaw models auth list --provider openai --json` on the running
// gateway (testdata/models_auth_list.raw).
//
// The trap this guards: that output opens with a colorized
// `[state-migrations]` banner, so the first '[' in the stream is inside
// the ANSI escape \x1b[32m at byte offset 1 — long before any JSON.
// The pre-existing extractJSON helper returns from that offset and
// yields "[32m[state-migrations]...", which never parses. Credential
// discovery would then fail on every reconcile and the gateway would
// silently keep whatever billing route it already had.
func TestExtractJSONDocumentOnRealCLIOutput(t *testing.T) {
	raw, err := os.ReadFile("testdata/models_auth_list.raw")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// Guard the premise: the naive scan really is wrong here.
	if naive := extractJSON(string(raw)); json.Valid([]byte(naive)) {
		t.Fatalf("fixture no longer reproduces the ANSI trap; naive extractJSON parsed cleanly")
	}

	doc := extractJSONDocument(string(raw))
	if doc == "" {
		t.Fatal("extractJSONDocument found no JSON document in real CLI output")
	}
	var listing authProfileListing
	if err := json.Unmarshal([]byte(doc), &listing); err != nil {
		t.Fatalf("unmarshal extracted doc: %v (doc=%s)", err, doc)
	}
	if len(listing.Profiles) == 0 {
		t.Fatal("no profiles parsed from real CLI output")
	}
	foundOAuth := false
	for _, p := range listing.Profiles {
		if p.Type == "oauth" {
			foundOAuth = true
			if !strings.HasPrefix(p.ID, "openai:") {
				t.Errorf("oauth profile id %q does not look like an openai profile id", p.ID)
			}
		}
	}
	if !foundOAuth {
		t.Error("no oauth profile parsed; subscription pinning would fail")
	}
}

// TestExtractJSONDocumentRejectsGarbage asserts we return "" (so the
// caller raises a real error) rather than handing back a bogus string.
func TestExtractJSONDocumentRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "no json here", "\x1b[32m[warn]\x1b[39m still nothing"} {
		if got := extractJSONDocument(in); got != "" {
			t.Errorf("extractJSONDocument(%q) = %q, want empty", in, got)
		}
	}
}

// TestCanonicalProviderID guards the regression that made every agent
// turn fail silently on OpenClaw 2026.7.x: emitting
// `openai-codex/<model>` in the agent's model string. `openai-codex`
// is a legacy provider id there — it must be rendered as `openai`,
// with the subscription expressed via the gateway's auth.order.
func TestCanonicalProviderID(t *testing.T) {
	cases := []struct {
		in   agentofficev1alpha1.ModelProvider
		want string
	}{
		{agentofficev1alpha1.ModelProviderOpenAICodex, "openai"},
		{agentofficev1alpha1.ModelProviderOpenAI, "openai"},
		{agentofficev1alpha1.ModelProviderAnthropic, "anthropic"},
		{agentofficev1alpha1.ModelProviderSMR, "smr"},
		{agentofficev1alpha1.ModelProviderCustom, "custom"},
	}
	for _, tc := range cases {
		if got := agentofficev1alpha1.CanonicalProviderID(tc.in); got != tc.want {
			t.Errorf("CanonicalProviderID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestResolveModelAuthMode covers the defaulting rule: a gateway that
// mounts codex subscription credentials must default to the
// subscription route. Defaulting to apiKey there is what silently put
// agent traffic on per-token billing.
func TestResolveModelAuthMode(t *testing.T) {
	sub := agentofficev1alpha1.ModelAuthModeSubscription
	key := agentofficev1alpha1.ModelAuthModeAPIKey

	cases := []struct {
		name string
		spec agentofficev1alpha1.AgentGatewaySpec
		want agentofficev1alpha1.ModelAuthMode
	}{
		{
			name: "codex creds mounted implies subscription",
			spec: agentofficev1alpha1.AgentGatewaySpec{CodexCredentialsSecretRef: "codex-subscription-credentials"},
			want: sub,
		},
		{
			name: "no creds and no mode falls back to apiKey",
			spec: agentofficev1alpha1.AgentGatewaySpec{},
			want: key,
		},
		{
			name: "explicit mode beats the codex-creds default",
			spec: agentofficev1alpha1.AgentGatewaySpec{
				CodexCredentialsSecretRef: "codex-subscription-credentials",
				ModelAuth:                 &agentofficev1alpha1.ModelAuthSpec{Mode: key},
			},
			want: key,
		},
		{
			name: "explicit subscription without codex creds is honored",
			spec: agentofficev1alpha1.AgentGatewaySpec{
				ModelAuth: &agentofficev1alpha1.ModelAuthSpec{Mode: sub},
			},
			want: sub,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &agentofficev1alpha1.AgentGateway{Spec: tc.spec}
			if got := resolveModelAuthMode(gw); got != tc.want {
				t.Errorf("resolveModelAuthMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDefaultSubscriptionModelsDeclareChatGPTAPI asserts the
// subscription catalog carries the CURRENT api id.
// `openai-codex-responses` was REMOVED in OpenClaw 2026.7.x —
// config validation rejects it outright, so a stale constant here
// would break every gateway on upgrade.
func TestDefaultSubscriptionModelsIncludeSol(t *testing.T) {
	foundSol := false
	for _, m := range defaultSubscriptionModels {
		if m.ID == "gpt-5.6-sol" {
			foundSol = true
		}
		if m.ID == "" {
			t.Errorf("subscription catalog has an entry with an empty ID: %+v", m)
		}
	}
	if !foundSol {
		t.Error("subscription catalog is missing gpt-5.6-sol; it is subscription-only and must be declared here")
	}
}
