// Package tui holds bubbletea terminal UIs: the watch dashboard.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/abhijitkrm/cometcli/internal/monitor"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

var (
	title  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	ok     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	bad    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	warn   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	rowKey = lipgloss.NewStyle().Width(16).Foreground(lipgloss.Color("252"))
)

type tickMsg time.Time

// WatchModel renders the validator dashboard.
type WatchModel struct {
	c        *toolkit.Context
	interval time.Duration
	snap     *monitor.Snapshot
	quitting bool
}

// NewWatch creates the model.
func NewWatch(c *toolkit.Context, interval time.Duration) *WatchModel {
	return &WatchModel{c: c, interval: interval}
}

func (m *WatchModel) Init() tea.Cmd {
	return tea.Batch(m.collect(), tick(m.interval))
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *WatchModel) collect() tea.Cmd {
	return func() tea.Msg { return monitor.Collect(m.c) }
}

func (m *WatchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.KeyMsg:
		switch v.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}
	case *monitor.Snapshot:
		m.snap = v
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.collect(), tick(m.interval))
	}
	return m, nil
}

func (m *WatchModel) View() string {
	if m.snap == nil {
		return title.Render("cometcli watch") + "\ncollecting…\n"
	}
	s := m.snap
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  %s\n\n",
		title.Render("cometcli watch"),
		m.c.Profile.Name,
		dim.Render(s.TS.Format("15:04:05")))

	row := func(k, v string, st lipgloss.Style) {
		fmt.Fprintf(&b, "%s %s\n", rowKey.Render(k), st.Render(v))
	}
	if !s.Reachable {
		row("status", "RPC UNREACHABLE", bad)
	} else {
		st := ok
		if s.CatchingUp {
			st = warn
		}
		row("status", fmt.Sprintf("height %d  catching_up=%v", s.Height, s.CatchingUp), st)
	}
	row("peers", fmt.Sprint(s.Peers), ok)
	row("voting power", fmt.Sprint(s.VotingPower), ok)
	if s.Window > 0 {
		st := ok
		if s.Missed > s.Window/10 {
			st = bad
		} else if s.Missed > 0 {
			st = warn
		}
		row("signing", fmt.Sprintf("missed %d/%d  uptime %.2f%%", s.Missed, s.Window, s.UptimePct), st)
	}
	if s.Tombstoned {
		row("jail", "TOMBSTONED", bad)
	} else if s.Jailed {
		row("jail", "JAILED", bad)
	}
	if s.ServiceUp != nil {
		if *s.ServiceUp {
			row("service", "active", ok)
		} else {
			row("service", "INACTIVE", bad)
		}
	}
	if s.DiskUsedPct > 0 {
		st := ok
		if s.DiskUsedPct > 85 {
			st = bad
		} else if s.DiskUsedPct > 70 {
			st = warn
		}
		row("disk", fmt.Sprintf("%.0f%% used", s.DiskUsedPct), st)
	}
	if s.EVMHeight > 0 {
		st := ok
		if s.EVMDrift > 25 {
			st = bad
		}
		row("evm", fmt.Sprintf("height %d  drift %d", s.EVMHeight, s.EVMDrift), st)
	}
	for _, e := range s.Errors {
		fmt.Fprintf(&b, "%s %s\n", rowKey.Render("warn"), dim.Render(e))
	}
	fmt.Fprintf(&b, "\n%s\n", dim.Render("q to quit"))
	return b.String()
}

// RunWatch starts the TUI program.
func RunWatch(c *toolkit.Context, interval time.Duration) error {
	p := tea.NewProgram(NewWatch(c, interval))
	_, err := p.Run()
	return err
}
