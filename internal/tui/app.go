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
	tabSend
)

// approvalReq is a synchronous toolkit approval bridged through the
// bubbletea event loop — the tool goroutine blocks until the user answers.
type approvalReq struct {
	prompt string
	tier   toolkit.Tier
	detail map[string]any
	resp   chan bool
}

type approvalReqMsg approvalReq

// tuiApprover implements toolkit.Approver by parking the tool goroutine on
// a channel while the UI shows a y/n modal.
type tuiApprover struct {
	req chan approvalReq
}

func (a *tuiApprover) approve(_ *toolkit.Context, prompt string, tier toolkit.Tier, detail map[string]any) (bool, error) {
	r := approvalReq{prompt: prompt, tier: tier, detail: detail, resp: make(chan bool)}
	a.req <- r
	return <-r.resp, nil
}

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

	// tx send pane
	txTo    string
	txAmt   string
	txGas   string
	txMemo  string
	txField int
	txErr   string

	approver *tuiApprover
	pending  *approvalReq

	vp       viewport.Model
	width    int
	height   int
	quitting bool
}

// NewApp creates the app model.
func NewApp(c *toolkit.Context, reg *toolkit.Registry, interval time.Duration) *AppModel {
	var tools []toolkit.Tool
	for _, t := range reg.All() {
		if t.Tier() > toolkit.TierDiagnose {
			continue // never mutating tools in the runner
		}
		// only tools runnable with zero args — the UI can't fill schemas yet
		if req, _ := t.Schema()["required"].([]string); len(req) > 0 {
			continue
		}
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })
	vp := viewport.New(80, 20)
	return &AppModel{c: c, reg: reg, interval: interval, tools: tools, vp: vp,
		approver: &tuiApprover{req: make(chan approvalReq)}, txGas: "1e9"}
}

// waitApproval parks on the approver channel — emits approvalReqMsg when a
// tool run inside the UI needs a human decision.
func (m *AppModel) waitApproval() tea.Cmd {
	return func() tea.Msg { return approvalReqMsg(<-m.approver.req) }
}

func (m *AppModel) Init() tea.Cmd {
	return tea.Batch(m.collectSnap(), m.collectFleet(), tick(m.interval), m.waitApproval())
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

// subCtx builds a fresh Context for in-app tool runs. Read-only tiers are
// auto-approved (the Tools pane only lists observe/diagnose anyway); any
// explicit Approve call still routes through the modal instead of stdin,
// which bubbletea owns in raw mode.
func (m *AppModel) subCtx() *toolkit.Context {
	return &toolkit.Context{
		Context: m.c.Context, Profile: m.c.Profile, Cfg: m.c.Cfg,
		Out: m.c.Out, Audit: m.c.Audit,
		Approver:         m.approver.approve,
		AutoApproveBelow: toolkit.TierLocalChange,
	}
}

func (m *AppModel) collectLogs() tea.Cmd {
	t, ok := m.reg.Get("node.logs")
	if !ok {
		return func() tea.Msg { return logsMsg("node.logs tool not registered") }
	}
	return func() tea.Msg {
		res, err := t.Run(m.subCtx(), toolkit.Args{"lines": 60})
		if err != nil {
			return logsMsg("error: " + err.Error())
		}
		return logsMsg(res.Text)
	}
}

func (m *AppModel) runTool(t toolkit.Tool) tea.Cmd {
	return func() tea.Msg {
		res, err := t.Run(m.subCtx(), toolkit.Args{})
		if err != nil {
			return toolResultMsg{name: t.Name(), err: err}
		}
		return toolResultMsg{name: t.Name(), text: res.Text}
	}
}

// runSend builds + broadcasts tx.send through the TUI approval gate.
func (m *AppModel) runSend() tea.Cmd {
	t, ok := m.reg.Get("tx.send")
	if !ok {
		return func() tea.Msg { return toolResultMsg{name: "tx.send", err: fmt.Errorf("not registered")} }
	}
	args := toolkit.Args{
		"to": m.txTo, "amount": m.txAmt, "memo": m.txMemo,
		"gas-price": m.txGas,
	}
	return func() tea.Msg {
		sub := m.subCtx()
		sub.AutoApproveBelow = 0 // txs always prompt — never auto-approve in the UI
		res, err := t.Run(sub, args)
		if err != nil {
			return toolResultMsg{name: "tx.send", err: err}
		}
		return toolResultMsg{name: "tx.send", text: res.Text}
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
		// pending approval modal captures y/n above everything
		if m.pending != nil {
			switch v.String() {
			case "y", "Y":
				m.pending.resp <- true
			case "n", "N", "esc", "ctrl+c":
				m.pending.resp <- false
			}
			m.pending = nil
			return m, m.waitApproval()
		}
		if m.tab == tabSend {
			return m.updateSend(v)
		}
		switch v.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "1", "2", "3", "4", "5":
			m.tab = tab(int(v.String()[0] - '1'))
			if m.tab == tabLogs {
				return m, m.collectLogs()
			}
			return m, nil
		case "tab", "]":
			m.tab = (m.tab + 1) % 5
			if m.tab == tabLogs {
				return m, m.collectLogs()
			}
			return m, nil
		case "shift+tab", "[":
			m.tab = (m.tab + 4) % 5
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
	case approvalReqMsg:
		r := approvalReq(v)
		m.pending = &r
		return m, nil
	}
	return m, nil
}

// updateSend handles keys while the tx-send form has focus.
func (m *AppModel) updateSend(v tea.KeyMsg) (tea.Model, tea.Cmd) {
	fields := []*string{&m.txTo, &m.txAmt, &m.txGas, &m.txMemo}
	switch v.String() {
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "esc", "[":
		m.tab = tabOverview
		return m, nil
	case "]":
		m.tab = tabOverview
		return m, nil
	case "tab", "down":
		m.txField = (m.txField + 1) % len(fields)
	case "shift+tab", "up":
		m.txField = (m.txField + len(fields) - 1) % len(fields)
	case "enter":
		if m.txTo == "" || m.txAmt == "" {
			m.txErr = "to + amount required"
			return m, nil
		}
		m.txErr = ""
		m.toolBusy = "tx.send"
		return m, m.runSend()
	case "backspace":
		if s := *fields[m.txField]; len(s) > 0 {
			*fields[m.txField] = s[:len(s)-1]
		}
	default:
		if v.Type == tea.KeyRunes {
			*fields[m.txField] += string(v.Runes)
		}
	}
	return m, nil
}

func (m *AppModel) View() string {
	var b strings.Builder
	tabs := []string{"Overview", "Fleet", "Logs", "Tools", "Send"}
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
	case tabSend:
		b.WriteString(m.sendView())
	}
	if m.tab == tabTools {
		b.WriteString("\n" + m.toolList())
	}

	// approval modal — blocks the UI until y/n
	if m.pending != nil {
		var det strings.Builder
		for k, v := range m.pending.detail {
			fmt.Fprintf(&det, "  %s: %v\n", k, v)
		}
		fmt.Fprintf(&b, "\n%s\n%s\n%s\n%s\n",
			warn.Render(fmt.Sprintf("⚠ [%s] %s", m.pending.tier, m.pending.prompt)),
			det.String(),
			bad.Render("on-chain"+func() string {
				if m.pending.tier == toolkit.TierOnChain {
					return " — real funds"
				}
				return ""
			}()),
			headStyle.Render("approve? [y/n]"))
	}

	help := "1-5/tab/[ ]: panes · j/k scroll · r refresh · q quit"
	switch m.tab {
	case tabTools:
		help = "j/k select · enter run · [ ] panes · q quit"
	case tabSend:
		help = "tab fields · enter broadcast · esc/[ back"
	}
	fmt.Fprintf(&b, "\n%s\n", dim.Render(help))
	return b.String()
}

func (m *AppModel) sendView() string {
	var b strings.Builder
	fields := []struct{ label, val, hint string }{
		{"to", m.txTo, "recipient bech32/0x address"},
		{"amount", m.txAmt, "e.g. 1000000uatom"},
		{"gas price", m.txGas, "per-gas unit price"},
		{"memo", m.txMemo, "optional"},
	}
	for i, f := range fields {
		label := rowKey.Render(fmt.Sprintf("%-10s", f.label))
		val := f.val
		if i == m.txField {
			val = selStyle.Render(val + "█")
		} else if val == "" {
			val = dim.Render("(" + f.hint + ")")
		}
		fmt.Fprintf(&b, "%s %s\n", label, val)
	}
	if m.toolBusy == "tx.send" {
		fmt.Fprintf(&b, "\n%s\n", warn.Render("broadcasting…"))
	}
	if m.txErr != "" {
		fmt.Fprintf(&b, "\n%s\n", bad.Render(m.txErr))
	}
	b.WriteString(dim.Render("\nbroadcasts via build → simulate → approve → confirm\n"))
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
