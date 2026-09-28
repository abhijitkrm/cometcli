package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// chatPane is the conversational front-end: a transcript of user prompts,
// assistant text, tool calls, and approvals — Claude Code style. The agent
// may be nil (no provider configured); slash commands and /run still work.
type chatPane struct {
	agent  *agent.Agent
	ta     textarea.Model
	blocks []string // rendered transcript blocks
	busy   bool
	cancel context.CancelFunc
	events chan tea.Msg // agent callbacks → Update

	md  *glamour.TermRenderer
	mdW int
}

// agent events pumped into the bubbletea loop
type evText struct{ s string }
type evToolCall struct {
	name string
	args map[string]any
}
type evToolRes struct {
	name, summary string
	err           error
}
type evDone struct {
	text string
	err  error
}
type runResMsg struct {
	name, text string
	err        error
}

var (
	userSt    = lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
	dimSt     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	callStl   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStl    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	approveSt = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
)

func newChatPane(c *toolkit.Context, reg *toolkit.Registry, appr *tuiApprover) *chatPane {
	ta := textarea.New()
	ta.Placeholder = "ask about your node, or /help"
	ta.Prompt = "❯ "
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.Focus()
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline.SetEnabled(false)

	p := &chatPane{ta: ta, events: make(chan tea.Msg)}
	// the agent's context uses the app-level approval bridge — tool prompts
	// surface as the in-app modal, never stdin (bubbletea owns it raw).
	sub := &toolkit.Context{
		Context: c.Context, Profile: c.Profile, Cfg: c.Cfg, Out: c.Out,
		Audit: c.Audit, Approver: appr.approve,
		AutoApproveBelow: toolkit.TierLocalChange,
	}
	p.blocks = append(p.blocks, dimSt.Render("cometcli — ask anything about your node, drive tools with /run, /help for commands"))
	if ag, err := agent.New(sub, reg); err != nil {
		p.blocks = append(p.blocks, dimSt.Render("agent.provider not configured — /run <tool> {json-args} works without one"))
	} else {
		ag.OnText = func(t string) { p.push(evText{t}) }
		ag.OnToolCall = func(n string, a map[string]any) { p.push(evToolCall{n, a}) }
		ag.OnToolResult = func(n, s string, e error) { p.push(evToolRes{n, s, e}) }
		p.agent = ag
	}
	return p
}

func (p *chatPane) push(m tea.Msg) { p.events <- m }

func (p *chatPane) append(kind, text string) {
	p.blocks = append(p.blocks, renderBlock(kind, text, p.mdW, &p.md))
}

func (p *chatPane) transcript() string { return strings.Join(p.blocks, "\n") }

func renderBlock(kind, text string, width int, md **glamour.TermRenderer) string {
	switch kind {
	case "user":
		return userSt.Render("❯ " + text)
	case "agent":
		if width > 10 {
			if *md == nil || width != 0 {
				r, err := glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(width))
				if err == nil {
					*md = r
				}
			}
			if *md != nil {
				if out, err := (*md).Render(text); err == nil {
					return strings.TrimRight(out, "\n")
				}
			}
		}
		return text
	case "call":
		return dimSt.Render("◐ ") + callStl.Render(text)
	case "ok":
		return dimSt.Render("  ✓ ") + dimSt.Render(text)
	case "err":
		return dimSt.Render("  ✗ ") + errStl.Render(text)
	case "approval":
		return approveSt.Render("⚠ " + text)
	default: // info
		return dimSt.Render(text)
	}
}

// submit routes the input line: slash commands, agent turn, or /run.
func (m *AppModel) chatSubmit(s string) tea.Cmd {
	p := m.chat
	if strings.HasPrefix(s, "/") {
		return m.chatSlash(s)
	}
	p.append("user", s)
	m.syncChatView()
	if p.agent == nil {
		p.append("info", "no agent provider — /run <tool> {json} works, or set agent.provider in the profile")
		m.syncChatView()
		return nil
	}
	if p.busy {
		p.append("info", "working… (esc to cancel)")
		m.syncChatView()
		return nil
	}
	p.busy = true
	ctx, cancel := context.WithCancel(m.c.Context)
	p.cancel = cancel
	ag := p.agent
	return func() tea.Msg {
		defer cancel()
		out, err := ag.Run(ctx, s)
		return evDone{out, err}
	}
}

func (m *AppModel) chatSlash(s string) tea.Cmd {
	p := m.chat
	f := strings.Fields(s)
	add := func(kind, txt string) tea.Cmd {
		p.append(kind, txt)
		m.syncChatView()
		return nil
	}
	switch f[0] {
	case "/exit", "/quit", "/q":
		m.quitting = true
		return tea.Quit
	case "/help":
		return add("info", `commands:
  /run <tool> {"args"}   run a tool directly (e.g. /run node.logs {"lines":50})
  /tools                 list the registry
  /safe                  toggle read-only agent mode
  /mode                  approval posture
  /profile               active profile
  /audit                 audit log path
  /reset                 clear agent memory + transcript
  /exit                  quit
anything else is sent to the agent — "send 1uatom to cosmos1…", "why is disk high", "unjail" all work`)
	case "/tools":
		var b strings.Builder
		for _, t := range m.reg.All() {
			fmt.Fprintf(&b, "%-24s [%s] %s\n", t.Name(), t.Tier(), t.Desc())
		}
		return add("info", strings.TrimRight(b.String(), "\n"))
	case "/safe":
		if p.agent == nil {
			return add("err", "no agent configured")
		}
		p.agent.Safe = !p.agent.Safe
		return add("info", fmt.Sprintf("safe mode %v — mutating tools refused", p.agent.Safe))
	case "/mode":
		return add("info", "approvals: observe/diagnose auto · local-change prompts · on-chain always prompts (in-app modal)")
	case "/profile":
		pr := m.c.Profile
		return add("info", fmt.Sprintf("%s (%s, chain %s, role %s)", pr.Name, pr.Transport.Type, pr.ChainID, pr.Role))
	case "/audit":
		if m.c.Audit != nil {
			return add("info", m.c.Audit.Path())
		}
		return add("info", "audit disabled")
	case "/reset":
		p.blocks = nil
		if p.agent != nil {
			p.agent.Reset()
		}
		return add("info", "cleared")
	case "/run":
		if len(f) < 2 {
			return add("err", "usage: /run <tool> {json-args}")
		}
		args := toolkit.Args{}
		if i := strings.Index(s, "{"); i >= 0 {
			if err := json.Unmarshal([]byte(s[i:]), &args); err != nil {
				return add("err", "bad args json: "+err.Error())
			}
		}
		name := f[1]
		t, ok := m.reg.Get(name)
		if !ok {
			return add("err", "no such tool: "+name)
		}
		p.append("user", s)
		p.append("call", name+" "+compactArgs(map[string]any(args)))
		m.syncChatView()
		m.toolBusy = name
		return func() tea.Msg {
			res, err := t.Run(m.subCtx(), args)
			if err != nil {
				return runResMsg{name: name, err: err}
			}
			txt := res.Text
			if txt == "" && res.Data != nil {
				txt = res.JSON()
			}
			return runResMsg{name: name, text: txt}
		}
	default:
		return add("err", "unknown command: "+f[0]+" — /help")
	}
}

// syncChatView pushes the transcript into the viewport and scrolls down.
func (m *AppModel) syncChatView() {
	m.vp.SetContent(m.chat.transcript())
	m.vp.GotoBottom()
}

// waitEvent parks until the agent goroutine emits the next UI event.
func (m *AppModel) waitEvent() tea.Cmd {
	return func() tea.Msg { return <-m.chat.events }
}

// chatKey handles keys while the chat pane is focused. Returns
// (model, cmd, handled) — unhandled keys fall through to global bindings.
func (m *AppModel) chatKey(v tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.chat
	switch v.String() {
	case "enter":
		s := strings.TrimSpace(p.ta.Value())
		p.ta.Reset()
		if s == "" {
			return m, nil
		}
		return m, m.chatSubmit(s)
	case "esc":
		if p.busy && p.cancel != nil {
			p.cancel()
			p.busy = false
			p.append("info", "run cancelled")
			m.syncChatView()
		}
		return m, nil
	}
	var cmd tea.Cmd
	p.ta, cmd = p.ta.Update(v)
	return m, cmd
}

func compactArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	var parts []string
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	s := strings.Join(parts, " ")
	if len(s) > 100 {
		return s[:100] + "…"
	}
	return s
}
