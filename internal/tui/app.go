package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/monitor"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

type tab int

const (
	tabChat tab = iota
	tabOverview
	tabFleet
	tabLogs
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

// AppModel is the multi-pane `cometcli ui` application — chat-first, with
// read-only dashboards behind it.
type AppModel struct {
	c        *toolkit.Context
	reg      *toolkit.Registry
	interval time.Duration

	tab      tab
	chat     *chatPane
	snap     *monitor.Snapshot
	fleet    map[string]*monitor.Snapshot
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
	initial  string // prompt submitted on start

	vp       viewport.Model
	width    int
	height   int
	quitting bool
}

// AgentSetup adjusts the chat agent after it is built (CLI flags,
// session resume). The returned text, if any, is shown in the transcript.
type AgentSetup func(*agent.Agent) (string, error)

// NewApp creates the app model.
func NewApp(c *toolkit.Context, reg *toolkit.Registry, interval time.Duration, setup ...AgentSetup) *AppModel {
	vp := viewport.New(80, 20)
	appr := &tuiApprover{req: make(chan approvalReq)}
	return &AppModel{
		c: c, reg: reg, interval: interval, vp: vp,
		chat:     newChatPane(c, reg, appr, setup...),
		approver: appr, txGas: "1e9",
	}
}

// waitApproval parks on the approver channel — emits approvalReqMsg when a
// tool run inside the UI needs a human decision.
func (m *AppModel) waitApproval() tea.Cmd {
	return func() tea.Msg { return approvalReqMsg(<-m.approver.req) }
}

func (m *AppModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.collectSnap(), m.collectFleet(), tick(m.interval), m.waitApproval(), m.waitEvent()}
	if s := m.initial; s != "" {
		m.initial = ""
		m.chat.append("user", s)
		cmds = append(cmds, m.startTurn(s))
	}
	return tea.Batch(cmds...)
}

func (m *AppModel) collectSnap() tea.Cmd {
	if m.c.Profile == nil {
		return nil // general mode: no node to watch
	}
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
		if m.tab == tabChat {
			m.vp.Height = v.Height - 9 // tabs + status + input + help
		} else {
			m.vp.Height = v.Height - 6
		}
		m.chat.ta.SetWidth(v.Width - 4)
		m.chat.mdW = min(v.Width-8, 110)
		m.chat.md = nil // re-render lazily at the new width
		return m, nil
	case tea.KeyMsg:
		// pending approval modal captures y/n above everything
		if m.pending != nil {
			ok := false
			switch v.String() {
			case "y", "Y":
				ok = true
			}
			m.pending.resp <- ok
			m.chat.append("approval", fmt.Sprintf("%s: %s",
				map[bool]string{true: "approved", false: "denied"}[ok], m.pending.prompt))
			m.syncChatView()
			m.pending = nil
			return m, m.waitApproval()
		}
		if m.tab == tabSend {
			return m.updateSend(v)
		}
		if m.tab == tabChat {
			// on chat, printable keys belong to the textarea; only pane
			// switching and quit are intercepted
			switch v.String() {
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			case "tab":
				m.tab = tabOverview
				return m, nil
			case "shift+tab":
				m.tab = tabSend
				return m, nil
			case "pgdown":
				m.vp.HalfPageDown()
				return m, nil
			case "pgup":
				m.vp.HalfPageUp()
				return m, nil
			}
			return m.chatKey(v)
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
			m.vp.ScrollDown(1)
		case "k", "up":
			m.vp.ScrollUp(1)
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
			m.chat.append("err", fmt.Sprintf("%s: %v", v.name, v.err))
		} else {
			m.chat.append("ok", fmt.Sprintf("%s → %s", v.name, firstLine(v.text)))
		}
		m.syncChatView()
	case runResMsg:
		m.toolBusy = ""
		if v.err != nil {
			m.chat.append("err", fmt.Sprintf("%s: %v", v.name, v.err))
		} else {
			m.chat.append("ok", v.name)
			m.chat.append("info", v.text)
		}
		m.syncChatView()
	case jobDoneMsg:
		m.chat.busy = false
		if v.err != nil {
			m.chat.append("err", v.err.Error())
		} else if v.text != "" {
			m.chat.append("info", v.text)
		}
		m.syncChatView()
	case evAgent:
		m.onAgentEvent(v.e)
		return m, m.waitEvent()
	case evDone:
		m.chat.busy = false
		m.chat.live, m.chat.liveText = -1, ""
		if v.err != nil {
			m.chat.append("err", "agent: "+v.err.Error())
			m.syncChatView()
		}
		// text already streamed through evAgent
		return m, m.waitEvent()
	case tickMsg:
		return m, tea.Batch(m.collectSnap(), m.collectFleet(), tick(m.interval))
	case approvalReqMsg:
		r := approvalReq(v)
		m.pending = &r
		m.chat.append("approval", "requested: "+r.prompt)
		m.syncChatView()
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
	tabs := []string{"Chat", "Overview", "Fleet", "Logs", "Send"}
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
	scope := "general"
	if m.chat.agent != nil && m.chat.agent.Node() {
		scope = m.chat.agent.Ctx.Profile.Name
	} else if m.chat.agent == nil && m.c.Profile != nil {
		scope = m.c.Profile.Name
	}
	fmt.Fprintf(&b, "  %s  %s\n", headStyle.Render(scope), dim.Render(clock))

	if m.tab == tabChat {
		b.WriteString(m.statusLine() + "\n")
		b.WriteString(m.vp.View())
		b.WriteString("\n" + m.chat.ta.View())
	} else {
		b.WriteString("\n")
		switch m.tab {
		case tabOverview:
			b.WriteString(m.overviewView())
		case tabFleet:
			b.WriteString(m.fleetView())
		case tabLogs:
			b.WriteString(m.vp.View())
		case tabSend:
			b.WriteString(m.sendView())
		}
	}

	// approval modal — blocks the UI until y/n
	if m.pending != nil {
		var det strings.Builder
		keys := make([]string, 0, len(m.pending.detail))
		for k := range m.pending.detail {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := fmt.Sprint(m.pending.detail[k])
			if k == "diff" {
				det.WriteString(renderDiff(v))
				continue
			}
			if k == "doc" && strings.Contains(m.pending.prompt, v) {
				continue // the tx doc is already part of the prompt
			}
			fmt.Fprintf(&det, "  %s: %s\n", k, v)
		}
		risk := ""
		if m.pending.tier == toolkit.TierOnChain {
			risk = bad.Render("on-chain — signs and broadcasts a real transaction") + "\n"
		}
		fmt.Fprintf(&b, "\n%s\n%s%s%s\n",
			warn.Render(fmt.Sprintf("⚠ [%s] %s", m.pending.tier, m.pending.prompt)),
			det.String(), risk,
			headStyle.Render("approve? [y/n]"))
	}

	help := "1-5/tab/[ ]: panes · j/k scroll · r refresh · q quit"
	switch m.tab {
	case tabChat:
		help = "enter send · esc cancel run · pgup/pgdn scroll · tab panes · ctrl+c quit · /help"
	case tabSend:
		help = "tab fields · enter broadcast · esc/[ back"
	}
	fmt.Fprintf(&b, "\n%s\n", dim.Render(help))
	return b.String()
}

// renderDiff colors a unified-style diff for the approval modal.
func renderDiff(d string) string {
	var b strings.Builder
	for _, ln := range strings.Split(strings.TrimRight(d, "\n"), "\n") {
		switch {
		case strings.HasPrefix(ln, "+"):
			b.WriteString("  " + ok.Render(ln) + "\n")
		case strings.HasPrefix(ln, "-"):
			b.WriteString("  " + bad.Render(ln) + "\n")
		default:
			b.WriteString("  " + dim.Render(ln) + "\n")
		}
	}
	return b.String()
}

// statusLine is the chat-pane header: provider, mode, live height.
func (m *AppModel) statusLine() string {
	prov := "no agent"
	if m.chat.agent != nil && !m.chat.agent.HasModel() {
		prov = "no agent model · local answers only"
	} else if m.chat.agent != nil {
		prov = fmt.Sprintf("%s/%s", m.chat.agent.Provider.Name(), m.chat.agent.Model)
		if pol := m.chat.agent.Policy; pol.ReadOnly() {
			prov += " [readonly]"
		} else if pol.AutoLocal {
			prov += " [autopilot: local-change]"
		}
	}
	live := "offline"
	if m.snap != nil && m.snap.Reachable {
		live = fmt.Sprintf("h=%d peers=%d", m.snap.Height, m.snap.Peers)
	}
	busy := ""
	if m.chat.busy {
		busy = " · " + warn.Render("working… esc to cancel")
	}
	return "  " + dim.Render(fmt.Sprintf("%s · %s", prov, live)) + busy
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		n := 120
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n] + "…"
	}
	return s
}

// RunApp starts the multi-pane TUI.
func RunApp(c *toolkit.Context, reg *toolkit.Registry, interval time.Duration, setup ...AgentSetup) error {
	return RunAppWith(c, reg, interval, "", setup...)
}

// RunAppWith starts the TUI and submits prompt (if any) right away.
func RunAppWith(c *toolkit.Context, reg *toolkit.Registry, interval time.Duration, prompt string, setup ...AgentSetup) error {
	m := NewApp(c, reg, interval, setup...)
	m.initial = prompt
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	if m.chat.agent != nil {
		m.chat.agent.Close()
	}
	return err
}
