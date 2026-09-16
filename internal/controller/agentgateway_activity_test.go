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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

// activityFindPath returns a PATH under which `find` understands the one
// invocation the activity script makes (`find DIR -path
// '*/sessions/*.jsonl' -printf '%T@\n'`): the host's own when it is GNU
// find, otherwise a perl stand-in, so the loop and its exit status are
// tested on macOS too.
func activityFindPath(t *testing.T) string {
	t.Helper()
	if out, err := exec.Command("find", ".", "-maxdepth", "0", "-printf", "%T@").CombinedOutput(); err == nil && len(out) > 0 {
		return os.Getenv("PATH")
	}
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("neither GNU find nor perl on this host; the gateway image has GNU find")
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\n" +
		`exec perl -MFile::Find -e 'find(sub { print((stat($_))[9], "\n") if -f $_ && $File::Find::name =~ m{/sessions/.*\.jsonl$} }, $ARGV[0])' "$1"` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "find"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin + string(os.PathListSeparator) + os.Getenv("PATH")
}

// The shipped activity script, run against a fixture agents dir. The
// sqlite side files are touched AFTER the transcript — exactly what a
// config reload does — and must not count. The last agent listed has no
// transcript, like "main" on a real gateway, and the script must still
// exit 0 (v1.7.77 exited 1 there and the pass was dropped).
func TestAgentActivityScriptCountsTranscriptsOnly(t *testing.T) {
	path := activityFindPath(t)
	root := t.TempDir()
	touch := func(rel string, at time.Time) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	turn := time.Date(2026, 9, 10, 3, 0, 53, 0, time.UTC)
	reload := time.Date(2026, 9, 16, 15, 50, 31, 0, time.UTC)
	touch("busy/sessions/old.jsonl", turn.Add(-time.Hour))
	touch("busy/sessions/new.jsonl", turn)
	touch("busy/sessions/new.trajectory.jsonl", turn.Add(-time.Minute))
	touch("busy/sessions/new.jsonl.lock", reload)
	touch("busy/sessions/sessions.json", reload)
	touch("busy/agent/openclaw-agent.sqlite", reload)
	touch("busy/agent/openclaw-agent.sqlite-shm", reload)
	touch("busy/agent/openclaw-agent.sqlite-wal", reload)
	touch("idle/agent/openclaw-agent.sqlite-shm", reload)
	touch("idle/agent/models.json", reload)
	touch("main/agent/openclaw-agent.sqlite-wal", reload) // sorts last, no transcript

	script := strings.Replace(agentActivityScript, "/home/node/.openclaw/agents/*/", root+"/*/", 1)
	if script == agentActivityScript {
		t.Fatal("could not redirect the agents dir; the script's loop line changed")
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("activity script must exit 0 even when the last agent has no transcript: %v\n%s", err, out)
	}
	seen := parseAgentActivity(string(out))
	if len(seen) != 1 || !seen["busy"].Equal(turn) {
		t.Fatalf("want only busy@%s, got %v (raw %q)", turn, seen, out)
	}
}

func TestParseAgentActivity(t *testing.T) {
	seen := parseAgentActivity("a 1757991653\nbad line here\nb notanumber\nc 0\n\n")
	if len(seen) != 1 || seen["a"].Unix() != 1757991653 {
		t.Fatalf("got %v", seen)
	}
}

func activityAW(last *time.Time, marked bool) *agentofficev1alpha1.AgentWorkstation {
	aw := &agentofficev1alpha1.AgentWorkstation{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", Generation: 3, ResourceVersion: "42"}}
	if last != nil {
		t := metav1.NewTime(*last)
		aw.Status.LastActivity = &t
	}
	aw.Status.Conditions = []metav1.Condition{{Type: "SchedulesConverged", Status: metav1.ConditionTrue, Reason: "Converged"}}
	if marked {
		aw.Status.Conditions = append(aw.Status.Conditions, metav1.Condition{
			Type: activitySignalCondition, Status: metav1.ConditionTrue, Reason: activitySignalReason})
	}
	return aw
}

func patchBody(t *testing.T, patched *agentofficev1alpha1.AgentWorkstation, p client.Patch) map[string]interface{} {
	t.Helper()
	data, err := p.Data(patched)
	if err != nil {
		t.Fatalf("patch data: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("patch json: %v", err)
	}
	return m
}

func TestPlanActivityPatchFirstPassReplacesOldSignal(t *testing.T) {
	fake := time.Date(2026, 9, 16, 15, 50, 31, 0, time.UTC) // a reload, not a turn
	turn := time.Date(2026, 9, 10, 3, 0, 53, 0, time.UTC)

	aw := activityAW(&fake, false)
	patched, p, changed := planActivityPatch(aw, turn, true)
	if !changed || !patched.Status.LastActivity.Time.Equal(turn) {
		t.Fatalf("first pass must move lastActivity back to the real turn, got %v %v", changed, patched)
	}
	if c := meta.FindStatusCondition(patched.Status.Conditions, activitySignalCondition); c == nil || c.Reason != activitySignalReason || c.ObservedGeneration != 3 {
		t.Fatalf("marker condition missing: %+v", patched.Status.Conditions)
	}
	if meta.FindStatusCondition(patched.Status.Conditions, "SchedulesConverged") == nil {
		t.Fatal("other conditions must survive")
	}
	body := patchBody(t, patched, p)
	if md, _ := body["metadata"].(map[string]interface{}); md == nil || md["resourceVersion"] != "42" {
		t.Fatalf("the conditions-rewriting patch must carry resourceVersion, got %v", body)
	}

	// No transcript at all: the polluted value is cleared.
	patched, p, changed = planActivityPatch(activityAW(&fake, false), time.Time{}, false)
	if !changed || patched.Status.LastActivity != nil {
		t.Fatalf("no transcript must clear lastActivity, got %v %v", changed, patched.Status.LastActivity)
	}
	st := patchBody(t, patched, p)["status"].(map[string]interface{})
	if v, ok := st["lastActivity"]; !ok || v != nil {
		t.Fatalf("clearing must send lastActivity: null, got %v", st)
	}
}

func TestPlanActivityPatchMarkedOnlyMovesForward(t *testing.T) {
	last := time.Date(2026, 9, 16, 15, 0, 0, 0, time.UTC)
	aw := activityAW(&last, true)
	if _, _, changed := planActivityPatch(aw, last.Add(-time.Hour), true); changed {
		t.Fatal("an older transcript must not move lastActivity backwards")
	}
	if _, _, changed := planActivityPatch(aw, last, true); changed {
		t.Fatal("an unchanged transcript must not patch")
	}
	if _, _, changed := planActivityPatch(aw, time.Time{}, false); changed {
		t.Fatal("a vanished transcript must not clear a recorded value")
	}
	newer := last.Add(time.Minute)
	patched, p, changed := planActivityPatch(aw, newer, true)
	if !changed || !patched.Status.LastActivity.Time.Equal(newer) {
		t.Fatalf("a newer transcript must move forward, got %v", patched)
	}
	body := patchBody(t, patched, p)
	if _, hasMeta := body["metadata"]; hasMeta {
		t.Fatalf("the forward patch must carry only lastActivity, got %v", body)
	}
	if st := body["status"].(map[string]interface{}); len(st) != 1 || st["lastActivity"] == nil {
		t.Fatalf("the forward patch must carry only lastActivity, got %v", body)
	}
	// A marker from someone else's reason is not ours.
	aw.Status.Conditions[1].Reason = "Other"
	if _, _, changed := planActivityPatch(aw, last.Add(-time.Hour), true); !changed {
		t.Fatal("a foreign marker must be replaced")
	}
}

func TestIgnoreStatusOnlyUpdates(t *testing.T) {
	pred := ignoreStatusOnlyUpdates()
	base := func() *agentofficev1alpha1.AgentWorkstation {
		aw := activityAW(nil, false)
		aw.Labels = map[string]string{"team": "news"}
		aw.Annotations = map[string]string{}
		aw.Finalizers = []string{awSharedGatewayFinalizer}
		return aw
	}
	now := metav1.Now()
	cases := map[string]struct {
		mutate func(aw *agentofficev1alpha1.AgentWorkstation)
		want   bool
	}{
		"status only (lastActivity)": {func(aw *agentofficev1alpha1.AgentWorkstation) {
			aw.Status.LastActivity = &now
			aw.ResourceVersion = "43"
			aw.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "operator", Subresource: "status"}}
		}, false},
		"nil vs empty annotations": {func(aw *agentofficev1alpha1.AgentWorkstation) { aw.Annotations = nil }, false},
		"spec (generation)":        {func(aw *agentofficev1alpha1.AgentWorkstation) { aw.Generation++ }, true},
		"annotation":               {func(aw *agentofficev1alpha1.AgentWorkstation) { aw.Annotations["kick"] = "1" }, true},
		"label":                    {func(aw *agentofficev1alpha1.AgentWorkstation) { aw.Labels["team"] = "ops" }, true},
		"finalizer":                {func(aw *agentofficev1alpha1.AgentWorkstation) { aw.Finalizers = nil }, true},
		"deletion":                 {func(aw *agentofficev1alpha1.AgentWorkstation) { aw.DeletionTimestamp = &now }, true},
		"owner": {func(aw *agentofficev1alpha1.AgentWorkstation) {
			aw.OwnerReferences = []metav1.OwnerReference{{Name: "x", Kind: "Team", APIVersion: "v1", UID: "u"}}
		}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			old, cur := base(), base()
			tc.mutate(cur)
			if got := pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: cur}); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if !pred.Create(event.CreateEvent{Object: base()}) || !pred.Delete(event.DeleteEvent{Object: base()}) {
		t.Fatal("create and delete must pass")
	}
	// Works for any object kind (the AG watch sees AWs, other callers may not).
	if pred.Update(event.UpdateEvent{ObjectOld: &corev1.Pod{}, ObjectNew: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}}) {
		t.Fatal("status-only pod update must be dropped")
	}
}
