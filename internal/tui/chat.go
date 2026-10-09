package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// chatPane is the conversational front-end: a transcript of user prompts,
// assistant text, tool calls, and approvals. The agent
// may be nil (no provider configured); slash commands and /run still work.
type chatPane struct {
	agent  *agent.Agent
	ta     textarea.Model
	blocks []string // rendered transcript blocks
	busy   bool
	cancel context.CancelFunc
	events chan tea.Msg // agent events → Update

	// streaming: index of the block being filled by deltas (-1 = none)
	live     int
	liveText string
	// thinking: index of the reasoning block being streamed (-1 = none)
	think     int
	thinkText string

	md  *glamour.TermRenderer
	mdW int
}

// agent events pumped into the bubbletea loop
type evAgent struct{ e agent.Event }
type evDone struct {
	text string
	err  error
}

// jobDoneMsg reports a slash command's background job (e.g. /compact).
type jobDoneMsg struct {
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

func newChatPane(c *toolkit.Context, reg *toolkit.Registry, appr *tuiApprover, setup ...AgentSetup) *chatPane {
	ta := textarea.New()
	ta.Placeholder = "ask about your node, or /help"
	ta.Prompt = "❯ "
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.Focus()
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline.SetEnabled(false)

	p := &chatPane{ta: ta, events: make(chan tea.Msg), live: -1, think: -1}
	// the agent's context uses the app-level approval bridge — tool prompts
	// surface as the in-app modal, never stdin (bubbletea owns it raw).
	sub := &toolkit.Context{
		Context: c.Context, Profile: c.Profile, Cfg: c.Cfg, Out: c.Out,
		Audit: c.Audit, Approver: appr.approve,
		AutoApproveBelow: toolkit.TierLocalChange,
	}
	p.blocks = append(p.blocks, dimSt.Render("cometcli — ask anything about your node, drive tools with /run, /help for commands"))
	if ag, err := agent.New(sub, reg); err != nil {
		p.blocks = append(p.blocks, dimSt.Render("no agent: "+err.Error()+" — /run <tool> {json-args} still works"))
	} else {
		ag.OnEvent = func(e agent.Event) { p.push(evAgent{e}) }
		p.agent = ag
		if !ag.HasModel() {
			p.append("info", "no agent provider — known questions are answered locally; for the rest set one: cometcli config set agent.provider groq")
		}
		var notes []string
		for _, fn := range setup {
			note, err := fn(ag)
			if err != nil {
				notes = append(notes, errStl.Render(err.Error()))
			} else if note != "" {
				notes = append(notes, dimSt.Render(note))
			}
		}
		p.blocks = append(p.blocks, dimSt.Render(fmt.Sprintf("%s/%s · %s · session %s", ag.Provider.Name(), ag.Model, ag.Policy, ag.ID())))
		p.blocks = append(p.blocks, notes...)
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
	return m.startTurn(s)
}

// startTurn runs one agent turn in the background; events stream back
// through p.events.
func (m *AppModel) startTurn(s string) tea.Cmd {
	p := m.chat
	if p.agent == nil {
		p.append("info", "no agent provider — /run <tool> {json} works; set one with: cometcli config set agent.provider groq")
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
		return add("info", "commands:\n"+agent.HelpText(p.agent)+`
  /run <tool> {"args"}          run a tool directly (e.g. /run node.logs {"lines":50})
  /exit                         quit
anything else is sent to the agent — "why is disk high", "unjail", "send 1uatom to …" all work`)
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
		if p.busy {
			return add("info", "a turn is running — esc to cancel it before changing session settings")
		}
		res, err := agent.RunCommand(p.agent, m.c, m.reg, s)
		switch {
		case errors.Is(err, agent.ErrUnknownCommand):
			return add("err", "unknown command: "+f[0]+" — /help")
		case err != nil:
			return add("err", err.Error())
		}
		if res.Text != "" {
			p.append("info", res.Text)
			m.syncChatView()
		}
		if res.Job != nil {
			p.busy = true
			job, ctx := res.Job, m.c.Context
			return func() tea.Msg {
				txt, err := job(ctx)
				return jobDoneMsg{txt, err}
			}
		}
		if res.Prompt != "" {
			p.append("user", res.Prompt)
			m.syncChatView()
			return m.startTurn(res.Prompt)
		}
		return nil
	}
}

// onAgentEvent renders one streamed agent event into the transcript.
func (m *AppModel) onAgentEvent(e agent.Event) {
	p := m.chat
	if e.Kind != agent.EvThinking {
		p.think, p.thinkText = -1, ""
	}
	switch e.Kind {
	case agent.EvThinking:
		p.thinkText += e.Text
		if p.think < 0 {
			p.blocks = append(p.blocks, "")
			p.think = len(p.blocks) - 1
		}
		t := strings.Join(strings.Fields(p.thinkText), " ")
		if len(t) > 240 { // show the tail: what it's thinking about now
			t = "…" + t[len(t)-240:]
			for len(t) > 3 && !utf8.RuneStart(t[3]) {
				t = "…" + t[4:]
			}
		}
		p.blocks[p.think] = dimSt.Render("∴ " + t)
	case agent.EvNotice:
		p.append("info", e.Text)
	case agent.EvTodos:
		p.append("info", agent.RenderTodos(e.Todos))
	case agent.EvDelta:
		p.liveText += e.Text
		if p.live < 0 {
			p.blocks = append(p.blocks, "")
			p.live = len(p.blocks) - 1
		}
		p.blocks[p.live] = p.liveText
	case agent.EvText:
		final := renderBlock("agent", e.Text, p.mdW, &p.md)
		if p.live >= 0 {
			p.blocks[p.live] = final
		} else {
			p.blocks = append(p.blocks, final)
		}
		p.live, p.liveText = -1, ""
	case agent.EvToolStart:
		p.append("call", fmt.Sprintf("%s %s %s", e.Tool, dimSt.Render("["+e.Tier+"]"), agent.CompactArgs(e.Args)))
	case agent.EvToolResult:
		if e.Err != "" {
			p.append("err", e.Err)
		} else {
			p.append("ok", e.Text)
		}
	}
	m.syncChatView()
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

func compactArgs(args map[string]any) string { return agent.CompactArgs(args) }
