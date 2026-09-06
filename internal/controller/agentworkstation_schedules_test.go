package controller

import (
	"testing"

	agentofficev1alpha1 "github.com/enterprisewebservice/agent-office-operator/api/v1alpha1"
)

const cronListSample = `{"jobs":[{"id":"j1","name":"ub-wire-hourly","enabled":true,"agentId":"upstreambeat-wire","schedule":{"kind":"cron","expr":"5 * * * *"},"payload":{"kind":"agentTurn","message":"sweep"},"delivery":{"mode":"announce"}},
{"id":"j2","name":"ub-old","agentId":"upstreambeat-wire","schedule":{"kind":"cron","expr":"0 0 * * *"},"payload":{"kind":"agentTurn","message":"old"}},
{"id":"j3","name":"other-agent-job","agentId":"someone-else","schedule":{"kind":"cron","expr":"5 * * * *"},"payload":{"kind":"agentTurn","message":"sweep"}}],"total":3}`

func TestParseCronListTolerantOfBanner(t *testing.T) {
	jobs, err := parseCronList("OpenClaw 2026.7.1\n" + cronListSample)
	if err != nil || len(jobs) != 3 || jobs[0].Schedule.Expr != "5 * * * *" || jobs[0].Payload.Message != "sweep" {
		t.Fatalf("parse: %v %+v", err, jobs)
	}
}

func TestPlanSchedulesAdoptsEditsAddsAndRemoves(t *testing.T) {
	jobs, _ := parseCronList(cronListSample)
	declared := []agentofficev1alpha1.AgentSchedule{
		{Name: "ub-wire-hourly", Cron: "5 * * * *", Message: "sweep"},   // identical: adopted, no op
		{Name: "ub-anchor-2h", Cron: "45 */2 * * *", Message: "review"}, // missing: add
	}
	ops := planSchedules("upstreambeat-wire", declared, []string{"ub-old", "ub-wire-hourly"}, jobs)
	kinds := map[string]string{}
	for _, o := range ops {
		kinds[o.Name] = o.Kind
	}
	if kinds["ub-wire-hourly"] != "" {
		t.Fatalf("an identical running job must be adopted without churn, got %v", ops)
	}
	if kinds["ub-anchor-2h"] != "add" {
		t.Fatalf("a missing declared job must be added, got %v", ops)
	}
	if kinds["ub-old"] != "rm" {
		t.Fatalf("a previously declared name that left the spec must be removed, got %v", ops)
	}
	if _, touched := kinds["other-agent-job"]; touched {
		t.Fatalf("another agent's job must never be touched, got %v", ops)
	}
	// Drift: same name, different cron -> edit in place with the job id.
	ops = planSchedules("upstreambeat-wire", []agentofficev1alpha1.AgentSchedule{{Name: "ub-wire-hourly", Cron: "10 * * * *", Message: "sweep"}}, nil, jobs)
	if len(ops) != 1 || ops[0].Kind != "edit" || ops[0].Args[3] != "j1" {
		t.Fatalf("drift must be an edit by id, got %v", ops)
	}
	// The add command carries the message as its own argv entry (quotes and newlines survive) and never a shell.
	ops = planSchedules("a", []agentofficev1alpha1.AgentSchedule{{Name: "n", Cron: "* * * * *", Message: "say \"hi\"\nthen stop"}}, nil, nil)
	if ops[0].Args[4] != "say \"hi\"\nthen stop" || ops[0].Args[0] != "openclaw" {
		t.Fatalf("message must be passed verbatim as argv, got %q", ops[0].Args)
	}
}

// v1.7.76: a block-scalar message (trailing newline) must adopt the stored,
// trimmed job silently instead of editing it on every reconcile.
func TestPlanSchedulesTrailingNewlineIsNotDrift(t *testing.T) {
	declared := []agentofficev1alpha1.AgentSchedule{{Name: "ub-scout-daily", Cron: "10 13 * * *", Message: "Shift trigger (daily): run your pass.\n"}}
	jobs := []cronJob{{ID: "j1", Name: "ub-scout-daily", AgentID: "upstreambeat-scout"}}
	jobs[0].Schedule.Expr = "10 13 * * *"
	jobs[0].Payload.Message = "Shift trigger (daily): run your pass."
	if ops := planSchedules("upstreambeat-scout", declared, []string{"ub-scout-daily"}, jobs); len(ops) != 0 {
		t.Fatalf("expected no ops for a trailing-newline-only difference, got %+v", ops)
	}
	// and when it IS missing, the add sends the trimmed message
	ops := planSchedules("upstreambeat-scout", declared, nil, nil)
	if len(ops) != 1 || ops[0].Kind != "add" || ops[0].Args[4] != "Shift trigger (daily): run your pass." {
		t.Fatalf("expected one add with trimmed message, got %+v", ops)
	}
}
