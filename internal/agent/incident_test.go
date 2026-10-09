package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// incidentAgent has stub node tools that record what ran.
func incidentAgent(t *testing.T, prov *mockProvider, roots []string, sig kb.Signals, fail map[string]error) (*Agent, *[]string) {
	t.Helper()
	var ran []string
	stub := func(name string, tier toolkit.Tier, data map[string]any) stubTool {
		return stubTool{name: name, tier: tier, run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
			ran = append(ran, strings.TrimSpace(name+" "+a.String("action", "")+a.String("condition", "")))
			if name == "val.unjail" && !strings.Contains(c.Purpose, "known case node-process-down") {
				t.Errorf("the key use should say why: purpose %q", c.Purpose)
			}
			if err := fail[name]; err != nil {
				return nil, err
			}
			return &toolkit.Result{Text: name + " ok", Data: data}, nil
		}}
	}
	a := routedAgent(t, prov,
		stub("node.triage", toolkit.TierDiagnose, map[string]any{"roots": roots, "signals": sig}),
		stub("node.logs", toolkit.TierObserve, nil),
		stub("node.service", toolkit.TierLocalChange, nil),
		stub("wait.until", toolkit.TierObserve, nil),
		stub("val.unjail", toolkit.TierOnChain, nil),
		stub("incident.record", toolkit.TierDiagnose, nil),
	)
	return a, &ran
}

func runIncidentCmd(t *testing.T, a *Agent, what string) string {
	t.Helper()
	cmd, err := RunCommand(a, a.Ctx, a.Reg, "/incident "+what)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Run(context.Background(), cmd.Prompt)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestKnownIncidentRunsItsPlaybookLocally(t *testing.T) {
	prov := &mockProvider{}
	a, ran := incidentAgent(t, prov, []string{"node-process-down"}, kb.Signals{"proc.running": false, "val.jailed": true}, nil)
	out := runIncidentCmd(t, a, "val1 stopped signing")
	if prov.calls != 0 {
		t.Fatalf("a known case with a playbook needs no model call (calls=%d)", prov.calls)
	}
	want := []string{"node.triage", "node.logs", "node.service start", "wait.until synced", "wait.until unjailable", "val.unjail", "wait.until in-consensus", "incident.record"}
	if strings.Join(*ran, "|") != strings.Join(want, "|") {
		t.Fatalf("ran %v\nwant %v", *ran, want)
	}
	if !strings.Contains(out, "Resolved: Node process/container not running") || !strings.Contains(out, "no model call") {
		t.Fatalf("report: %s", out)
	}
	if a.Routes().Local != 1 {
		t.Errorf("routes = %+v", a.Routes())
	}
}

func TestPlaybookSkipsStepsWhoseConditionFails(t *testing.T) {
	prov := &mockProvider{}
	a, ran := incidentAgent(t, prov, []string{"node-process-down"}, kb.Signals{"proc.running": false, "val.jailed": false}, nil)
	out := runIncidentCmd(t, a, "")
	for _, r := range *ran {
		if r == "val.unjail" {
			t.Fatal("a validator that isn't jailed must not be unjailed")
		}
	}
	if !strings.Contains(out, "skipped val.unjail") {
		t.Errorf("report should say what was skipped:\n%s", out)
	}
}

func TestFailedPlaybookStepHandsOffWithContext(t *testing.T) {
	prov := &mockProvider{}
	a, _ := incidentAgent(t, prov, []string{"node-process-down"}, kb.Signals{"proc.running": false},
		map[string]error{"node.service": fmt.Errorf("container exited again: exit 1")})
	runIncidentCmd(t, a, "")
	if prov.calls == 0 {
		t.Fatal("a failed step goes to the model")
	}
	sent := prov.sent[0]
	for _, want := range []string{"already ran node.triage", "ran its playbook until a step failed", "container exited again", "node.logs"} {
		if !strings.Contains(sent, want) {
			t.Errorf("handoff lacks %q", want)
		}
	}
}

func TestDeclinedStepStopsWithoutTheModel(t *testing.T) {
	prov := &mockProvider{}
	a, ran := incidentAgent(t, prov, []string{"node-process-down"}, kb.Signals{"proc.running": false},
		map[string]error{"node.service": fmt.Errorf("denied: start the node")})
	out := runIncidentCmd(t, a, "")
	if prov.calls != 0 || !strings.Contains(out, "Stopped at step 2") {
		t.Fatalf("calls=%d out=%s", prov.calls, out)
	}
	if (*ran)[len(*ran)-1] != "node.service start" {
		t.Fatalf("nothing may run after a declined step: %v", *ran)
	}
}

func TestUnknownIncidentGoesToTheModelWithTriage(t *testing.T) {
	prov := &mockProvider{}
	a, ran := incidentAgent(t, prov, nil, kb.Signals{}, nil)
	runIncidentCmd(t, a, "something odd")
	if prov.calls == 0 || len(*ran) != 1 || !strings.Contains(prov.sent[0], "already ran node.triage") || !strings.Contains(prov.sent[0], "something odd") {
		t.Fatalf("calls=%d ran=%v", prov.calls, *ran)
	}
	// a case without a playbook goes the same way
	prov2 := &mockProvider{}
	a2, _ := incidentAgent(t, prov2, []string{"cfg-parse-error"}, kb.Signals{}, nil)
	runIncidentCmd(t, a2, "")
	if prov2.calls == 0 {
		t.Fatal("a case without a playbook needs the model")
	}
}

func TestReportPlaybookLeavesTheDecisionToTheOperator(t *testing.T) {
	prov := &mockProvider{}
	a, ran := incidentAgent(t, prov, []string{"val-gov-vote-due"}, kb.Signals{"gov.unvoted": float64(1)}, nil)
	a.Reg.Register(stubTool{name: "chain.gov", tier: toolkit.TierObserve, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		*ran = append(*ran, "chain.gov")
		return &toolkit.Result{Text: "#6 VOTING Upgrade to v0.6.1"}, nil
	}})
	a.Reg.Register(stubTool{name: "val.votes", tier: toolkit.TierObserve, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		*ran = append(*ran, "val.votes")
		return &toolkit.Result{Text: "prop #6 ✗ NOT VOTED"}, nil
	}})
	out := runIncidentCmd(t, a, "")
	if prov.calls != 0 || !strings.Contains(out, "Your decision") || strings.Contains(out, "Resolved") {
		t.Fatalf("calls=%d\n%s", prov.calls, out)
	}
	for _, r := range *ran {
		if r == "val.vote" {
			t.Fatal("a report playbook never votes for the operator")
		}
	}
}

func TestStepArgumentsComeFromSignals(t *testing.T) {
	prov := &mockProvider{}
	var got toolkit.Args
	a, _ := incidentAgent(t, prov, []string{"cfg-grpc-disabled"}, kb.Signals{"app.grpc_enable": false}, nil)
	a.Reg.Register(stubTool{name: "node.set-config", tier: toolkit.TierLocalChange, run: func(_ *toolkit.Context, args toolkit.Args) (*toolkit.Result, error) {
		got = args
		return &toolkit.Result{Text: "set grpc.enable = true"}, nil
	}})
	runIncidentCmd(t, a, "")
	if prov.calls != 0 || got["key"] != "grpc.enable" || got["value"] != "true" {
		t.Fatalf("calls=%d args=%v", prov.calls, got)
	}
	// a step that needs a signal the node didn't report goes to the model
	prov2 := &mockProvider{}
	b, _ := incidentAgent(t, prov2, []string{"cfg-db-backend-mismatch"}, kb.Signals{"config.db_backend_mismatch": true}, nil)
	b.Reg.Register(stubTool{name: "node.set-config", tier: toolkit.TierLocalChange, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		t.Fatal("must not run without the data backend known")
		return nil, nil
	}})
	runIncidentCmd(t, b, "")
	if prov2.calls == 0 || !strings.Contains(prov2.sent[0], "data.db_backend") {
		t.Fatalf("handoff should name the missing signal (calls=%d)", prov2.calls)
	}
}
