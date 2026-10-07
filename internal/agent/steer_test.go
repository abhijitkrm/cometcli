package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func TestSteerReachesTheNextStep(t *testing.T) {
	var a *Agent
	slow := stubTool{name: "node.status", tier: toolkit.TierObserve, run: func(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
		a.Steer("stop — don't restart anything") // typed while the tool runs
		return &toolkit.Result{Text: "height 42"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "node__status", Args: json.RawMessage(`{}`)}}},
		{Text: "ok, stopping", Done: true},
	}}
	a = newTestAgent(t, prov, slow)
	if _, err := a.Run(context.Background(), "check and restart the node"); err != nil {
		t.Fatal(err)
	}
	last := prov.reqs[1].Messages
	got := last[len(last)-1]
	if got.Role != "tool" || !strings.Contains(got.Text, "stop — don't restart anything") || !strings.Contains(got.Text, "height 42") {
		t.Fatalf("steer not delivered with the tool result: %+v", got)
	}
	if left := a.TakeSteer(); len(left) != 0 {
		t.Fatalf("delivered message still queued: %v", left)
	}
}

func TestSteerTypedDuringFinalAnswerIsKept(t *testing.T) {
	prov := &mockProvider{responses: []*Response{{Text: "done", Done: true}}}
	a := newTestAgent(t, prov)
	a.Steer("also check peers")
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	// no tool step happened: the front end gets it back to send next
	if left := a.TakeSteer(); len(left) != 1 || left[0] != "also check peers" {
		t.Fatalf("left = %v", left)
	}
}
