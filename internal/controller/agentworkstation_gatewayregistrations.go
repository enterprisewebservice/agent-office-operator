package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// mcpRegistrationGVK is the Kuadrant MCP gateway's registration of a backend
// MCP server. A seat's own servers (Module 6) live in the seat namespace.
var mcpRegistrationGVK = schema.GroupVersionKind{Group: "mcp.kuadrant.io", Version: "v1alpha1", Kind: "MCPServerRegistration"}

// registrationEntry is what the runtime cares about: which servers exist,
// whether the gateway could reach them, and how many tools it found.
type registrationEntry struct {
	Name      string
	Ready     bool
	ToolCount int64
}

// registrationsSignature turns the entries into a stable fingerprint plus
// the sorted names, so status stays readable and equality is exact.
func registrationsSignature(entries []registrationEntry) (string, []string) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	lines := make([]string, 0, len(entries))
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, fmt.Sprintf("%s|%t|%d", e.Name, e.Ready, e.ToolCount))
		names = append(names, e.Name)
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:8]), names
}

// registrationDiff phrases what changed between two name lists.
func registrationDiff(prev, cur []string) string {
	in := func(x string, xs []string) bool {
		for _, y := range xs {
			if x == y {
				return true
			}
		}
		return false
	}
	var added, removed []string
	for _, n := range cur {
		if !in(n, prev) {
			added = append(added, n)
		}
	}
	for _, n := range prev {
		if !in(n, cur) {
			removed = append(removed, n)
		}
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	if len(parts) == 0 {
		return "a registration changed"
	}
	return strings.Join(parts, "; ")
}

// listGatewayRegistrations reads the namespace's MCPServerRegistrations.
func (r *AgentWorkstationReconciler) listGatewayRegistrations(ctx context.Context, ns string) ([]registrationEntry, error) {
	var ul unstructured.UnstructuredList
	ul.SetGroupVersionKind(schema.GroupVersionKind{Group: mcpRegistrationGVK.Group, Version: mcpRegistrationGVK.Version, Kind: mcpRegistrationGVK.Kind + "List"})
	if err := r.List(ctx, &ul, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	out := make([]registrationEntry, 0, len(ul.Items))
	for _, it := range ul.Items {
		e := registrationEntry{Name: it.GetName()}
		conds, _, _ := unstructured.NestedSlice(it.Object, "status", "conditions")
		for _, c := range conds {
			m, ok := c.(map[string]interface{})
			if ok && m["type"] == "Ready" && m["status"] == "True" {
				e.Ready = true
			}
		}
		if tc, found, _ := unstructured.NestedInt64(it.Object, "status", "toolCount"); found {
			e.ToolCount = tc
		}
		out = append(out, e)
	}
	return out, nil
}

// reconcileGatewayRegistrations keeps the agent's governed tool list honest.
// The runtime lists a gateway's tools when its MCP runtime connects, so a
// server registered afterwards (the service an attendee ships in Module 6)
// is invisible until the runtime reconnects. When the namespace's
// registration set changes, dispose the gateway's cached MCP runtimes
// (`openclaw mcp reload`: the next turn rebuilds the connection and
// re-lists) and tell the agent's channel what changed. The first
// observation only records the set.
func (r *AgentWorkstationReconciler) reconcileGatewayRegistrations(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation, gwPod *corev1.Pod) error {
	log := logf.FromContext(ctx)
	entries, err := r.listGatewayRegistrations(ctx, aw.Namespace)
	if err != nil {
		return fmt.Errorf("list MCPServerRegistrations in %s: %w", aw.Namespace, err)
	}
	sig, names := registrationsSignature(entries)
	if aw.Status.GatewayRegistrationsHash == sig {
		return nil
	}
	if aw.Status.GatewayRegistrationsHash == "" {
		aw.Status.GatewayRegistrationsHash = sig
		aw.Status.GatewayRegistrations = names
		return nil
	}
	if gwPod == nil {
		return fmt.Errorf("no ready gateway pod to reload for %s", aw.Name)
	}
	if out, err := r.execInPod(ctx, gwPod, []string{"openclaw", "mcp", "reload"}); err != nil {
		return fmt.Errorf("openclaw mcp reload in %s: %w (out=%s)", gwPod.Name, err, strings.TrimSpace(out))
	}
	what := registrationDiff(aw.Status.GatewayRegistrations, names)
	for _, e := range entries {
		if strings.Contains(what, "added "+e.Name) && e.ToolCount > 0 {
			what = strings.Replace(what, "added "+e.Name, fmt.Sprintf("added %s (%d tools)", e.Name, e.ToolCount), 1)
		}
	}
	log.Info("gateway registrations changed; MCP runtimes reloaded", "aw", aw.Name, "change", what, "pod", gwPod.Name)
	aw.Status.GatewayRegistrationsHash = sig
	aw.Status.GatewayRegistrations = names
	r.mmNotify(ctx, aw, "🔧 Governed tools changed — "+what+". Reconnecting "+aw.Name+" to the gateway; the new tools are in its list from its next message.")
	return nil
}

// skillSetSignature is the delivered skill set as sorted name@version.
func skillSetSignature(rs []ResolvedSkill) []string {
	out := make([]string, 0, len(rs))
	for _, s := range rs {
		v := s.Version
		if v == "" {
			v = s.Skill.Spec.Version
		}
		if v != "" {
			out = append(out, s.Skill.Name+"@"+v)
		} else {
			out = append(out, s.Skill.Name)
		}
	}
	sort.Strings(out)
	return out
}

// noteSkillSetDelivery bumps the session epoch when a declared-skills agent
// receives a different set than last time, and says so in the channel.
func (r *AgentWorkstationReconciler) noteSkillSetDelivery(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation, rs []ResolvedSkill) {
	cur := skillSetSignature(rs)
	prev := aw.Status.SkillSet
	if len(prev) > 0 && strings.Join(prev, ",") != strings.Join(cur, ",") {
		aw.Status.SessionEpoch++
		what := registrationDiff(prev, cur)
		logf.FromContext(ctx).Info("skill set changed; session epoch bumped", "aw", aw.Name, "epoch", aw.Status.SessionEpoch, "change", what)
		r.mmNotify(ctx, aw, "🔄 Skills changed — "+what+". Fresh session so "+aw.Name+" can use its current skills from the first word.")
	}
	aw.Status.SkillSet = cur
}

// mmNotify posts a platform note into the agent's channel (best effort).
func (r *AgentWorkstationReconciler) mmNotify(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation, text string) {
	token := r.mmAdminToken(ctx, aw.Namespace)
	if token == "" {
		return
	}
	base := mmURL()
	st, team := mmAPI("GET", base, token, "/api/v4/teams/name/"+mmTeam, nil)
	teamID := mmStr(team, "id")
	if st != 200 || teamID == "" {
		return
	}
	st, ch := mmAPI("GET", base, token, "/api/v4/teams/"+teamID+"/channels/name/"+mmSlug(aw.Name), nil)
	chID := mmStr(ch, "id")
	if st != 200 || chID == "" {
		return
	}
	// Marked as a platform notice: the chat bridge does not relay posts carrying
	// agentoffice.ai/notice to the agent (they are for the attendee), and
	// from_bot renders Mattermost's BOT tag so nobody mistakes it for a person.
	mmAPI("POST", base, token, "/api/v4/posts", map[string]interface{}{
		"channel_id": chID,
		"message":    text,
		"props":      map[string]interface{}{"agentoffice.ai/notice": "platform", "from_bot": "true"},
	})
}
