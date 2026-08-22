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

package templates

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// The rendered openclaw.json is seeded into a gateway's PVC once and
// read by OpenClaw at process start. A malformed render does not fail
// here — it fails as a gateway that never becomes Ready, with the
// operator unable to exec a repair into a pod that cannot boot (the
// v1.7.37/v1.7.38 deadlock). So every render path gets parsed as JSON.

func testGateway() *agentofficev1alpha1.AgentGateway {
	return &agentofficev1alpha1.AgentGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "newsroom-gateway", Namespace: "agent-office"},
	}
}

func render(t *testing.T, hooks *HooksRender) map[string]interface{} {
	t.Helper()
	out, err := RenderAgentGatewayConfig(testGateway(), "gw-token-literal", "apps.example.com", hooks)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v\n%s", err, out)
	}
	return cfg
}

func TestRenderAgentGatewayConfig_NoHooksBlockWhenUnset(t *testing.T) {
	cfg := render(t, nil)
	if _, has := cfg["hooks"]; has {
		t.Fatal("a gateway with spec.hooks unset must not get a hooks block")
	}
	for _, k := range []string{"gateway", "models", "agents", "bindings"} {
		if _, ok := cfg[k]; !ok {
			t.Errorf("base config lost top-level key %q", k)
		}
	}
}

func TestRenderAgentGatewayConfig_HooksDisabledRendersNothing(t *testing.T) {
	hooks := HooksFromSpec(&agentofficev1alpha1.HooksSpec{Enabled: false})
	if hooks == nil {
		t.Fatal("a declared-but-disabled spec must still produce a render contract (the converge script needs it)")
	}
	cfg := render(t, hooks)
	if _, has := cfg["hooks"]; has {
		t.Fatal("hooks.enabled=false must render no hooks block: an absent block is already disabled")
	}
}

func TestRenderAgentGatewayConfig_HooksTokenIsAnEnvReference(t *testing.T) {
	spec := &agentofficev1alpha1.HooksSpec{
		Enabled:         true,
		TokenSecretRef:  &agentofficev1alpha1.HooksTokenSecretRef{Name: "newsroom-hooks", Key: "token"},
		AllowedAgentIDs: []string{"nl2sql-wire"},
	}
	hooks := HooksFromSpec(spec)
	cfg := render(t, hooks)

	raw, has := cfg["hooks"]
	if !has {
		t.Fatal("enabled hooks must render a hooks block")
	}
	h := raw.(map[string]interface{})
	if h["enabled"] != true {
		t.Errorf("hooks.enabled = %v, want true", h["enabled"])
	}
	if h["token"] != "${OPENCLAW_HOOKS_TOKEN}" {
		t.Errorf("hooks.token = %v, want the env reference %q", h["token"], HooksTokenRef)
	}
	if h["path"] != "/hooks" {
		t.Errorf("hooks.path = %v, want the default /hooks", h["path"])
	}
	if got := h["allowedAgentIds"]; !reflect.DeepEqual(got, []interface{}{"nl2sql-wire"}) {
		t.Errorf("hooks.allowedAgentIds = %v, want [nl2sql-wire]", got)
	}
	if h["allowRequestSessionKey"] != false {
		t.Errorf("hooks.allowRequestSessionKey = %v, want an explicit false", h["allowRequestSessionKey"])
	}
	// The Secret's NAME is config metadata, not secret material, but it
	// has no business in openclaw.json either — nothing about the Secret
	// should be rendered except the env reference.
	out, _ := RenderAgentGatewayConfig(testGateway(), "gw-token-literal", "", hooks)
	if strings.Contains(out, "newsroom-hooks") {
		t.Error("rendered config mentions the token Secret; only the env reference belongs there")
	}
}

// Merge, don't clobber: adding the hooks block must leave every other
// rendered key byte-for-byte identical to the no-hooks render.
func TestRenderAgentGatewayConfig_HooksDoNotDisturbOtherKeys(t *testing.T) {
	base := render(t, nil)
	with := render(t, HooksFromSpec(&agentofficev1alpha1.HooksSpec{
		Enabled:        true,
		TokenSecretRef: &agentofficev1alpha1.HooksTokenSecretRef{Name: "x"},
		Path:           "/ingress",
	}))
	delete(with, "hooks")
	if !reflect.DeepEqual(base, with) {
		a, _ := json.Marshal(base)
		b, _ := json.Marshal(with)
		t.Errorf("hooks render changed unrelated keys:\n base=%s\n with=%s", a, b)
	}
}

func TestHooksFromSpec_DefaultsAndCopies(t *testing.T) {
	if HooksFromSpec(nil) != nil {
		t.Fatal("nil spec must map to nil (hands off the hooks block)")
	}
	ids := []string{"a", "b"}
	h := HooksFromSpec(&agentofficev1alpha1.HooksSpec{Enabled: true, AllowedAgentIDs: ids, Path: "/custom"})
	if h.Path != "/custom" {
		t.Errorf("path = %q, want /custom", h.Path)
	}
	if h.TokenEnvVar != HooksTokenEnvVar {
		t.Errorf("tokenEnvVar = %q, want %q", h.TokenEnvVar, HooksTokenEnvVar)
	}
	ids[0] = "mutated"
	if h.AllowedAgentIDs[0] != "a" {
		t.Error("HooksFromSpec must copy allowedAgentIds, not alias the spec slice")
	}
	if HooksFromSpec(&agentofficev1alpha1.HooksSpec{Enabled: true}).Path != DefaultHooksPath {
		t.Error("empty path must default to /hooks")
	}
	if HooksFromSpec(&agentofficev1alpha1.HooksSpec{Enabled: true}).AllowedAgentIDs != nil {
		t.Error("omitted allowedAgentIds must stay nil (unmanaged), not become an empty deny-all list")
	}
}

// The converge script receives HooksRender as JSON; pin the wire shape
// so a renamed field cannot silently stop the script from seeing it.
func TestHooksRender_WireShape(t *testing.T) {
	b, err := json.Marshal(HooksFromSpec(&agentofficev1alpha1.HooksSpec{Enabled: true, AllowedAgentIDs: []string{"w"}}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"enabled":true,"tokenEnvVar":"OPENCLAW_HOOKS_TOKEN","path":"/hooks","allowedAgentIds":["w"],"allowRequestSessionKey":false}`
	if string(b) != want {
		t.Errorf("wire shape drifted:\n got=%s\nwant=%s", b, want)
	}
}
