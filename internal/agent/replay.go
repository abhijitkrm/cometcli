package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// ReplayReport summarizes a replayed session.
type ReplayReport struct {
	Session  string
	Profile  string
	Prompts  int
	Rounds   int
	Tools    int
	Problems []string // divergences between recording and replay
}

// Replay re-runs a recorded session through the real agent loop, with a
// provider that returns the recorded LLM turns and tools that return the
// recorded output the model saw. Nothing reaches an LLM or the node, and
// the same events a live session emits are delivered to onEvent — so the
// transcript is reproduced deterministically.
func Replay(ctx context.Context, events []audit.Event, onEvent func(Event)) (*ReplayReport, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("no events for that session")
	}
	rep := &ReplayReport{Session: events[0].Session, Profile: events[0].Profile}
	prov := &replayProvider{}
	reg := toolkit.NewRegistry()
	recorded := map[string]*recordedTool{}
	var prompts []string

	for _, e := range events {
		switch e.Kind {
		case audit.KindPrompt:
			s, _ := e.Detail["text"].(string)
			prompts = append(prompts, s)
		case audit.KindLLM:
			prov.turns = append(prov.turns, responseFromAudit(e.Detail))
		case audit.KindTool:
			name, _ := e.Detail["name"].(string)
			tier, _ := e.Detail["tier"].(string)
			rt := recorded[name]
			if rt == nil {
				rt = &recordedTool{name: name, tier: parseTier(tier)}
				recorded[name] = rt
				reg.Register(rt)
			}
			out := recordedOut{}
			out.text, _ = e.Detail["seen"].(string)
			out.err, _ = e.Detail["error"].(string)
			rt.outs = append(rt.outs, out)
		}
	}
	if len(prompts) == 0 {
		return nil, fmt.Errorf("session %s has no prompts (not an agent session?)", rep.Session)
	}

	a := &Agent{
		Provider: prov, Model: "replay", Reg: reg,
		Ctx: &toolkit.Context{
			Context: ctx, Profile: &config.Profile{Name: rep.Profile},
			Approver: toolkit.DenyApprover,
		},
		MaxIter: 1 << 10,
		// recorded tools never call Approve; autopilot keeps the gate quiet
		Policy:     Policy{Mode: ModeOps, AutoLocal: true},
		SnapshotFn: func(*toolkit.Context) string { return "" },
		toolTrunc:  1 << 30, // recorded text was already truncated live
	}
	a.OnEvent = func(e Event) {
		if e.Kind == EvToolStart {
			rep.Tools++
		}
		if onEvent != nil {
			onEvent(e)
		}
	}
	for _, p := range prompts {
		if onEvent != nil {
			onEvent(Event{Kind: "prompt", Text: p})
		}
		rep.Prompts++
		if _, err := a.Run(ctx, p); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("prompt %d: %v", rep.Prompts, err))
		}
	}
	rep.Rounds = prov.next
	if prov.next < len(prov.turns) {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d recorded LLM turn(s) never replayed", len(prov.turns)-prov.next))
	}
	for _, rt := range recorded {
		if rt.next < len(rt.outs) {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %d recorded result(s) never consumed", rt.name, len(rt.outs)-rt.next))
		}
		rep.Problems = append(rep.Problems, rt.problems...)
	}
	return rep, nil
}

func responseFromAudit(d map[string]any) *Response {
	r := &Response{}
	r.Text, _ = d["text"].(string)
	r.Done, _ = d["done"].(bool)
	calls, _ := d["calls"].([]any)
	for _, c := range calls {
		m, _ := c.(map[string]any)
		id, _ := m["id"].(string)
		name, _ := m["name"].(string)
		args, _ := m["args"].(string)
		if !json.Valid([]byte(args)) {
			args = "{}"
		}
		r.Calls = append(r.Calls, Call{ID: id, Name: name, Args: json.RawMessage(args)})
	}
	return r
}

type replayProvider struct {
	turns []*Response
	next  int
}

func (p *replayProvider) Name() string { return "replay" }

func (p *replayProvider) Chat(context.Context, *Request) (*Response, error) {
	if p.next >= len(p.turns) {
		return nil, fmt.Errorf("recording exhausted after %d LLM turn(s)", p.next)
	}
	r := p.turns[p.next]
	p.next++
	return r, nil
}

type recordedOut struct{ text, err string }

type recordedTool struct {
	name     string
	tier     toolkit.Tier
	outs     []recordedOut
	next     int
	problems []string
}

func (t *recordedTool) Name() string           { return t.name }
func (t *recordedTool) Desc() string           { return "recorded " + t.name }
func (t *recordedTool) Tier() toolkit.Tier     { return t.tier }
func (t *recordedTool) Schema() map[string]any { return toolkit.ObjSchema(map[string]any{}) }

func (t *recordedTool) Run(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
	if t.next >= len(t.outs) {
		t.problems = append(t.problems, t.name+": called more times than recorded")
		return nil, fmt.Errorf("no recorded result")
	}
	o := t.outs[t.next]
	t.next++
	if o.err != "" {
		return nil, fmt.Errorf("%s", o.err)
	}
	return &toolkit.Result{Text: o.text}, nil
}
