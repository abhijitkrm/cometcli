package tui

import (
	"context"
	"github.com/abhijitkrm/cometcli/internal/agent"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

type fakeTool struct {
	name string
	tier toolkit.Tier
	text string
}

func (f fakeTool) Name() string           { return f.name }
func (f fakeTool) Desc() string           { return "fake " + f.name }
func (f fakeTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (f fakeTool) Tier() toolkit.Tier     { return f.tier }
func (f fakeTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	return &toolkit.Result{Text: f.text}, nil
}

func appTestCtx() *toolkit.Context {
	return &toolkit.Context{
		Context: context.Background(),
		Profile: &config.Profile{Name: "t"},
		Cfg: &config.Config{Profiles: map[string]*config.Profile{
			"a": {Name: "a"}, "b": {Name: "b"},
		}},
	}
}

func testReg() *toolkit.Registry {
	r := toolkit.NewRegistry()
	r.Register(fakeTool{name: "node.status", tier: toolkit.TierObserve, text: "ok"})
	r.Register(fakeTool{name: "val.unjail", tier: toolkit.TierOnChain, text: "nope"})
	return r
}

func key(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestApp_TabSwitch(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	m.tab = tabOverview // digits switch panes except on chat
	m2, _ := m.Update(key("3"))
	if m2.(*AppModel).tab != tabFleet {
		t.Fatal("key 3 must select fleet tab")
	}
	m3, _ := m2.Update(key("2"))
	if m3.(*AppModel).tab != tabOverview {
		t.Fatal("key 2 must select overview tab")
	}
	m4, _ := m3.Update(key("1"))
	if m4.(*AppModel).tab != tabChat {
		t.Fatal("key 1 must select chat tab")
	}
}

func TestApp_FleetRenders(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	m2, _ := m.Update(fleetMsg{
		"a": {TS: time.Now(), Reachable: true, Height: 10, Peers: 2},
		"b": {TS: time.Now(), Reachable: false},
	})
	view := m2.(*AppModel).fleetView()
	if !strings.Contains(view, "UNREACHABLE") || !strings.Contains(view, "a") {
		t.Fatalf("fleet view missing rows:\n%s", view)
	}
}

func TestApp_ToolRunResultLandsInViewport(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	m2, _ := m.Update(toolResultMsg{name: "node.status", text: "height 42"})
	v := m2.(*AppModel)
	if v.toolBusy != "" {
		t.Fatal("toolBusy must clear after result")
	}
	if !strings.Contains(v.vp.View(), "height 42") {
		t.Fatalf("viewport missing tool output:\n%s", v.vp.View())
	}
}

func TestApp_QuitKey(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	// q quits from dashboard panes…
	m.tab = tabOverview
	m2, cmd := m.Update(key("q"))
	if !m2.(*AppModel).quitting || cmd == nil {
		t.Fatal("q must quit from a dashboard pane")
	}
	// …while ctrl+c quits even from chat
	m3, _ := NewApp(appTestCtx(), testReg(), time.Second).Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m3.(*AppModel).quitting {
		t.Fatal("ctrl+c must quit from chat")
	}
}

func TestApp_ChatSlashRunWithoutAgent(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	// no provider configured in the test profile → agent is nil, /run still works
	cmd := m.chatSubmit("/run node.status")
	if cmd == nil {
		t.Fatal("/run must return a command")
	}
	msg := cmd()
	m2, _ := m.Update(msg)
	v := m2.(*AppModel)
	if !strings.Contains(v.chat.transcript(), "ok") {
		t.Fatalf("tool result missing from transcript:\n%s", v.chat.transcript())
	}
}

func TestApp_ChatPlainTextWithoutAgent(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	m.chatSubmit("is my validator healthy?")
	if !strings.Contains(m.chat.transcript(), "no agent provider") {
		t.Fatalf("expected provider warning:\n%s", m.chat.transcript())
	}
}

func TestApp_ChatAgentEventsAppend(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	m2, _ := m.Update(evAgent{agent.Event{Kind: agent.EvText, Text: "checking the node"}})
	m3, _ := m2.(*AppModel).Update(evAgent{agent.Event{Kind: agent.EvToolStart, Tool: "node.status", Tier: "observe"}})
	m4, _ := m3.(*AppModel).Update(evAgent{agent.Event{Kind: agent.EvToolResult, Tool: "node.status", Text: "h=42"}})
	tr := m4.(*AppModel).chat.transcript()
	for _, want := range []string{"checking the node", "node.status", "h=42"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("transcript missing %q:\n%s", want, tr)
		}
	}
}

func TestApp_ChatStreamingDeltasCollapseIntoOneBlock(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	before := len(m.chat.blocks)
	for _, d := range []string{"node ", "is ", "healthy"} {
		m.Update(evAgent{agent.Event{Kind: agent.EvDelta, Text: d}})
	}
	if got := m.chat.blocks[len(m.chat.blocks)-1]; got != "node is healthy" {
		t.Fatalf("live block = %q", got)
	}
	m.Update(evAgent{agent.Event{Kind: agent.EvText, Text: "node is healthy"}})
	if len(m.chat.blocks) != before+1 || m.chat.live != -1 {
		t.Fatalf("deltas+final should be one block; blocks=%d live=%d", len(m.chat.blocks)-before, m.chat.live)
	}
}

func TestApp_SendFormEditing(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	m.tab = tabSend
	// type into the "to" field
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("cosmos1abc")})
	v := m2.(*AppModel)
	if v.txTo != "cosmos1abc" {
		t.Fatalf("to field = %q", v.txTo)
	}
	// tab to amount, type
	m3, _ := v.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m3.(*AppModel).txField != 1 {
		t.Fatal("tab must advance field")
	}
	m4, _ := m3.(*AppModel).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("100")})
	if m4.(*AppModel).txAmt != "100" {
		t.Fatalf("amount field = %q", m4.(*AppModel).txAmt)
	}
	// backspace
	m5, _ := m4.(*AppModel).Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if m5.(*AppModel).txAmt != "10" {
		t.Fatal("backspace must delete")
	}
}

func TestApp_SendRequiresToAndAmount(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	m.tab = tabSend
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m2.(*AppModel).txErr == "" {
		t.Fatal("empty form must set an error, not broadcast")
	}
}

func TestApp_ApprovalModalBlocksAndResolves(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	req := approvalReq{prompt: "broadcast?", tier: toolkit.TierOnChain, resp: make(chan bool, 1)}
	m2, _ := m.Update(approvalReqMsg(req))
	v := m2.(*AppModel)
	if v.pending == nil {
		t.Fatal("pending approval must be set")
	}
	if !strings.Contains(v.View(), "approve?") {
		t.Fatal("modal must render the prompt")
	}
	m3, _ := v.Update(key("y"))
	if m3.(*AppModel).pending != nil {
		t.Fatal("y must clear the modal")
	}
	select {
	case ok := <-req.resp:
		if !ok {
			t.Fatal("y must approve")
		}
	case <-time.After(time.Second):
		t.Fatal("response channel never received the answer")
	}
}
