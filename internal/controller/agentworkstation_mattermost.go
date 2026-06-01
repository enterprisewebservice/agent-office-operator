/*
Mattermost auto-provisioning for AgentWorkstations.

Every agent gets a Mattermost presence — a USER account @<name> (its own name,
green presence dot — Mattermost doesn't render dots for *bot* accounts) plus a
#<name> channel — created on reconcile and torn down by the finalizer on
delete. The in-cluster mm-bridge then discovers these users and does the chat
(receive → drive the openclaw agent → reply as the user), plus presence + the
"…is typing" indicator.

Opt-in + graceful: enabled only when a `mattermost-admin-token` Secret exists
in the AgentWorkstation's namespace (key `token`). Absent ⇒ no-op. The
Mattermost base URL is MM_URL (default: the in-cluster service).
*/
package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

const (
	mmAdminTokenSecret = "mattermost-admin-token"
	mmTeam             = "agents"
)

var mmHTTP = &http.Client{
	Timeout:   15 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

func mmURL() string {
	if u := os.Getenv("MM_URL"); u != "" {
		return u
	}
	return "http://mattermost.mattermost.svc.cluster.local:8065"
}

func randString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// mmAdminToken returns the Mattermost admin PAT from a Secret in ns, or ""
// (⇒ Mattermost integration disabled — skip silently).
func (r *AgentWorkstationReconciler) mmAdminToken(ctx context.Context, ns string) string {
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: mmAdminTokenSecret}, &s); err != nil {
		return ""
	}
	return string(s.Data["token"])
}

// mmAPI calls the Mattermost REST API with the admin token. Returns the HTTP
// status and the decoded object (best-effort; nil for arrays/empty).
func mmAPI(method, base, token, path string, body interface{}) (int, map[string]interface{}) {
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, buf)
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := mmHTTP.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]interface{}
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func mmStr(m map[string]interface{}, k string) string {
	s, _ := m[k].(string)
	return s
}

// reconcileMattermost ensures the agent's Mattermost user + channel exist.
// Best-effort: errors are returned for logging but never block the core AW
// reconcile (the caller treats them as non-fatal).
func (r *AgentWorkstationReconciler) reconcileMattermost(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) error {
	token := r.mmAdminToken(ctx, aw.Namespace)
	if token == "" {
		return nil // integration not configured
	}
	base := mmURL()
	agent := aw.Name
	display := aw.Spec.DisplayName
	if display == "" {
		display = agent
	}

	// team
	st, team := mmAPI("GET", base, token, "/api/v4/teams/name/"+mmTeam, nil)
	if st != 200 {
		_, team = mmAPI("POST", base, token, "/api/v4/teams",
			map[string]interface{}{"name": mmTeam, "display_name": "Agents", "type": "O"})
	}
	teamID := mmStr(team, "id")
	if teamID == "" {
		return fmt.Errorf("mattermost: no team")
	}

	// user (find-or-create). A legacy auto-provisioned BOT owning the name is
	// renamed away (bots get no presence dot; agents must be real users).
	st, u := mmAPI("GET", base, token, "/api/v4/users/username/"+agent, nil)
	if st == 200 {
		if isBot, _ := u["is_bot"].(bool); isBot {
			id := mmStr(u, "id")
			mmAPI("PUT", base, token, "/api/v4/bots/"+id, map[string]interface{}{"username": "zz-" + agent + "-bot"})
			mmAPI("POST", base, token, "/api/v4/bots/"+id+"/disable", nil)
			st = 404
		}
	}
	var userID string
	if st != 200 {
		cst, cu := mmAPI("POST", base, token, "/api/v4/users", map[string]interface{}{
			"email": agent + "@agents.local", "username": agent, "password": "Aa1!" + randString(12)})
		if cst != 201 {
			return fmt.Errorf("mattermost: create user %s: %v", agent, cu["message"])
		}
		userID = mmStr(cu, "id")
	} else {
		userID = mmStr(u, "id")
		mmAPI("PUT", base, token, "/api/v4/users/"+userID+"/active", map[string]interface{}{"active": true})
	}
	mmAPI("PUT", base, token, "/api/v4/users/"+userID+"/patch", map[string]interface{}{"nickname": display})
	mmAPI("POST", base, token, "/api/v4/teams/"+teamID+"/members",
		map[string]interface{}{"team_id": teamID, "user_id": userID})

	// channel
	st, ch := mmAPI("GET", base, token, "/api/v4/teams/"+teamID+"/channels/name/"+agent, nil)
	if st != 200 {
		_, ch = mmAPI("POST", base, token, "/api/v4/channels",
			map[string]interface{}{"team_id": teamID, "name": agent, "display_name": display, "type": "O"})
	}
	if chID := mmStr(ch, "id"); chID != "" {
		mmAPI("POST", base, token, "/api/v4/channels/"+chID+"/members", map[string]interface{}{"user_id": userID})
	}
	logf.FromContext(ctx).Info("mattermost presence ensured", "agent", agent)
	return nil
}

// cleanupMattermost archives the channel + deactivates the agent user.
func (r *AgentWorkstationReconciler) cleanupMattermost(ctx context.Context, aw *agentofficev1alpha1.AgentWorkstation) error {
	token := r.mmAdminToken(ctx, aw.Namespace)
	if token == "" {
		return nil
	}
	base := mmURL()
	agent := aw.Name
	if st, team := mmAPI("GET", base, token, "/api/v4/teams/name/"+mmTeam, nil); st == 200 {
		teamID := mmStr(team, "id")
		if cst, ch := mmAPI("GET", base, token, "/api/v4/teams/"+teamID+"/channels/name/"+agent, nil); cst == 200 {
			mmAPI("DELETE", base, token, "/api/v4/channels/"+mmStr(ch, "id"), nil)
		}
	}
	if st, u := mmAPI("GET", base, token, "/api/v4/users/username/"+agent, nil); st == 200 {
		if isBot, _ := u["is_bot"].(bool); !isBot {
			mmAPI("DELETE", base, token, "/api/v4/users/"+mmStr(u, "id"), nil)
		}
	}
	logf.FromContext(ctx).Info("mattermost presence cleaned up", "agent", agent)
	return nil
}
