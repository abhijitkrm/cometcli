package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/monitor"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func testCtx() *toolkit.Context {
	return &toolkit.Context{
		Context: context.Background(),
		Profile: &config.Profile{Name: "t"},
	}
}

func TestWatchModel_RendersSnapshot(t *testing.T) {
	m := NewWatch(testCtx(), time.Second)
	// feed a snapshot as the collect command would
	up := true
	snap := &monitor.Snapshot{
		TS: time.Now(), Reachable: true, Height: 1234, Peers: 3,
		VotingPower: 100, Window: 100, Missed: 2, UptimePct: 98,
		ServiceUp: &up, DiskUsedPct: 55, EVMHeight: 1234, EVMDrift: 0,
	}
	m2, _ := m.Update(snap)
	view := m2.(*WatchModel).View()
	for _, want := range []string{"height 1234", "peers", "98.00%", "missed 2/100", "disk"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestWatchModel_Unreachable(t *testing.T) {
	m := NewWatch(testCtx(), time.Second)
	m2, _ := m.Update(&monitor.Snapshot{TS: time.Now(), Reachable: false})
	if !strings.Contains(m2.(*WatchModel).View(), "RPC UNREACHABLE") {
		t.Fatal("dead endpoint must render UNREACHABLE, not hang")
	}
}

func TestWatchModel_QuitKey(t *testing.T) {
	m := NewWatch(testCtx(), time.Second)
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	wm := m2.(*WatchModel)
	if !wm.quitting {
		t.Fatal("q must set quitting")
	}
	if cmd == nil {
		t.Fatal("q must return tea.Quit")
	}
}

func TestWatchModel_InitCommands(t *testing.T) {
	m := NewWatch(testCtx(), 50*time.Millisecond)
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init must schedule collect+tick")
	}
}

func TestRunWatch_NonTTY(t *testing.T) {
	// piped stdout / no controlling terminal: bubbletea must return an
	// error, not hang or crash the process.
	done := make(chan error, 1)
	go func() { done <- RunWatch(testCtx(), time.Second) }()
	select {
	case <-done:
		// returned (error or nil) — fine
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch hung without a TTY")
	}
}
