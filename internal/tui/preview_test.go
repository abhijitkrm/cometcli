package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestChatLayout asserts the chat pane composes status, transcript, input.
func TestChatLayout(t *testing.T) {
	m := NewApp(appTestCtx(), testReg(), time.Second)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 92, Height: 30})
	m = mm.(*AppModel)
	m.chatSubmit("is my validator healthy?")
	for _, ev := range []tea.Msg{
		evText{"Checking node status…"},
		evToolCall{name: "node.status", args: map[string]any{}},
		evToolRes{name: "node.status", summary: "h=20496 catching_up=false"},
		evDone{},
	} {
		mm, _ := m.Update(ev)
		m = mm.(*AppModel)
	}
	v := m.View()
	for _, want := range []string{"Chat", "Overview", "no agent", "is my validator healthy?",
		"node.status", "h=20496", "❯", "/help"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view missing %q:\n%s", want, v)
		}
	}
	if m.chat.busy {
		t.Fatal("evDone must clear busy")
	}
}
