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

// The nodes table returns a bare array whose entries are caps-shaped —
// this is the SECOND stage, the one that actually grants capability.
const nodesPendingJSON = `[{
  "requestId": "635e1c1a-2044-4848-9389-4c8e3fd613be",
  "displayName": "fedora-black-zebra-36-newsroom",
  "platform": "linux",
  "caps": ["system", "browser", "file", "local-inference"]
}]`

func TestNodesStagePendingIsMatched(t *testing.T) {
	var bare []pendingPairing
	if err := json.Unmarshal([]byte(nodesPendingJSON), &bare); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := matchNodePairing(bare, "fedora-black-zebra-36-newsroom")
	if got != "635e1c1a-2044-4848-9389-4c8e3fd613be" {
		t.Errorf("nodes-stage request not matched, got %q", got)
	}
}

// Both stages describe the SAME node host and both must be approvable —
// approving only the devices stage leaves a connected node with empty
// caps that refuses every invoke.
func TestBothStagesResolveForTheSameNode(t *testing.T) {
	var dl struct {
		Pending []pendingPairing `json:"pending"`
	}
	_ = json.Unmarshal([]byte(devicesListJSON), &dl)
	var bare []pendingPairing
	_ = json.Unmarshal([]byte(nodesPendingJSON), &bare)

	name := "fedora-black-zebra-36-newsroom"
	stage1 := matchNodePairing(dl.Pending, name)
	stage2 := matchNodePairing(bare, name)
	if stage1 == "" || stage2 == "" {
		t.Fatalf("both stages must match: devices=%q nodes=%q", stage1, stage2)
	}
	if stage1 == stage2 {
		t.Error("the two stages carry DIFFERENT request ids; treating them as one is the bug")
	}
}
