package chatui

import (
	"context"
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

func TestTypingDuringATurnSteersAndEscSendsIt(t *testing.T) {
	m, _ := newTestModel(t)
	m.running = true
	cancelled := false
	m.cancel = func() { cancelled = true }
	m.submit("okay lets stop")
	if !m.steering || !strings.Contains(m.printed[len(m.printed)-1], "esc") {
		t.Fatalf("no steer hint: %q", m.printed[len(m.printed)-1])
	}
	if !strings.Contains(m.View(), "your message goes in after this step") {
		t.Fatal("waiting indicator missing")
	}
	key(m, tea.KeyEsc)
	if !cancelled {
		t.Fatal("esc did not interrupt")
	}
	m.onDone(doneMsg{err: context.Canceled})
	if !m.running {
		t.Fatal("the queued message was not sent after the interrupt")
	}
	if !strings.Contains(strings.Join(m.printed, "\n"), "sending your message") {
		t.Fatal("no notice that the message is being sent")
	}
}
