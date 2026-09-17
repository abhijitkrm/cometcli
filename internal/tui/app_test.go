package tui

import (
	"context"
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

func TestApp_OnlyReadOnlyToolsListed(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	for _, tool := range m.tools {
		if tool.Tier() > toolkit.TierDiagnose {
			t.Fatalf("mutating tool %q must not appear in the runner", tool.Name())
		}
	}
	if len(m.tools) != 1 || m.tools[0].Name() != "node.status" {
		t.Fatalf("expected only node.status, got %v", m.tools)
	}
}

func TestApp_TabSwitch(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	m2, _ := m.Update(key("2"))
	if m2.(*AppModel).tab != tabFleet {
		t.Fatal("key 2 must select fleet tab")
	}
	m3, _ := m2.Update(key("1"))
	if m3.(*AppModel).tab != tabOverview {
		t.Fatal("key 1 must select overview tab")
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
	m2, cmd := m.Update(key("q"))
	if !m2.(*AppModel).quitting || cmd == nil {
		t.Fatal("q must quit")
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
