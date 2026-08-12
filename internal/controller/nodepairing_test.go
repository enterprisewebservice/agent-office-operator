package controller

import (
	"encoding/json"
	"testing"
)

// The exact payload openclaw 2026.7.1 returns for the newsroom node
// host — role "node", no caps field at all. The old code filtered on
// caps and therefore matched nothing.
const devicesListJSON = `{
  "pending": [{
    "requestId": "b0155147-50f6-43de-b12c-1b4f55d3e3fc",
    "deviceId": "2bd218a9e7cb",
    "displayName": "fedora-black-zebra-36-newsroom",
    "platform": "linux",
    "clientId": "node-host",
    "clientMode": "node",
    "role": "node",
    "roles": ["node"],
    "scopes": []
  }],
  "paired": []
}`

func TestDevicesTableYieldsNodePairing(t *testing.T) {
	var dl struct {
		Pending []pendingPairing `json:"pending"`
	}
	if err := json.Unmarshal([]byte(devicesListJSON), &dl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := matchNodePairing(dl.Pending, "fedora-black-zebra-36-newsroom")
	if got != "b0155147-50f6-43de-b12c-1b4f55d3e3fc" {
		t.Errorf("devices-table node host not matched, got %q", got)
	}
	// Partial name (how research-gateway is configured) must also match.
	if got := matchNodePairing(dl.Pending, "fedora-black-zebra-36"); got == "" {
		t.Error("substring nodeHostRef name should still match")
	}
	// A different node host must NOT be approved.
	if got := matchNodePairing(dl.Pending, "some-other-vm"); got != "" {
		t.Errorf("approved a non-matching node: %q", got)
	}
}

func TestLegacyNodesTableStillWorks(t *testing.T) {
	// Older runtimes: caps-identified, no role field.
	legacy := []pendingPairing{{
		RequestID: "old-1", DisplayName: "fedora-black-zebra-36-nodehost",
		Caps: []string{"browser", "system"},
	}}
	if got := matchNodePairing(legacy, "fedora-black-zebra-36"); got != "old-1" {
		t.Errorf("legacy caps path broken, got %q", got)
	}
}

func TestNonNodeDevicesAreNeverApproved(t *testing.T) {
	// A phone pairing that happens to share the name must be ignored —
	// auto-approval has to stay narrow.
	phone := []pendingPairing{{
		RequestID: "phone-1", DisplayName: "fedora-black-zebra-36-phone",
		Role: "user", Roles: []string{"user"}, ClientMode: "app", ClientID: "mobile",
	}}
	if got := matchNodePairing(phone, "fedora-black-zebra-36"); got != "" {
		t.Errorf("auto-approved a non-node device: %q", got)
	}
}
