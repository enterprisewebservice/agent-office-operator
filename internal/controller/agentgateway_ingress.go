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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// The operator reads Nodes (never writes them) to learn the address the
// cluster's ingress routers reach pods from; see discoverIngressSources.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list

// Why gateway.trustedProxies is converged at all (v1.7.87)
//
// OpenClaw decides per request whether it is talking to a client or to a
// reverse proxy, from the SOURCE ADDRESS alone:
//
//   - source in gateway.trustedProxies: it is a proxy, and it MUST carry
//     forwarded-client headers (X-Forwarded-For). Without them the request
//     is refused before authentication, 403 proxy_attribution_required.
//   - source not in the list: it is a client, and it must NOT carry such
//     headers (same refusal).
//
// Up to 2026.7.x the first rule was lenient. The gateway template seeded
// the whole pod network as "trusted proxies" so the OpenShift router would
// be believed, and every in-cluster caller that talks to the gateway
// directly (a website's backend, the agent-office backend, a node host)
// got away with being called a proxy. From the release that enforces the
// rule those callers are refused: runtab-help answered 403 to the Runtab
// backend for three minutes after its upgrade to 2026.10.33 (20 Nov 2026).
//
// The list has to name the routers and nothing else. Which addresses those
// are is a property of the cluster, so it is discovered, not written in a
// manifest:
//
//   - a router pod on the pod network is seen from its pod IP;
//   - a router on the HOST network (bare metal, endpointPublishingStrategy
//     HostNetwork) is seen from its node's OVN-Kubernetes management port,
//     the second address of that node's pod subnet (10.130.2.0/23 ->
//     10.130.2.2). A router can be rescheduled to any node, so every
//     node's address is listed. No pod is ever given that address.
//
// Loopback is always listed, as the template always did.
const (
	ingressRouterNamespace = "openshift-ingress"
	// Present on every router pod the ingress operator manages; the value
	// is the IngressController's name.
	ingressRouterLabel = "ingresscontroller.operator.openshift.io/deployment-ingresscontroller"
	// {"default":["10.130.2.0/23"]} (a bare string on older releases).
	ovnNodeSubnetsAnnotation = "k8s.ovn.org/node-subnets"
	// trustedProxiesEnv, when set on the operator, replaces discovery:
	// comma- or space-separated addresses and CIDRs. For a cluster whose
	// ingress the discovery does not understand.
	trustedProxiesEnv = "AGENT_GATEWAY_TRUSTED_PROXIES"
	// How long one discovery is reused across gateways and passes.
	ingressSourcesTTL = 2 * time.Minute
)

// loopbackProxies are always trusted: the CLI and anything else inside the
// gateway pod.
var loopbackProxies = []string{"127.0.0.1", "::1"}

// ingressSources is one answer to "where do this cluster's routers reach
// pods from".
type ingressSources struct {
	// Addresses is sorted and free of duplicates; loopback is not in it.
	Addresses []string
	// Basis says where the answer came from, for the status condition.
	Basis string
}

// ingressSourcesCache keeps the last successful discovery. Failures are
// not cached: the next pass asks again.
type ingressSourcesCache struct {
	mu      sync.Mutex
	at      time.Time
	sources ingressSources
}

var sharedIngressSources ingressSourcesCache

// discoverIngressSources answers from the cache, the operator setting, or
// the cluster, in that order. An error means "not determined": the caller
// leaves gateway.trustedProxies as it finds it.
func (r *AgentGatewayReconciler) discoverIngressSources(ctx context.Context) (ingressSources, error) {
	if raw := strings.TrimSpace(os.Getenv(trustedProxiesEnv)); raw != "" {
		addrs, err := parseProxyList(raw)
		if err != nil {
			return ingressSources{}, fmt.Errorf("%s: %w", trustedProxiesEnv, err)
		}
		return ingressSources{Addresses: addrs, Basis: "the operator setting " + trustedProxiesEnv}, nil
	}

	sharedIngressSources.mu.Lock()
	defer sharedIngressSources.mu.Unlock()
	if !sharedIngressSources.at.IsZero() && time.Since(sharedIngressSources.at) < ingressSourcesTTL {
		return sharedIngressSources.sources, nil
	}
	if r.Reader == nil {
		return ingressSources{}, fmt.Errorf("no uncached reader; cannot look at the ingress routers")
	}

	var routers corev1.PodList
	if err := r.Reader.List(ctx, &routers,
		client.InNamespace(ingressRouterNamespace),
		client.HasLabels{ingressRouterLabel},
	); err != nil {
		return ingressSources{}, fmt.Errorf("list ingress router pods: %w", err)
	}
	var nodes corev1.NodeList
	if routersUseHostNetwork(routers.Items) {
		if err := r.Reader.List(ctx, &nodes); err != nil {
			return ingressSources{}, fmt.Errorf("list nodes: %w", err)
		}
	}
	found, err := ingressSourcesFrom(routers.Items, nodes.Items)
	if err != nil {
		return ingressSources{}, err
	}
	sharedIngressSources.at = time.Now()
	sharedIngressSources.sources = found
	return found, nil
}

func routersUseHostNetwork(routers []corev1.Pod) bool {
	for i := range routers {
		if routers[i].DeletionTimestamp == nil && routers[i].Spec.HostNetwork {
			return true
		}
	}
	return false
}

// ingressSourcesFrom is the pure part of the discovery.
func ingressSourcesFrom(routers []corev1.Pod, nodes []corev1.Node) (ingressSources, error) {
	set := map[string]struct{}{}
	live, onHost, onPodNet := 0, 0, 0
	for i := range routers {
		p := &routers[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		live++
		if p.Spec.HostNetwork {
			onHost++
			continue
		}
		for _, ip := range p.Status.PodIPs {
			if parsed := net.ParseIP(ip.IP); parsed != nil {
				set[parsed.String()] = struct{}{}
				onPodNet++
			}
		}
	}
	if live == 0 {
		return ingressSources{}, fmt.Errorf("no ingress router pod found in namespace %s", ingressRouterNamespace)
	}
	basis := []string{}
	if onPodNet > 0 {
		basis = append(basis, fmt.Sprintf("%d router pod address(es)", onPodNet))
	}
	if onHost > 0 {
		fromNodes := 0
		for i := range nodes {
			ips, err := ovnManagementIPs(nodes[i].Annotations[ovnNodeSubnetsAnnotation])
			if err != nil {
				// One unreadable node must not hide the others; a router
				// landing there is refused until the node is readable.
				continue
			}
			for _, ip := range ips {
				set[ip] = struct{}{}
				fromNodes++
			}
		}
		if fromNodes == 0 {
			return ingressSources{}, fmt.Errorf(
				"the ingress routers use the host network, but no node carries the annotation %s to derive their address from; set %s on the operator",
				ovnNodeSubnetsAnnotation, trustedProxiesEnv)
		}
		basis = append(basis, fmt.Sprintf("the host-network address of %d node subnet(s)", fromNodes))
	}
	if len(set) == 0 {
		return ingressSources{}, fmt.Errorf("the ingress router pods have no address yet")
	}
	addrs := make([]string, 0, len(set))
	for a := range set {
		addrs = append(addrs, a)
	}
	sortProxyAddresses(addrs)
	return ingressSources{Addresses: addrs, Basis: strings.Join(basis, " and ")}, nil
}

// ovnManagementIPs returns the management-port address of each subnet in a
// k8s.ovn.org/node-subnets annotation: network address + 2. Only the
// cluster's default network is read; a user-defined network is not how a
// router reaches a gateway.
func ovnManagementIPs(annotation string) ([]string, error) {
	if strings.TrimSpace(annotation) == "" {
		return nil, fmt.Errorf("no %s annotation", ovnNodeSubnetsAnnotation)
	}
	var byNetwork map[string]json.RawMessage
	if err := json.Unmarshal([]byte(annotation), &byNetwork); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ovnNodeSubnetsAnnotation, err)
	}
	raw, ok := byNetwork["default"]
	if !ok {
		return nil, fmt.Errorf("%s has no default network", ovnNodeSubnetsAnnotation)
	}
	var cidrs []string
	if err := json.Unmarshal(raw, &cidrs); err != nil {
		var one string
		if err2 := json.Unmarshal(raw, &one); err2 != nil {
			return nil, fmt.Errorf("parse %s default network: %w", ovnNodeSubnetsAnnotation, err)
		}
		cidrs = []string{one}
	}
	out := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		_, ipnet, err := net.ParseCIDR(strings.TrimSpace(c))
		if err != nil {
			return nil, fmt.Errorf("parse subnet %q: %w", c, err)
		}
		ip := make(net.IP, len(ipnet.IP))
		copy(ip, ipnet.IP)
		// + 2, with carry: .1 is the subnet's router port, .2 the
		// management port the host talks to pods from.
		for i, add := len(ip)-1, 2; i >= 0 && add > 0; i-- {
			sum := int(ip[i]) + add
			ip[i] = byte(sum & 0xff)
			add = sum >> 8
		}
		if !ipnet.Contains(ip) {
			return nil, fmt.Errorf("subnet %q is too small to hold a management port", c)
		}
		out = append(out, ip.String())
	}
	return out, nil
}

// parseProxyList validates the operator setting: addresses and CIDRs,
// separated by commas or white space.
func parseProxyList(raw string) ([]string, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	set := map[string]struct{}{}
	for _, f := range fields {
		if ip := net.ParseIP(f); ip != nil {
			if !ip.IsLoopback() {
				set[ip.String()] = struct{}{}
			}
			continue
		}
		if _, ipnet, err := net.ParseCIDR(f); err == nil {
			set[ipnet.String()] = struct{}{}
			continue
		}
		return nil, fmt.Errorf("%q is neither an address nor a CIDR", f)
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("no address in %q", raw)
	}
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sortProxyAddresses(out)
	return out, nil
}

// sortProxyAddresses orders IPv4 before IPv6 and each numerically, so the
// rendered list reads like an inventory and never reorders by itself.
func sortProxyAddresses(addrs []string) {
	key := func(s string) (int, []byte, string) {
		host := s
		if i := strings.IndexByte(s, '/'); i >= 0 {
			host = s[:i]
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return 2, nil, s
		}
		if v4 := ip.To4(); v4 != nil {
			return 0, v4, s
		}
		return 1, ip.To16(), s
	}
	sort.Slice(addrs, func(i, j int) bool {
		fi, bi, si := key(addrs[i])
		fj, bj, sj := key(addrs[j])
		if fi != fj {
			return fi < fj
		}
		if c := bytes.Compare(bi, bj); c != 0 {
			return c < 0
		}
		return si < sj
	})
}

// runtimeDefaults is the JSON handed to runtimeDefaultsScript.
type runtimeDefaults struct {
	// TrustedProxies is the whole desired gateway.trustedProxies, loopback
	// included. Nil: leave the list alone (the routers are not known).
	TrustedProxies []string `json:"trustedProxies,omitempty"`
	// HeartbeatEveryWhenUnset is written to agents.defaults.heartbeat.every
	// only while nobody has chosen a cadence.
	HeartbeatEveryWhenUnset string `json:"heartbeatEveryWhenUnset,omitempty"`
	// DreamingEnabledWhenUnset is written to
	// plugins.entries.memory-core.config.dreaming.enabled only while nobody
	// has chosen.
	DreamingEnabledWhenUnset *bool `json:"dreamingEnabledWhenUnset,omitempty"`
	// ValidateCmd is run with OPENCLAW_CONFIG_PATH pointing at the
	// candidate file; a non-zero exit keeps the live config untouched.
	ValidateCmd []string `json:"validateCmd,omitempty"`
}

// Scheduled work on a gateway is declared (spec.schedules on a
// workstation), not something a runtime version switches on by itself.
// OpenClaw 2026.8+ creates two jobs on its own at upgrade: a model turn
// every 30 minutes for the first agent ("heartbeat"; on 2026.7.x the
// same default never fired here because the seeded HEARTBEAT.md is
// empty) and a nightly memory consolidation ("dreaming", opt-in before,
// on by default after). The operator pins both to what the gateways did
// before, and only where nobody chose otherwise: a value set by hand, on
// either key, is left alone.
const defaultHeartbeatEvery = "0m"

// runtimeDefaultsScript converges settings that the gateway applies by
// itself when the file changes, so no pod is restarted for them:
// gateway.trustedProxies, the heartbeat cadence and the dreaming switch.
// (OpenClaw 2026.7.x restarts its gateway IN PROCESS for any gateway.*
// change; later releases hot-reload the proxy list.)
//
// It never writes a config the runtime would refuse: the candidate is
// written beside the live file, validated by the runtime's own
// `openclaw config validate`, and renamed into place only if that passes
// and the live file is still the one the candidate was computed from.
//
// Markers on stdout: NO_CHANGE | RECONCILED_RUNTIME changed=<paths> |
// INVALID_CANDIDATE <validator output> | STALE.
//
// Tested in agentgateway_ingress_test.go by running this exact constant
// under node; nothing else type-checks it.
const runtimeDefaultsScript = `
const fs = require("fs");
const crypto = require("crypto");
const { spawnSync } = require("child_process");
const CONFIG_PATH = "/home/node/.openclaw/openclaw.json";
const D = JSON.parse(process.argv[1]);

const baseText = fs.readFileSync(CONFIG_PATH, "utf8");
const cfg = JSON.parse(baseText);
const isObj = (v) => v && typeof v === "object" && !Array.isArray(v);
const changed = [];

// 1. gateway.trustedProxies: the routers and loopback, nothing else.
//    Compared as a set, so a hand-ordered identical list is not rewritten.
if (Array.isArray(D.trustedProxies) && D.trustedProxies.length) {
  if (!isObj(cfg.gateway)) cfg.gateway = {};
  const cur = Array.isArray(cfg.gateway.trustedProxies) ? cfg.gateway.trustedProxies.map(String) : [];
  const norm = (a) => [...new Set(a)].sort().join("\n");
  if (norm(cur) !== norm(D.trustedProxies)) {
    cfg.gateway.trustedProxies = D.trustedProxies;
    changed.push("gateway.trustedProxies");
  }
}

// 2. Heartbeat cadence, only while unset and only on a gateway that has
//    agents (a config without a roster is left as the runtime seeded it).
if (D.heartbeatEveryWhenUnset && isObj(cfg.agents)) {
  const defaults = isObj(cfg.agents.defaults) ? cfg.agents.defaults : {};
  const hb = isObj(defaults.heartbeat) ? defaults.heartbeat : {};
  if (hb.every === undefined) {
    hb.every = D.heartbeatEveryWhenUnset;
    defaults.heartbeat = hb;
    cfg.agents.defaults = defaults;
    changed.push("agents.defaults.heartbeat.every");
  }
}

// 3. Dreaming, only while unset and only while the memory plugin is on.
if (typeof D.dreamingEnabledWhenUnset === "boolean" && !(isObj(cfg.plugins) && cfg.plugins.enabled === false)) {
  const plugins = isObj(cfg.plugins) ? cfg.plugins : {};
  const entries = isObj(plugins.entries) ? plugins.entries : {};
  const mc = isObj(entries["memory-core"]) ? entries["memory-core"] : {};
  if (mc.enabled !== false) {
    const conf = isObj(mc.config) ? mc.config : {};
    const dreaming = isObj(conf.dreaming) ? conf.dreaming : {};
    if (dreaming.enabled === undefined) {
      dreaming.enabled = D.dreamingEnabledWhenUnset;
      conf.dreaming = dreaming;
      mc.config = conf;
      entries["memory-core"] = mc;
      plugins.entries = entries;
      cfg.plugins = plugins;
      changed.push("plugins.entries.memory-core.config.dreaming.enabled");
    }
  }
}

if (!changed.length) {
  console.log("NO_CHANGE");
  process.exit(0);
}

// The candidate keeps the live file's permissions, so the rename changes
// the content and nothing else about the file.
const candidate = CONFIG_PATH + ".operator-candidate";
const mode = fs.statSync(CONFIG_PATH).mode & 0o777;
fs.writeFileSync(candidate, JSON.stringify(cfg, null, 2), { mode });
fs.chmodSync(candidate, mode);
const drop = () => { try { fs.unlinkSync(candidate); } catch (e) { /* already gone */ } };

if (Array.isArray(D.validateCmd) && D.validateCmd.length) {
  const v = spawnSync(D.validateCmd[0], D.validateCmd.slice(1), {
    env: { ...process.env, OPENCLAW_CONFIG_PATH: candidate },
    encoding: "utf8",
    timeout: 90000,
  });
  if (v.error || v.status !== 0) {
    drop();
    const why = v.error ? String(v.error.message || v.error) : ((v.stdout || "") + (v.stderr || ""));
    console.log("INVALID_CANDIDATE " + why.replace(/\x1b\[[0-9;]*[A-Za-z]/g, "").replace(/\s+/g, " ").trim().slice(0, 400));
    process.exit(0);
  }
}

// Someone else may have written the live file while the runtime validated
// ours: the agent merge, the gateway itself. Their write wins; the next
// pass computes a fresh candidate from it.
const sha = (s) => crypto.createHash("sha256").update(s).digest("hex");
if (sha(fs.readFileSync(CONFIG_PATH, "utf8")) !== sha(baseText)) {
  drop();
  console.log("STALE");
  process.exit(0);
}
fs.renameSync(candidate, CONFIG_PATH);
console.log("RECONCILED_RUNTIME changed=" + changed.join(","));
`

// runtimeValidateCmd is the runtime's own verdict on a config file.
var runtimeValidateCmd = []string{"openclaw", "config", "validate"}

// reconcileRuntimeDefaults converges the settings above on a running
// gateway and records the outcome in the IngressProxiesTrusted condition
// (persisted with the status at the end of the reconcile). It never
// restarts a pod and never fails the reconcile: every way it can go wrong
// leaves the gateway as it was and says why.
func (r *AgentGatewayReconciler) reconcileRuntimeDefaults(ctx context.Context, gw *agentofficev1alpha1.AgentGateway) {
	log := logf.FromContext(ctx)
	setCond := func(status metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               agentofficev1alpha1.AgentGatewayConditionIngressProxiesTrusted,
			Status:             status,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: gw.Generation,
		})
	}
	if r.RestConfig == nil {
		return
	}
	pod, err := r.findReadyGatewayPod(ctx, gw)
	if err != nil {
		// Stopped, starting or suspended: nothing to converge, and the
		// last recorded condition still describes the file on the volume.
		return
	}

	off := false
	desired := runtimeDefaults{
		HeartbeatEveryWhenUnset:  defaultHeartbeatEvery,
		DreamingEnabledWhenUnset: &off,
		ValidateCmd:              runtimeValidateCmd,
	}
	sources, discoverErr := r.discoverIngressSources(ctx)
	if discoverErr == nil {
		desired.TrustedProxies = append(append([]string{}, sources.Addresses...), loopbackProxies...)
	}

	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		log.Error(err, "marshal runtime defaults")
		return
	}
	out, err := r.execInGatewayPod(ctx, pod, []string{"node", "-e", runtimeDefaultsScript, string(desiredJSON)})
	result := strings.TrimSpace(out)
	if err != nil {
		log.Info("runtime defaults reconcile skipped", "err", err.Error(), "out", truncate(result, 300))
		return
	}

	switch {
	case strings.HasPrefix(result, "INVALID_CANDIDATE"):
		// The runtime refused what the operator wanted to write. The live
		// config is untouched; say so where a person will look.
		log.Info("runtime defaults refused by the runtime; gateway config left as it was", "validator", truncate(result, 400))
		setCond(metav1.ConditionFalse, "RefusedByRuntime",
			"the gateway's own config check refused the operator's change, so nothing was written: "+truncate(strings.TrimPrefix(result, "INVALID_CANDIDATE "), 300))
		return
	case result == "STALE":
		log.V(1).Info("runtime defaults: config changed underneath; retrying next pass")
		return
	case strings.HasPrefix(result, "RECONCILED_RUNTIME"):
		// No pod restart: the gateway watches its config file.
		log.Info("runtime defaults written to gateway config", "result", result)
	case result == "NO_CHANGE":
		log.V(1).Info("runtime defaults reconcile", "result", result)
	default:
		log.Info("runtime defaults reconcile: unexpected output", "out", truncate(result, 300))
		return
	}

	if discoverErr != nil {
		setCond(metav1.ConditionFalse, "RoutersNotDetermined",
			"gateway.trustedProxies was left as it is: "+truncate(discoverErr.Error(), 300))
		return
	}
	setCond(metav1.ConditionTrue, "Converged",
		fmt.Sprintf("gateway.trustedProxies lists %d ingress router address(es), from %s, and loopback; in-cluster callers are treated as clients",
			len(sources.Addresses), sources.Basis))
}
