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
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- where the routers reach pods from ------------------------------------

func TestOVNManagementIPs(t *testing.T) {
	cases := []struct {
		name, annotation string
		want             []string
		wantErr          bool
	}{
		{"list form", `{"default":["10.130.2.0/23"]}`, []string{"10.130.2.2"}, false},
		{"bare string form of older releases", `{"default":"10.128.4.0/23"}`, []string{"10.128.4.2"}, false},
		{"dual stack", `{"default":["10.129.0.0/23","fd01:0:0:5::/64"]}`, []string{"10.129.0.2", "fd01:0:0:5::2"}, false},
		{"other networks are not how a router reaches a gateway", `{"default":["10.131.0.0/23"],"tenant-blue":["192.168.50.0/24"]}`, []string{"10.131.0.2"}, false},
		{"a host address inside the subnet is read as its network", `{"default":["10.0.255.254/15"]}`, []string{"10.0.0.2"}, false},
		{"no default network", `{"tenant-blue":["192.168.50.0/24"]}`, nil, true},
		{"a /31 has no room for a management port", `{"default":["10.9.9.0/31"]}`, nil, true},
		{"not JSON", `10.130.2.0/23`, nil, true},
		{"empty", ``, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ovnManagementIPs(c.annotation)
			if (err != nil) != c.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func routerPod(name string, hostNetwork bool, ips ...string) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ingressRouterNamespace, Labels: map[string]string{ingressRouterLabel: "default"}},
		Spec:       corev1.PodSpec{HostNetwork: hostNetwork},
	}
	for _, ip := range ips {
		p.Status.PodIPs = append(p.Status.PodIPs, corev1.PodIP{IP: ip})
	}
	return p
}

func nodeWithSubnet(name, annotation string) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if annotation != "" {
		n.Annotations = map[string]string{ovnNodeSubnetsAnnotation: annotation}
	}
	return n
}

// The bare-metal cluster this was written on: two host-network routers,
// eight nodes. The expected list is the one that made both routers and
// the in-cluster help desk caller work on OpenClaw 2026.10.33 when it was
// set by hand (20 Nov 2026).
func TestIngressSourcesFrom_HostNetworkRouters(t *testing.T) {
	routers := []corev1.Pod{
		routerPod("router-default-a", true, "192.168.1.149"),
		routerPod("router-default-b", true, "192.168.1.26"),
	}
	nodes := []corev1.Node{
		nodeWithSubnet("18-c0-4d-87-74-43", `{"default":["10.130.2.0/23"]}`),
		nodeWithSubnet("3c-ec-ef-30-cc-2b.localdomain", `{"default":["10.129.0.0/23"]}`),
		nodeWithSubnet("3c-ec-ef-75-fb-d2", `{"default":["10.129.2.0/23"]}`),
		nodeWithSubnet("d8-43-ae-5b-c9-9a", `{"default":["10.128.2.0/23"]}`),
		nodeWithSubnet("ec-0d-9a-2e-71-28", `{"default":["10.131.0.0/23"]}`),
		nodeWithSubnet("ec-0d-9a-89-19-46", `{"default":["10.128.4.0/23"]}`),
		nodeWithSubnet("ec-0d-9a-92-a4-c2", `{"default":["10.128.0.0/23"]}`),
		nodeWithSubnet("ec-0d-9a-92-a5-6a", `{"default":["10.130.0.0/23"]}`),
	}
	got, err := ingressSourcesFrom(routers, nodes)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.128.0.2", "10.128.2.2", "10.128.4.2", "10.129.0.2", "10.129.2.2", "10.130.0.2", "10.130.2.2", "10.131.0.2"}
	if !reflect.DeepEqual(got.Addresses, want) {
		t.Fatalf("addresses\n got %v\nwant %v", got.Addresses, want)
	}
	// The routers' own (node) addresses are NOT what a pod sees them from.
	for _, a := range got.Addresses {
		if strings.HasPrefix(a, "192.168.") {
			t.Fatalf("a node address leaked into the list: %v", got.Addresses)
		}
	}
	if !strings.Contains(got.Basis, "8 node subnet") {
		t.Fatalf("basis = %q", got.Basis)
	}
}

func TestIngressSourcesFrom_RoutersOnThePodNetwork(t *testing.T) {
	gone := routerPod("router-default-old", false, "10.131.9.9")
	now := metav1.Now()
	gone.DeletionTimestamp = &now
	routers := []corev1.Pod{
		routerPod("router-default-b", false, "10.129.4.17"),
		routerPod("router-default-a", false, "10.128.6.40"),
		gone,
	}
	// Nodes are not consulted at all when no router is on the host network.
	got, err := ingressSourcesFrom(routers, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.128.6.40", "10.129.4.17"}; !reflect.DeepEqual(got.Addresses, want) {
		t.Fatalf("got %v, want %v (a terminating router must not be trusted)", got.Addresses, want)
	}
}

func TestIngressSourcesFrom_NotDetermined(t *testing.T) {
	if _, err := ingressSourcesFrom(nil, nil); err == nil {
		t.Fatal("no router pods: the answer must be 'not determined', not an empty list")
	}
	// Host-network routers on a cluster whose nodes carry no OVN subnet
	// annotation: say how to settle it by hand.
	_, err := ingressSourcesFrom(
		[]corev1.Pod{routerPod("router-default-a", true, "192.168.1.149")},
		[]corev1.Node{nodeWithSubnet("n1", ""), nodeWithSubnet("n2", "")},
	)
	if err == nil || !strings.Contains(err.Error(), trustedProxiesEnv) {
		t.Fatalf("want an error naming %s, got %v", trustedProxiesEnv, err)
	}
	// One unreadable node does not hide the others.
	got, err := ingressSourcesFrom(
		[]corev1.Pod{routerPod("router-default-a", true, "192.168.1.149")},
		[]corev1.Node{nodeWithSubnet("n1", `{"default":["10.128.0.0/23"]}`), nodeWithSubnet("n2", `garbage`)},
	)
	if err != nil || !reflect.DeepEqual(got.Addresses, []string{"10.128.0.2"}) {
		t.Fatalf("got %v, %v", got.Addresses, err)
	}
}

func TestParseProxyList(t *testing.T) {
	got, err := parseProxyList("10.130.2.2, 10.128.0.2 127.0.0.1\n10.200.0.0/24,::1")
	if err != nil {
		t.Fatal(err)
	}
	// Loopback is added by the caller for every gateway; it is not kept here.
	if want := []string{"10.128.0.2", "10.130.2.2", "10.200.0.0/24"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"router.example.com", "10.0.0.300", " , ", "127.0.0.1"} {
		if _, err := parseProxyList(bad); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
}

func TestSortProxyAddressesIsNumeric(t *testing.T) {
	a := []string{"10.130.2.2", "fd01::2", "10.128.10.2", "10.128.2.2", "10.129.0.0/23", "fd01::10"}
	sortProxyAddresses(a)
	want := []string{"10.128.2.2", "10.128.10.2", "10.129.0.0/23", "10.130.2.2", "fd01::2", "fd01::10"}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("got %v, want %v", a, want)
	}
}

// --- the in-pod script ----------------------------------------------------

// runRuntimeDefaults runs the SHIPPED script on cfg with plain node and
// returns its marker, the config as it is on disk afterwards, and the
// directory (to look for leftovers).
func runRuntimeDefaults(t *testing.T, cfg map[string]interface{}, desired runtimeDefaults) (string, map[string]interface{}, string) {
	t.Helper()
	mustNode(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o640); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	redirected := strings.Replace(runtimeDefaultsScript, rosterConfigPathLine, `const CONFIG_PATH = process.env.TEST_CFG_PATH;`, 1)
	if redirected == runtimeDefaultsScript {
		t.Fatal("could not redirect the config path; the script's CONFIG_PATH line changed")
	}
	d, err := json.Marshal(desired)
	if err != nil {
		t.Fatalf("marshal desired: %v", err)
	}
	cmd := exec.Command("node", "-e", redirected, string(d))
	cmd.Env = []string{"TEST_CFG_PATH=" + cfgPath, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(after, &got); err != nil {
		t.Fatalf("config is not JSON after the script: %v\n%s", err, after)
	}
	return strings.TrimSpace(string(out)), got, dir
}

// Validators for the tests: the runtime's verdict, played by node.
var (
	validatorAccepts = []string{"node", "-e", `JSON.parse(require("fs").readFileSync(process.env.OPENCLAW_CONFIG_PATH,"utf8"))`}
	validatorRefuses = []string{"node", "-e", `console.log("Config invalid: plugins.entries.memory-core.config: unknown key dreaming"); process.exit(1)`}
	// A second writer: changes the LIVE file while "validating" ours.
	validatorWhileSomeoneElseWrites = []string{"node", "-e", `const fs=require("fs");const p=process.env.TEST_CFG_PATH;const c=JSON.parse(fs.readFileSync(p,"utf8"));c.writtenBySomeoneElse=true;fs.writeFileSync(p,JSON.stringify(c,null,2))`}
)

func wantedDefaults(proxies ...string) runtimeDefaults {
	return runtimeDefaults{
		TrustedProxies:           proxies,
		HeartbeatEveryWhenUnset:  defaultHeartbeatEvery,
		DreamingEnabledWhenUnset: ptrBool(false),
		ValidateCmd:              validatorAccepts,
	}
}

// A gateway as the template seeded it on OpenClaw 2026.7.x: the whole pod
// network trusted, no heartbeat or dreaming choice.
func seededGatewayConfig() map[string]interface{} {
	return map[string]interface{}{
		"gateway": map[string]interface{}{
			"mode": "local", "port": float64(18789),
			"auth":           map[string]interface{}{"mode": "token", "token": "not-a-real-token"},
			"trustedProxies": []interface{}{"10.128.0.0/14", "10.131.0.0/16", "127.0.0.1", "::1"},
			"http":           map[string]interface{}{"endpoints": map[string]interface{}{"chatCompletions": map[string]interface{}{"enabled": true}}},
		},
		"agents": map[string]interface{}{
			"defaults": map[string]interface{}{"model": map[string]interface{}{"primary": "openai/gpt-6.2-sol"}},
			"list":     []interface{}{map[string]interface{}{"id": "desk", "tools": map[string]interface{}{"allow": []interface{}{"read"}}}},
		},
		"plugins":  map[string]interface{}{"entries": map[string]interface{}{"memory-core": map[string]interface{}{"config": map[string]interface{}{}}, "duckduckgo": map[string]interface{}{"enabled": true}}},
		"channels": map[string]interface{}{"discord": map[string]interface{}{"enabled": false}},
	}
}

func dig(t *testing.T, m map[string]interface{}, path ...string) interface{} {
	t.Helper()
	var cur interface{} = m
	for _, k := range path {
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = obj[k]
	}
	return cur
}

func TestRuntimeDefaults_NarrowsTheProxyListAndPinsTheTwoJobs(t *testing.T) {
	want := []string{"10.129.2.2", "10.131.0.2", "127.0.0.1", "::1"}
	before := seededGatewayConfig()
	marker, got, dir := runRuntimeDefaults(t, before, wantedDefaults(want...))

	if !strings.HasPrefix(marker, "RECONCILED_RUNTIME changed=") {
		t.Fatalf("marker = %q", marker)
	}
	for _, p := range []string{"gateway.trustedProxies", "agents.defaults.heartbeat.every", "plugins.entries.memory-core.config.dreaming.enabled"} {
		if !strings.Contains(marker, p) {
			t.Fatalf("marker %q does not report %s", marker, p)
		}
	}
	gotList, _ := dig(t, got, "gateway", "trustedProxies").([]interface{})
	if len(gotList) != len(want) {
		t.Fatalf("trustedProxies = %v", gotList)
	}
	for i := range want {
		if gotList[i] != want[i] {
			t.Fatalf("trustedProxies = %v, want %v", gotList, want)
		}
	}
	if v := dig(t, got, "agents", "defaults", "heartbeat", "every"); v != "0m" {
		t.Fatalf("heartbeat.every = %v", v)
	}
	if v := dig(t, got, "plugins", "entries", "memory-core", "config", "dreaming", "enabled"); v != false {
		t.Fatalf("dreaming.enabled = %v", v)
	}

	// Nothing else moved: undo the three writes and the file is the fixture.
	gw := got["gateway"].(map[string]interface{})
	gw["trustedProxies"] = before["gateway"].(map[string]interface{})["trustedProxies"]
	delete(got["agents"].(map[string]interface{})["defaults"].(map[string]interface{}), "heartbeat")
	delete(got["plugins"].(map[string]interface{})["entries"].(map[string]interface{})["memory-core"].(map[string]interface{})["config"].(map[string]interface{}), "dreaming")
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("keys the operator does not own were changed:\n got %v\nwant %v", got, before)
	}

	// The candidate was renamed into place, not left beside the config.
	if _, err := os.Stat(filepath.Join(dir, "openclaw.json.operator-candidate")); !os.IsNotExist(err) {
		t.Fatalf("candidate file left behind (stat err=%v)", err)
	}
	// And the file kept its permissions.
	if st, err := os.Stat(filepath.Join(dir, "openclaw.json")); err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("config mode = %v, err = %v; want 0640", st.Mode().Perm(), err)
	}
}

func TestRuntimeDefaults_SecondPassWritesNothing(t *testing.T) {
	want := wantedDefaults("10.129.2.2", "10.131.0.2", "127.0.0.1", "::1")
	_, once, _ := runRuntimeDefaults(t, seededGatewayConfig(), want)
	marker, twice, _ := runRuntimeDefaults(t, once, want)
	if marker != "NO_CHANGE" {
		t.Fatalf("a converged gateway must not be written again (a write restarts 2026.7.x in process): %q", marker)
	}
	if !reflect.DeepEqual(once, twice) {
		t.Fatal("second pass changed the file")
	}
}

// The list set by hand on runtab-help during its upgrade is the same SET
// in a hand-chosen order. Rewriting it would be a pointless reload.
func TestRuntimeDefaults_SameSetInAnotherOrderIsNotRewritten(t *testing.T) {
	cfg := seededGatewayConfig()
	cfg["gateway"].(map[string]interface{})["trustedProxies"] = []interface{}{"::1", "10.131.0.2", "127.0.0.1", "10.129.2.2"}
	cfg["agents"].(map[string]interface{})["defaults"].(map[string]interface{})["heartbeat"] = map[string]interface{}{"agentId": "desk", "every": "0m"}
	cfg["plugins"].(map[string]interface{})["entries"].(map[string]interface{})["memory-core"] = map[string]interface{}{"config": map[string]interface{}{"dreaming": map[string]interface{}{"enabled": false}}}
	marker, _, _ := runRuntimeDefaults(t, cfg, wantedDefaults("10.129.2.2", "10.131.0.2", "127.0.0.1", "::1"))
	if marker != "NO_CHANGE" {
		t.Fatalf("marker = %q", marker)
	}
}

func TestRuntimeDefaults_AChoiceMadeByHandIsKept(t *testing.T) {
	cfg := seededGatewayConfig()
	cfg["agents"].(map[string]interface{})["defaults"].(map[string]interface{})["heartbeat"] = map[string]interface{}{"every": "2h"}
	cfg["plugins"].(map[string]interface{})["entries"].(map[string]interface{})["memory-core"] = map[string]interface{}{"config": map[string]interface{}{"dreaming": map[string]interface{}{"enabled": true}}}
	marker, got, _ := runRuntimeDefaults(t, cfg, wantedDefaults("10.129.2.2", "127.0.0.1", "::1"))
	if marker != "RECONCILED_RUNTIME changed=gateway.trustedProxies" {
		t.Fatalf("only the proxy list may change: %q", marker)
	}
	if v := dig(t, got, "agents", "defaults", "heartbeat", "every"); v != "2h" {
		t.Fatalf("a chosen heartbeat cadence was overwritten: %v", v)
	}
	if v := dig(t, got, "plugins", "entries", "memory-core", "config", "dreaming", "enabled"); v != true {
		t.Fatalf("a chosen dreaming setting was overwritten: %v", v)
	}
}

func TestRuntimeDefaults_LeavesAloneWhatIsNotThere(t *testing.T) {
	// No agents yet: no heartbeat owner, and a roster-less agents block is
	// not ours to invent. Memory plugin switched off: nothing to decide.
	cfg := map[string]interface{}{
		"gateway": map[string]interface{}{"trustedProxies": []interface{}{"10.129.2.2", "127.0.0.1", "::1"}},
		"plugins": map[string]interface{}{"entries": map[string]interface{}{"memory-core": map[string]interface{}{"enabled": false}}},
	}
	marker, got, _ := runRuntimeDefaults(t, cfg, wantedDefaults("10.129.2.2", "127.0.0.1", "::1"))
	if marker != "NO_CHANGE" {
		t.Fatalf("marker = %q", marker)
	}
	if _, has := got["agents"]; has {
		t.Fatal("an agents block was invented")
	}

	// Routers not determined: the list is not touched, whatever it holds.
	cfg2 := seededGatewayConfig()
	d := wantedDefaults()
	d.TrustedProxies = nil
	marker, got2, _ := runRuntimeDefaults(t, cfg2, d)
	if strings.Contains(marker, "gateway.trustedProxies") {
		t.Fatalf("the proxy list was written without knowing the routers: %q", marker)
	}
	if l, _ := dig(t, got2, "gateway", "trustedProxies").([]interface{}); len(l) != 4 || l[0] != "10.128.0.0/14" {
		t.Fatalf("trustedProxies changed: %v", l)
	}
}

// The roster shape of OpenClaw 2026.8.1 and later.
func TestRuntimeDefaults_KeyedRoster(t *testing.T) {
	cfg := seededGatewayConfig()
	agents := cfg["agents"].(map[string]interface{})
	delete(agents, "list")
	agents["ownership"] = "explicit"
	agents["entries"] = map[string]interface{}{"desk": map[string]interface{}{"name": "Desk"}, "community": map[string]interface{}{"name": "Community"}}
	agents["defaults"].(map[string]interface{})["heartbeat"] = map[string]interface{}{"agentId": "desk"}
	marker, got, _ := runRuntimeDefaults(t, cfg, wantedDefaults("10.129.2.2", "127.0.0.1", "::1"))
	if !strings.HasPrefix(marker, "RECONCILED_RUNTIME") {
		t.Fatalf("marker = %q", marker)
	}
	hb, _ := dig(t, got, "agents", "defaults", "heartbeat").(map[string]interface{})
	if hb["every"] != "0m" || hb["agentId"] != "desk" {
		t.Fatalf("heartbeat = %v (the owner the runtime recorded must stay)", hb)
	}
	if _, has := got["agents"].(map[string]interface{})["list"]; has {
		t.Fatal("a legacy agents.list key appeared; 2026.8.1+ refuses a file that has one")
	}
}

func TestRuntimeDefaults_RuntimeRefusesTheCandidate(t *testing.T) {
	before := seededGatewayConfig()
	d := wantedDefaults("10.129.2.2", "127.0.0.1", "::1")
	d.ValidateCmd = validatorRefuses
	marker, got, dir := runRuntimeDefaults(t, before, d)
	if !strings.HasPrefix(marker, "INVALID_CANDIDATE") || !strings.Contains(marker, "unknown key dreaming") {
		t.Fatalf("marker = %q", marker)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatal("the live config was changed although the runtime refused the candidate")
	}
	if _, err := os.Stat(filepath.Join(dir, "openclaw.json.operator-candidate")); !os.IsNotExist(err) {
		t.Fatalf("refused candidate left behind (stat err=%v)", err)
	}
}

func TestRuntimeDefaults_AnotherWriterWins(t *testing.T) {
	d := wantedDefaults("10.129.2.2", "127.0.0.1", "::1")
	d.ValidateCmd = validatorWhileSomeoneElseWrites
	marker, got, dir := runRuntimeDefaults(t, seededGatewayConfig(), d)
	if marker != "STALE" {
		t.Fatalf("marker = %q", marker)
	}
	if got["writtenBySomeoneElse"] != true {
		t.Fatal("the other writer's change was overwritten")
	}
	if l, _ := dig(t, got, "gateway", "trustedProxies").([]interface{}); len(l) != 4 {
		t.Fatalf("our stale candidate was applied anyway: %v", l)
	}
	if _, err := os.Stat(filepath.Join(dir, "openclaw.json.operator-candidate")); !os.IsNotExist(err) {
		t.Fatalf("stale candidate left behind (stat err=%v)", err)
	}
}

// Point this at a cluster to see what the operator would trust there:
//
//	oc get pods -n openshift-ingress -l ingresscontroller.operator.openshift.io/deployment-ingresscontroller -o json > routers.json
//	oc get nodes -o json > nodes.json
//	INGRESS_ROUTERS_JSON=routers.json INGRESS_NODES_JSON=nodes.json go test ./internal/controller/ -run TestIngressSourcesFrom_ClusterDump -v
func TestIngressSourcesFrom_ClusterDump(t *testing.T) {
	routersPath, nodesPath := os.Getenv("INGRESS_ROUTERS_JSON"), os.Getenv("INGRESS_NODES_JSON")
	if routersPath == "" {
		t.Skip("INGRESS_ROUTERS_JSON not set")
	}
	var routers corev1.PodList
	raw, err := os.ReadFile(routersPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &routers); err != nil {
		t.Fatal(err)
	}
	var nodes corev1.NodeList
	if nodesPath != "" {
		raw, err := os.ReadFile(nodesPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &nodes); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ingressSourcesFrom(routers.Items, nodes.Items)
	if err != nil {
		t.Fatalf("not determined: %v", err)
	}
	list := append(append([]string{}, got.Addresses...), loopbackProxies...)
	b, _ := json.Marshal(list)
	t.Logf("routers=%d hostNetwork=%v nodes=%d", len(routers.Items), routersUseHostNetwork(routers.Items), len(nodes.Items))
	t.Logf("basis: %s", got.Basis)
	t.Logf("TRUSTED_PROXIES=%s", b)
}
