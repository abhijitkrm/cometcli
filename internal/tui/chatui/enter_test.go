package chatui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestEnterSubmitsExactSlashCommand(t *testing.T) {
	m, _ := newTestModel(t)
	typeText(m, "/status")
	if len(m.sugg) == 0 {
		t.Fatal("no menu")
	}
	key(m, tea.KeyEnter)
	if m.ta.Value() != "" {
		t.Fatalf("not submitted, input = %q, sugg=%v", m.ta.Value(), m.sugg)
	}
	if !strings.Contains(strings.Join(m.printed, "\n"), "scope:") {
		t.Fatalf("status not shown:\n%s", strings.Join(m.printed, "\n"))
	}
}
