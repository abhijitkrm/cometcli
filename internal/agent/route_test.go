package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/router"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func routedAgent(t *testing.T, prov *mockProvider, tools ...toolkit.Tool) *Agent {
	t.Helper()
	a := newTestAgent(t, prov, tools...)
	cat, err := router.Load()
	if err != nil {
		t.Fatal(err)
	}
	a.catalog = cat
	return a
}

func jailedTool(runs *int, jailed int, fail bool) stubTool {
	return stubTool{name: "chain.validators", tier: toolkit.TierObserve, run: func(_ *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
		*runs++
		if fail {
			return nil, fmt.Errorf("dial 127.0.0.1:9090: connection refused")
		}
		if a.String("status", "") != "jailed" {
			return nil, fmt.Errorf("want status=jailed, got %q", a.String("status", ""))
		}
		return &toolkit.Result{Text: fmt.Sprintf("val3 JAILED\ntotal: %d\n", jailed), Data: map[string]any{"count": jailed}}, nil
	}}
}

func TestKnownQuestionsAreAnsweredWithoutTheModel(t *testing.T) {
	prov := &mockProvider{}
	runs := 0
	a := routedAgent(t, prov, jailedTool(&runs, 1, false))
	out, err := a.Run(context.Background(), "is anyone jailed?")
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls != 0 || runs != 1 {
		t.Fatalf("model calls=%d tool runs=%d", prov.calls, runs)
	}
	if !strings.Contains(out, "1 validator(s) jailed") || !strings.Contains(out, "answered locally") {
		t.Fatalf("answer: %q", out)
	}
	// asked again within the TTL: served from the cache, nothing re-run
	if _, err := a.Run(context.Background(), "any validators jailed"); err != nil {
		t.Fatal(err)
	}
	if r := a.Routes(); r.Local != 1 || r.Cached != 1 || r.Model != 0 || runs != 1 {
		t.Fatalf("routes=%+v runs=%d", r, runs)
	}
	// the conversation keeps the local answers for later model turns
	if len(a.history) != 4 || !strings.Contains(a.history[1].Text, "answered locally by cometcli") {
		t.Fatalf("history: %+v", a.history)
	}
}

func TestOpenQuestionsAndForcedPromptsGoToTheModel(t *testing.T) {
	prov := &mockProvider{}
	runs := 0
	a := routedAgent(t, prov, jailedTool(&runs, 1, false))
	for _, q := range []string{"why is val3 jailed?", forceModel + "is anyone jailed?"} {
		if _, err := a.Run(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if prov.calls != 2 || runs != 0 || a.Routes().Model != 2 {
		t.Fatalf("model calls=%d runs=%d routes=%+v", prov.calls, runs, a.Routes())
	}
	if strings.Contains(prov.sent[1], forceModel) {
		t.Error("the /llm marker must not reach the model")
	}
	if !strings.Contains(a.RouteWhy(), "sent to the model") {
		t.Errorf("RouteWhy = %q", a.RouteWhy())
	}
}

func TestFailedLookupFallsBackToTheModel(t *testing.T) {
	prov := &mockProvider{}
	runs := 0
	a := routedAgent(t, prov, jailedTool(&runs, 0, true))
	if _, err := a.Run(context.Background(), "is anyone jailed?"); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || prov.calls != 1 || !strings.Contains(a.RouteWhy(), "failed") {
		t.Fatalf("runs=%d calls=%d why=%q", runs, prov.calls, a.RouteWhy())
	}
}

func TestRouterOnlyRunsReadOnlyTools(t *testing.T) {
	prov := &mockProvider{}
	ran := false
	// a catalog entry pointing at a tool that changes things is refused
	sneaky := stubTool{name: "node.peers", tier: toolkit.TierLocalChange, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		ran = true
		return &toolkit.Result{}, nil
	}}
	a := routedAgent(t, prov, sneaky)
	if _, err := a.Run(context.Background(), "how many peers"); err != nil {
		t.Fatal(err)
	}
	if ran || prov.calls != 1 {
		t.Fatalf("ran=%v model calls=%d", ran, prov.calls)
	}
}

func TestRouterOff(t *testing.T) {
	prov := &mockProvider{}
	runs := 0
	a := routedAgent(t, prov, jailedTool(&runs, 1, false))
	a.conf.Router = "off"
	if _, err := a.Run(context.Background(), "is anyone jailed?"); err != nil {
		t.Fatal(err)
	}
	if prov.calls != 1 || runs != 0 {
		t.Fatalf("router off: calls=%d runs=%d", prov.calls, runs)
	}
}
