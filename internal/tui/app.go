package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/monitor"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

type tab int

const (
	tabOverview tab = iota
	tabFleet
	tabLogs
	tabTools
)

var (
	tabStyle  = lipgloss.NewStyle().Padding(0, 1).Foreground(lipgloss.Color("240"))
	activeTab = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))
	selStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))
	headStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
)

type fleetMsg map[string]*monitor.Snapshot
type logsMsg string
type toolResultMsg struct {
	name, text string
	err        error
}

// AppModel is the multi-pane `cometcli ui` application.
type AppModel struct {
	c        *toolkit.Context
	reg      *toolkit.Registry
	interval time.Duration

	tab      tab
	snap     *monitor.Snapshot
	fleet    map[string]*monitor.Snapshot
	tools    []toolkit.Tool
	toolSel  int
	toolBusy string

	vp       viewport.Model
	width    int
	height   int
	quitting bool
}

// NewApp creates the app model.
func NewApp(c *toolkit.Context, reg *toolkit.Registry, interval time.Duration) *AppModel {
	var tools []toolkit.Tool
	for _, t := range reg.All() {
		if t.Tier() <= toolkit.TierDiagnose {
			tools = append(tools, t)
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })
	vp := viewport.New(80, 20)
	return &AppModel{c: c, reg: reg, interval: interval, tools: tools, vp: vp}
}

func (m *AppModel) Init() tea.Cmd {
	return tea.Batch(m.collectSnap(), m.collectFleet(), tick(m.interval))
}

func (m *AppModel) collectSnap() tea.Cmd {
	return func() tea.Msg { return monitor.Collect(m.c) }
}

func (m *AppModel) collectFleet() tea.Cmd {
	return func() tea.Msg {
		out := fleetMsg{}
		if m.c.Cfg == nil {
			return out
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for name, p := range m.c.Cfg.Profiles {
			wg.Add(1)
			go func(name string, p *config.Profile) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				sub := &toolkit.Context{Context: ctx, Profile: p, Cfg: m.c.Cfg, Audit: m.c.Audit}
				mu.Lock()
				out[name] = monitor.Collect(sub)
				mu.Unlock()
			}(name, p)
		}
		wg.Wait()
		return out
	}
}

func (m *AppModel) collectLogs() tea.Cmd {
	t, ok := m.reg.Get("node.logs")
	if !ok {
		return func() tea.Msg { return logsMsg("node.logs tool not registered") }
	}
	return func() tea.Msg {
		res, err := t.Run(m.c, toolkit.Args{"lines": 60})
		if err != nil {
			return logsMsg("error: " + err.Error())
		}
		return logsMsg(res.Text)
	}
}

func (m *AppModel) runTool(t toolkit.Tool) tea.Cmd {
	return func() tea.Msg {
		res, err := t.Run(m.c, toolkit.Args{})
		if err != nil {
			return toolResultMsg{name: t.Name(), err: err}
		}
		return toolResultMsg{name: t.Name(), text: res.Text}
	}
}

func (m *AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
		m.vp.Width = v.Width - 4
		m.vp.Height = v.Height - 8
		return m, nil
	case tea.KeyMsg:
		switch v.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "1", "2", "3", "4":
			m.tab = tab(int(v.String()[0] - '1'))
			if m.tab == tabLogs {
				return m, m.collectLogs()
			}
			return m, nil
		case "tab":
			m.tab = (m.tab + 1) % 4
			if m.tab == tabLogs {
				return m, m.collectLogs()
			}
			return m, nil
		case "shift+tab":
			m.tab = (m.tab + 3) % 4
			return m, nil
		case "j", "down":
			if m.tab == tabTools && m.toolSel < len(m.tools)-1 {
				m.toolSel++
			} else {
				m.vp.ScrollDown(1)
			}
		case "k", "up":
			if m.tab == tabTools && m.toolSel > 0 {
				m.toolSel--
			} else {
				m.vp.ScrollUp(1)
			}
		case "d", "pgdown":
			m.vp.HalfPageDown()
		case "u", "pgup":
			m.vp.HalfPageUp()
		case "r":
			cmds := []tea.Cmd{m.collectSnap(), m.collectFleet()}
			if m.tab == tabLogs {
				cmds = append(cmds, m.collectLogs())
			}
			return m, tea.Batch(cmds...)
		case "enter":
			if m.tab == tabTools && m.toolSel < len(m.tools) && m.toolBusy == "" {
				t := m.tools[m.toolSel]
				m.toolBusy = t.Name()
				return m, m.runTool(t)
			}
		}
	case *monitor.Snapshot:
		m.snap = v
	case fleetMsg:
		m.fleet = v
	case logsMsg:
		m.vp.SetContent(string(v))
		m.vp.GotoBottom()
	case toolResultMsg:
		m.toolBusy = ""
		if v.err != nil {
			m.vp.SetContent(fmt.Sprintf("$ %s\n\nerror: %v", v.name, v.err))
		} else {
			m.vp.SetContent(fmt.Sprintf("$ %s\n\n%s", v.name, v.text))
		}
	case tickMsg:
		return m, tea.Batch(m.collectSnap(), m.collectFleet(), tick(m.interval))
	}
	return m, nil
}

func (m *AppModel) View() string {
	var b strings.Builder
	tabs := []string{"Overview", "Fleet", "Logs", "Tools"}
	for i, t := range tabs {
		if tab(i) == m.tab {
			b.WriteString(activeTab.Render(t))
		} else {
			b.WriteString(tabStyle.Render(t))
		}
	}
	clock := ""
	if m.snap != nil {
		clock = m.snap.TS.Format("15:04:05")
	}
	fmt.Fprintf(&b, "  %s  %s\n\n", headStyle.Render(m.c.Profile.Name), dim.Render(clock))

	switch m.tab {
	case tabOverview:
		b.WriteString(m.overviewView())
	case tabFleet:
		b.WriteString(m.fleetView())
	case tabLogs, tabTools:
		b.WriteString(m.vp.View())
	}
	if m.tab == tabTools {
		b.WriteString("\n" + m.toolList())
	}
	help := "1-4/tab: panes · j/k scroll · r refresh · q quit"
	if m.tab == tabTools {
		help = "j/k select · enter run · q quit"
	}
	fmt.Fprintf(&b, "\n%s\n", dim.Render(help))
	return b.String()
}

func (m *AppModel) overviewView() string {
	if m.snap == nil {
		return "collecting…\n"
	}
	return snapshotRows(m.snap)
}

func (m *AppModel) fleetView() string {
	if len(m.fleet) == 0 {
		return "collecting fleet…\n"
	}
	var names []string
	for n := range m.fleet {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "%-18s %-8s %-6s %-14s %-6s %s\n",
		rowKey.Render("PROFILE"), rowKey.Render("HEIGHT"), rowKey.Render("PEERS"),
		rowKey.Render("SIGNING"), rowKey.Render("DISK"), rowKey.Render("STATUS"))
	for _, n := range names {
		s := m.fleet[n]
		status := ok.Render("ok")
		switch {
		case !s.Reachable:
			status = bad.Render("UNREACHABLE")
		case s.Tombstoned:
			status = bad.Render("TOMBSTONED")
		case s.Jailed:
			status = bad.Render("JAILED")
		case s.CatchingUp:
			status = warn.Render("catching-up")
		case s.Peers == 0:
			status = warn.Render("no peers")
		}
		signing := "-"
		if s.Window > 0 {
			signing = fmt.Sprintf("%d/%d missed", s.Missed, s.Window)
		}
		fmt.Fprintf(&b, "%-18s %-8d %-6d %-14s %-5.0f%% %s\n",
			n, s.Height, s.Peers, signing, s.DiskUsedPct, status)
	}
	return b.String()
}

func (m *AppModel) toolList() string {
	var b strings.Builder
	// keep the picker to a screenful around the selection
	const rows = 12
	start := 0
	if m.toolSel >= rows {
		start = m.toolSel - rows + 1
	}
	for i := start; i < len(m.tools) && i < start+rows; i++ {
		t := m.tools[i]
		line := fmt.Sprintf(" %-22s %s", t.Name(), dim.Render(t.Desc()))
		if i == m.toolSel {
			if m.toolBusy == t.Name() {
				line = fmt.Sprintf(" %-22s %s", t.Name(), warn.Render("running…"))
			}
			b.WriteString(selStyle.Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// RunApp starts the multi-pane TUI.
func RunApp(c *toolkit.Context, reg *toolkit.Registry, interval time.Duration) error {
	p := tea.NewProgram(NewApp(c, reg, interval), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
