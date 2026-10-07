// Package chatui is cometcli's interactive terminal: an inline,
// scrollback-friendly chat in the style of Claude Code. Finished messages
// are printed into the terminal's normal scrollback; only a small live
// region (streaming text, spinner, input, approval dialog, status line)
// is redrawn.
package chatui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	reflowansi "github.com/muesli/reflow/ansi"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Options configure a chat session.
type Options struct {
	Agent  *agent.Agent
	Prompt string   // submitted on start
	Notes  []string // shown under the banner (e.g. "resumed session …")
}

// Run starts the interactive chat and blocks until the operator exits.
func Run(o Options) error {
	m := newModel(o)
	p := tea.NewProgram(m)
	m.send = p.Send
	o.Agent.OnEvent = func(e agent.Event) { p.Send(eventMsg{e}) }
	o.Agent.Ctx.Approver = m.approve
	o.Agent.Ctx.Chooser = m.choose
	o.Agent.Ctx.Secret = m.secret
	_, err := p.Run()
	if m.a.Persist && len(m.a.History()) > 0 {
		fmt.Printf("\n%s\n", dim.Render("resume this session with: cometcli -r "+m.a.ID()))
	}
	return err
}

// --- messages --------------------------------------------------------------

type eventMsg struct{ e agent.Event }
type doneMsg struct {
	text string
	err  error
}
type jobDoneMsg struct {
	text string
	err  error
}
type execDoneMsg struct {
	cmd  string
	err  error
	code int
}
type approvalMsg struct{ p *pendingApproval }
type choiceMsg struct{ p *pendingChoice }
type secretMsg struct{ p *pendingSecret }

type pendingChoice struct {
	prompt  string
	options []string
	sel     int
	resp    chan choiceResult
}

type choiceResult struct {
	i   int
	err error
}

type pendingSecret struct {
	prompt string
	input  textinput.Model
	resp   chan secretResult
}

type secretResult struct {
	s   string
	err error
}

type approvalResult struct {
	ok  bool
	err error
}

type pendingApproval struct {
	prompt string
	tier   toolkit.Tier
	detail map[string]any
	resp   chan approvalResult
	sel    int
}

// --- styles ----------------------------------------------------------------

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

var (
	accent   = lipgloss.Color("#D97757")
	dim      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	faint    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	bold     = lipgloss.NewStyle().Bold(true)
	errSt    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	okSt     = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	warnSt   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	accentSt = lipgloss.NewStyle().Foreground(accent)
	userSt   = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("236"))
	addSt    = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	delSt    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	thinkSt  = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)
)

// --- model -----------------------------------------------------------------

type suggestion struct {
	insert string // replaces the current token
	label  string
	desc   string
}

type model struct {
	a    *agent.Agent
	send func(tea.Msg)

	ta    textarea.Model
	sp    spinner.Model
	width int

	running bool
	cancel  context.CancelFunc
	started time.Time
	live    strings.Builder
	think   string

	history []string
	histIdx int
	draft   string

	sugg    []suggestion
	suggIdx int
	cmds    []agent.CmdInfo
	files   []string

	approval  *pendingApproval
	choice    *pendingChoice
	secretReq *pendingSecret
	quitArmed time.Time
	hint      string // transient message in the footer
	baseMode  agent.Mode

	md      *glamour.TermRenderer
	mdWidth int
	mdStyle string

	initial  string
	notes    []string
	progress string   // latest live status from a long-running tool
	printed  []string // everything printed, for tests

	pending    []agent.Event // tool calls started, not finished (shown live)
	thinkStart time.Time     // when the current thinking began
	thoughts   []string      // finished thinking, newest last (ctrl+o)
	lastFull   string        // the last collapsed tool output (ctrl+r)
	verb       string        // this turn's spinner verb
	steering   bool          // a message typed mid-turn is waiting
	started2   bool          // banner printed
}

func newModel(o Options) *model {
	ta := textarea.New()
	ta.Placeholder = `Try "check my validator's last 100 log lines for errors"`
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(1)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Placeholder = faint
	ta.KeyMap.InsertNewline.SetEnabled(false) // enter submits; ctrl+j / \⏎ add lines
	ta.Focus()
	sp := spinner.New()
	sp.Spinner = spinner.Spinner{Frames: []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}, FPS: 120 * time.Millisecond}
	sp.Style = accentSt
	// no terminal background query (its reply can leak into the input):
	// dark unless COLORFGBG says the background is light
	style := "dark"
	if fgbg := os.Getenv("COLORFGBG"); fgbg != "" {
		if bg := fgbg[strings.LastIndex(fgbg, ";")+1:]; bg == "7" || bg == "15" {
			style = "light"
		}
	}
	return &model{
		a: o.Agent, ta: ta, sp: sp, width: 100, initial: o.Prompt, notes: o.Notes,
		cmds: agent.Commands(o.Agent), histIdx: -1, baseMode: o.Agent.Policy.Mode, mdStyle: style,
	}
}

func (m *model) Init() tea.Cmd {
	// the banner waits for the first window size so it fits the terminal
	// (startBanner runs it anyway if the terminal never reports one)
	return tea.Batch(textarea.Blink, tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return startMsg{} }))
}

type startMsg struct{}

// start prints the banner and submits the initial prompt, once.
func (m *model) start() tea.Cmd {
	if m.started2 {
		return nil
	}
	m.started2 = true
	cmds := []tea.Cmd{m.print(m.banner())}
	if m.initial != "" {
		cmds = append(cmds, m.submit(m.initial))
		m.initial = ""
	}
	return tea.Sequence(cmds...)
}

// print commits lines to the scrollback above the live region, wrapped
// to the terminal: a line the terminal wraps itself throws off the
// redraw of the live region below it.
func (m *model) print(lines ...string) tea.Cmd {
	s := strings.Join(lines, "\n")
	if s == "" {
		return nil
	}
	s = fitWidth(s, max(20, m.width-1))
	m.printed = append(m.printed, s)
	return tea.Println(s)
}

// fitWidth wraps each over-long line at w columns (ANSI-aware), keeping
// its indentation as a hanging indent ("  ⎿  " results stay aligned).
func fitWidth(s string, w int) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, l := range lines {
		if reflowansi.PrintableRuneWidth(l) <= w {
			out = append(out, l)
			continue
		}
		plain := ansiRe.ReplaceAllString(l, "")
		indent := len(plain) - len(strings.TrimLeft(plain, " "))
		if strings.HasPrefix(plain, "  ⎿  ") {
			indent = 5
		}
		if indent > w/2 {
			indent = 0
		}
		ww := wordwrap.NewWriter(w - indent)
		ww.Breakpoints = nil // break at spaces only: paths and hashes stay whole until hard-wrapped
		_, _ = ww.Write([]byte(l))
		_ = ww.Close()
		parts := strings.Split(wrap.String(ww.String(), w-indent), "\n")
		for i, p := range parts {
			if i > 0 {
				p = strings.Repeat(" ", indent) + strings.TrimLeft(p, " ")
			}
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		if v.Width <= 0 {
			return m, nil // some ptys report 0×0
		}
		m.width = v.Width
		m.ta.SetWidth(max(20, v.Width-6))
		return m, m.start()
	case startMsg:
		return m, m.start()
	case spinner.TickMsg:
		if !m.running {
			return m, nil
		}
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(v)
		return m, cmd
	case eventMsg:
		return m, m.onEvent(v.e)
	case approvalMsg:
		m.approval = v.p
		return m, nil
	case choiceMsg:
		m.choice = v.p
		return m, nil
	case secretMsg:
		m.secretReq = v.p
		return m, textinput.Blink
	case doneMsg:
		return m, m.onDone(v)
	case jobDoneMsg:
		if v.err != nil {
			return m, m.print(result(errSt.Render(v.err.Error())))
		}
		return m, m.print(result(v.text))
	case execDoneMsg:
		return m, m.onExecDone(v)
	case tea.KeyMsg:
		switch {
		case m.secretReq != nil:
			return m, m.secretKey(v)
		case m.choice != nil:
			return m, m.choiceKey(v)
		}
		if m.approval != nil {
			return m, m.approvalKey(v)
		}
		return m, m.key(v)
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

// --- keys ------------------------------------------------------------------

func (m *model) key(k tea.KeyMsg) tea.Cmd {
	if k.String() != "ctrl+c" {
		m.quitArmed = time.Time{}
	}
	m.hint = ""
	switch k.String() {
	case "ctrl+c":
		switch {
		case m.running:
			m.interrupt()
		case m.ta.Value() != "":
			m.ta.Reset()
			m.resize()
		case time.Since(m.quitArmed) < 2*time.Second:
			return tea.Quit
		default:
			m.quitArmed = time.Now()
			m.hint = "press ctrl+c again to exit"
		}
		return nil
	case "ctrl+d":
		if m.ta.Value() == "" {
			return tea.Quit
		}
	case "esc":
		switch {
		case len(m.sugg) > 0:
			m.sugg = nil
		case m.running:
			m.interrupt()
		}
		return nil
	case "shift+tab":
		m.cycleMode()
		return nil
	case "ctrl+r":
		if m.lastFull == "" {
			m.hint = "nothing collapsed to expand"
			return nil
		}
		full := m.lastFull
		m.lastFull = ""
		return m.print(result(full))
	case "ctrl+o":
		if len(m.thoughts) == 0 {
			m.hint = "no thinking to show"
			return nil
		}
		t := m.thoughts[len(m.thoughts)-1]
		return m.print("", thinkSt.Render("∴ Thinking…"), result(thinkSt.Render(lipgloss.NewStyle().Width(min(m.width-8, 110)).Render(t))))
	case "tab":
		if len(m.sugg) > 0 {
			m.acceptSuggestion()
			return nil
		}
	case "up", "down":
		if len(m.sugg) > 0 {
			d := 1
			if k.String() == "up" {
				d = -1
			}
			m.suggIdx = (m.suggIdx + d + len(m.sugg)) % len(m.sugg)
			return nil
		}
		if !strings.Contains(m.ta.Value(), "\n") {
			m.historyStep(k.String() == "up")
			return nil
		}
	case "ctrl+j", "alt+enter":
		m.ta.InsertString("\n")
		m.resize()
		return nil
	case "enter":
		if len(m.sugg) > 0 && m.sugg[m.suggIdx].insert != m.currentToken() {
			m.acceptSuggestion()
			if !strings.HasPrefix(m.ta.Value(), "/") || strings.Contains(m.ta.Value(), "@") {
				return nil
			}
		}
		v := m.ta.Value()
		if strings.HasSuffix(v, "\\") {
			m.ta.SetValue(strings.TrimSuffix(v, "\\") + "\n")
			m.resize()
			return nil
		}
		m.ta.Reset()
		m.sugg = nil
		m.resize()
		return m.submit(v)
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(k)
	m.resize()
	m.suggest()
	return cmd
}

func (m *model) resize() {
	n := strings.Count(m.ta.Value(), "\n") + 1
	m.ta.SetHeight(min(max(n, 1), 8))
}

func (m *model) historyStep(up bool) {
	if len(m.history) == 0 {
		return
	}
	if m.histIdx == -1 {
		m.draft = m.ta.Value()
	}
	switch {
	case up && m.histIdx < len(m.history)-1:
		m.histIdx++
	case !up && m.histIdx > -1:
		m.histIdx--
	}
	if m.histIdx == -1 {
		m.ta.SetValue(m.draft)
	} else {
		m.ta.SetValue(m.history[len(m.history)-1-m.histIdx])
	}
	m.ta.CursorEnd()
	m.resize()
}

func (m *model) cycleMode() {
	order := []agent.Mode{agent.ModeOps, agent.ModeAcceptEdits, agent.ModeReadOnly}
	if m.baseMode == agent.ModeBypass {
		order = append(order, agent.ModeBypass)
	}
	cur := m.a.Policy.Mode
	if cur == "" {
		cur = agent.ModeOps
	}
	next := order[0]
	for i, md := range order {
		if md == cur {
			next = order[(i+1)%len(order)]
		}
	}
	m.a.Policy.Mode = next
}

func (m *model) interrupt() {
	if m.cancel != nil {
		m.cancel()
	}
}

// --- submit ----------------------------------------------------------------

func (m *model) submit(raw string) tea.Cmd {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil
	}
	if len(m.history) == 0 || m.history[len(m.history)-1] != text {
		m.history = append(m.history, text)
	}
	m.histIdx = -1
	switch {
	case strings.HasPrefix(text, "!"):
		return m.runShell(strings.TrimSpace(text[1:]))
	case strings.HasPrefix(text, "#"):
		return m.command("/remember " + strings.TrimSpace(text[1:]))
	case strings.HasPrefix(text, "/"):
		return m.command(text)
	}
	if m.running {
		// steer: delivered at the agent's next step, not after the turn
		m.a.Steer(m.a.ExpandMentions(text))
		m.steering = true
		return m.print(userLine(text), result(dim.Render("cometcli will see this after the current step · ")+bold.Render("esc")+dim.Render(" to interrupt and send it now")))
	}
	m.ta.Placeholder = "" // the example is for an empty session only
	return tea.Sequence(m.print(userLine(text)), m.startTurn(m.a.ExpandMentions(text)))
}

func (m *model) startTurn(prompt string) tea.Cmd {
	ctx, cancel := context.WithCancel(m.a.Ctx.Context)
	m.cancel, m.running, m.started = cancel, true, time.Now()
	m.live.Reset()
	m.think, m.pending = "", nil
	m.verb = verbs[int(time.Now().UnixNano()/1e6)%len(verbs)]
	a := m.a
	return tea.Batch(m.sp.Tick, func() tea.Msg {
		defer cancel()
		out, err := a.Run(ctx, prompt)
		return doneMsg{out, err}
	})
}

func (m *model) command(line string) tea.Cmd {
	f := strings.Fields(line)
	switch f[0] {
	case "/exit", "/quit", "/q":
		return tea.Quit
	}
	if m.running && f[0] != "/cost" && f[0] != "/help" && f[0] != "/permissions" && f[0] != "/mcp" {
		return m.print(userLine(line), result(dim.Render("a turn is running — esc to interrupt it first")))
	}
	res, err := agent.RunCommand(m.a, m.a.Ctx, m.a.Reg, line)
	echo := userLine(line)
	switch {
	case errors.Is(err, agent.ErrUnknownCommand):
		return m.print(echo, result(errSt.Render("unknown command "+f[0]+" — /help lists them")))
	case err != nil:
		return m.print(echo, result(errSt.Render(err.Error())))
	}
	if f[0] == "/one" || f[0] == "/reset" || f[0] == "/clear" || f[0] == "/resume" {
		m.cmds = agent.Commands(m.a)
		m.files = nil
	}
	cmds := []tea.Cmd{m.print(echo, result(res.Text))}
	if res.Job != nil {
		job, ctx := res.Job, m.a.Ctx.Context
		cmds = append(cmds, func() tea.Msg {
			txt, err := job(ctx)
			return jobDoneMsg{txt, err}
		})
	}
	if res.Prompt != "" {
		cmds = append(cmds, m.startTurn(res.Prompt))
	}
	return tea.Sequence(cmds...)
}

// runShell runs a command in the operator's own terminal (the UI is
// suspended), so interactive scripts work. Its output is never captured;
// the model is told the command and its exit code.
func (m *model) runShell(cmdline string) tea.Cmd {
	if cmdline == "" {
		return m.print(result(dim.Render("! runs a command in your terminal, e.g. !./upgrade/vote-upgrade.sh")))
	}
	if m.running {
		return m.print(result(dim.Render("a turn is running — esc to interrupt it first")))
	}
	c := m.shellCmd(cmdline)
	return tea.Sequence(
		m.print(accentSt.Render("!")+" "+bold.Render(cmdline)),
		tea.ExecProcess(c, func(err error) tea.Msg {
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code, err = ee.ExitCode(), nil
			}
			return execDoneMsg{cmd: cmdline, err: err, code: code}
		}),
	)
}

// shellCmd builds the command for "!": local, or on the node over ssh -t
// in node mode with an SSH transport.
func (m *model) shellCmd(cmdline string) *exec.Cmd {
	cwd := m.a.Tools.Cwd(m.a.WorkRoot)
	if m.a.Node() && m.a.Ctx.Profile.Transport.Type == "ssh" {
		t := m.a.Ctx.Profile.Transport
		args := []string{"-t"}
		if t.Port != 0 {
			args = append(args, "-p", strconv.Itoa(t.Port))
		}
		if t.KeyFile != "" {
			args = append(args, "-i", t.KeyFile)
		}
		target := t.Host
		if t.User != "" {
			target = t.User + "@" + t.Host
		}
		remote := cmdline
		if rc := m.a.Tools.Cwd(""); rc != "" {
			remote = "cd '" + strings.ReplaceAll(rc, "'", `'\''`) + "' && " + cmdline
		}
		return exec.Command("ssh", append(args, target, remote)...)
	}
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	c := exec.Command(sh, "-c", cmdline)
	c.Dir = cwd
	return c
}

func (m *model) onExecDone(v execDoneMsg) tea.Cmd {
	status := okSt.Render("done")
	switch {
	case v.err != nil:
		status = errSt.Render(v.err.Error())
	case v.code != 0:
		status = warnSt.Render(fmt.Sprintf("exit %d", v.code))
	}
	where := "their terminal"
	if m.a.Node() && m.a.Ctx.Profile.Transport.Type == "ssh" {
		where = m.a.Ctx.Profile.Name + " over ssh"
	}
	m.a.AddNote(fmt.Sprintf("[The operator ran `%s` themselves in %s (exit %d). Its output was not captured — ask them if you need it.]",
		v.cmd, where, v.code))
	return m.print(result(status + dim.Render(" · the agent will know you ran this")))
}

// --- agent events ----------------------------------------------------------

func (m *model) onEvent(e agent.Event) tea.Cmd {
	switch e.Kind {
	case agent.EvThinking:
		if m.think == "" {
			m.thinkStart = time.Now()
		}
		m.think += e.Text
		return nil
	case agent.EvDelta:
		cmd := m.flushThinking()
		m.live.WriteString(e.Text)
		return cmd
	case agent.EvText:
		cmd := m.flushThinking()
		m.live.Reset()
		return tea.Sequence(cmd, m.print("", "⏺ "+strings.TrimLeft(m.markdown(e.Text), "\n ")))
	case agent.EvToolStart:
		cmd := m.flushThinking()
		m.pending = append(m.pending, e)
		return cmd
	case agent.EvToolResult:
		m.progress = ""
		start := e
		for i, p := range m.pending { // the oldest running call of this tool
			if p.Tool == e.Tool {
				start = p
				m.pending = append(m.pending[:i:i], m.pending[i+1:]...)
				break
			}
		}
		bullet := okSt.Render("⏺")
		if e.Err != "" {
			bullet = errSt.Render("⏺")
		}
		return m.print("", bullet+" "+toolHeader(start), result(m.toolBody(start, e)))
	case agent.EvNotice:
		if e.Text == "your message reached the agent" {
			m.steering = false
		}
		return m.print(result(dim.Render(e.Text)))
	case agent.EvProgress:
		m.progress = e.Text
		return nil
	case agent.EvTodos:
		return m.print("", okSt.Render("⏺")+" "+bold.Render("Update Todos"), result(todoLines(e.Todos)))
	}
	return nil
}

// flushThinking closes a finished stretch of thinking into a collapsed
// line, the way Claude Code shows it; ctrl+o prints the text.
func (m *model) flushThinking() tea.Cmd {
	t := strings.TrimSpace(m.think)
	m.think = ""
	if t == "" {
		return nil
	}
	m.thoughts = append(m.thoughts, t)
	secs := int(time.Since(m.thinkStart).Round(time.Second).Seconds())
	return m.print("", thinkSt.Render(fmt.Sprintf("∴ Thought for %ds", max(secs, 1)))+dim.Render(" (ctrl+o to show thinking)"))
}

// toolCollapse is how many output lines a finished tool shows.
const toolCollapse = 3

// toolBody is what a finished tool call shows under its header: a summary
// for reads, the diff for edits, otherwise the first lines of output.
func (m *model) toolBody(start, e agent.Event) string {
	if e.Err != "" {
		return m.collapse(errSt.Render("Error: " + e.Err))
	}
	out := e.Output
	if out == "" {
		out = e.Text
	}
	switch strings.TrimPrefix(start.Tool, "↳ ") {
	case "read":
		n := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1
		if strings.TrimSpace(out) == "" {
			n = 0
		}
		return fmt.Sprintf("Read %s %s", bold.Render(strconv.Itoa(n)), plural(n, "line"))
	case "edit", "write":
		return colorDiff(capLines(out, 40))
	case "todo_write":
		return ""
	}
	return m.collapse(colorDiff(out))
}

// collapse keeps the first lines and remembers the rest for ctrl+r.
func (m *model) collapse(s string) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) <= toolCollapse+1 {
		return s
	}
	m.lastFull = s
	return strings.Join(lines[:toolCollapse], "\n") + "\n" +
		faint.Render(fmt.Sprintf("… +%d lines ", len(lines)-toolCollapse)) + dim.Render("(ctrl+r to expand)")
}

func capLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + "\n" + faint.Render(fmt.Sprintf("… +%d lines", len(lines)-n))
}

func plural(n int, w string) string {
	if n == 1 {
		return w
	}
	return w + "s"
}

func (m *model) onDone(v doneMsg) tea.Cmd {
	m.running, m.cancel, m.progress = false, nil, ""
	cmds := []tea.Cmd{m.flushThinking()}
	if s := strings.TrimSpace(m.live.String()); s != "" && v.err != nil {
		cmds = append(cmds, m.print("", "⏺ "+strings.TrimLeft(m.markdown(s), "\n "))) // partial text before an error
	}
	for _, p := range m.pending { // calls cut off by an interrupt or error
		cmds = append(cmds, m.print("", errSt.Render("⏺")+" "+toolHeader(p)))
	}
	m.pending = nil
	m.live.Reset()
	pending := m.a.TakeSteer() // typed mid-turn and not yet delivered
	m.steering = false
	switch {
	case v.err == nil:
	case errors.Is(v.err, context.Canceled):
		if len(pending) > 0 {
			cmds = append(cmds, m.print(result(errSt.Render("Interrupted")+dim.Render(" · sending your message"))))
		} else {
			cmds = append(cmds, m.print(result(errSt.Render("Interrupted")+dim.Render(" · What should cometcli do instead?"))))
		}
	default:
		cmds = append(cmds, m.print(result(errSt.Render(v.err.Error()))))
	}
	if len(pending) > 0 {
		cmds = append(cmds, m.startTurn(strings.Join(pending, "\n\n")))
	}
	return tea.Sequence(cmds...)
}

// --- approvals -------------------------------------------------------------

// approve is the toolkit.Approver: it parks the tool goroutine until the
// operator answers the dialog.
func (m *model) approve(c *toolkit.Context, prompt string, tier toolkit.Tier, detail map[string]any) (bool, error) {
	p := &pendingApproval{prompt: prompt, tier: tier, detail: detail, resp: make(chan approvalResult, 1)}
	m.send(approvalMsg{p})
	select {
	case r := <-p.resp:
		return r.ok, r.err
	case <-c.Done():
		return false, c.Err()
	}
}

type option struct {
	label string
	kind  string // yes | always | no
}

func (p *pendingApproval) options() []option {
	opts := []option{{"Yes", "yes"}}
	if r, _ := p.detail[toolkit.RuleHint].(string); r != "" && p.tier < toolkit.TierOnChain {
		opts = append(opts, option{"Yes, and don't ask again for " + bold.Render(r), "always"})
	}
	return append(opts, option{"No, and tell cometcli what to do differently " + dim.Render("(esc)"), "no"})
}

func (m *model) approvalKey(k tea.KeyMsg) tea.Cmd {
	p := m.approval
	opts := p.options()
	switch k.String() {
	case "up", "k":
		p.sel = (p.sel - 1 + len(opts)) % len(opts)
	case "down", "j", "tab":
		p.sel = (p.sel + 1) % len(opts)
	case "1", "2", "3":
		if i := int(k.String()[0] - '1'); i < len(opts) {
			return m.answer(opts[i].kind)
		}
	case "y":
		return m.answer("yes")
	case "n", "esc":
		return m.answer("no")
	case "ctrl+c":
		return m.answer("no")
	case "enter":
		return m.answer(opts[p.sel].kind)
	}
	return nil
}

func (m *model) answer(kind string) tea.Cmd {
	p := m.approval
	m.approval = nil
	switch kind {
	case "yes":
		p.resp <- approvalResult{ok: true}
		return nil
	case "always":
		rule, _ := p.detail[toolkit.RuleHint].(string)
		path, err := m.a.AllowAlways(rule)
		p.resp <- approvalResult{ok: true}
		if err != nil {
			return m.print(result(warnSt.Render("allowed for this session; couldn't save: " + err.Error())))
		}
		return m.print(result(dim.Render("allowed " + rule + " — saved to " + path)))
	}
	p.resp <- approvalResult{err: errors.New("the operator declined")}
	m.interrupt()
	return nil
}

// --- choices and secrets ---------------------------------------------------

// choose is the toolkit.Chooser: it parks the tool until the operator picks.
func (m *model) choose(c *toolkit.Context, prompt string, options []string) (int, error) {
	p := &pendingChoice{prompt: prompt, options: options, resp: make(chan choiceResult, 1)}
	m.send(choiceMsg{p})
	select {
	case r := <-p.resp:
		return r.i, r.err
	case <-c.Done():
		return 0, c.Err()
	}
}

func (m *model) choiceKey(k tea.KeyMsg) tea.Cmd {
	p := m.choice
	switch k.String() {
	case "up", "k":
		p.sel = (p.sel - 1 + len(p.options)) % len(p.options)
	case "down", "j", "tab":
		p.sel = (p.sel + 1) % len(p.options)
	case "enter":
		m.choice = nil
		p.resp <- choiceResult{i: p.sel}
	case "esc", "ctrl+c":
		m.choice = nil
		p.resp <- choiceResult{err: errors.New("cancelled by the operator")}
	default:
		if n, err := strconv.Atoi(k.String()); err == nil && n >= 1 && n <= len(p.options) {
			m.choice = nil
			p.resp <- choiceResult{i: n - 1}
		}
	}
	return nil
}

// secret is the toolkit.SecretFunc: a masked input that is never printed,
// stored in history, or sent anywhere but the waiting tool.
func (m *model) secret(c *toolkit.Context, prompt string) (string, error) {
	in := textinput.New()
	in.EchoMode = textinput.EchoPassword
	in.EchoCharacter = '•'
	in.Prompt = ""
	in.Focus()
	p := &pendingSecret{prompt: prompt, input: in, resp: make(chan secretResult, 1)}
	m.send(secretMsg{p})
	select {
	case r := <-p.resp:
		return r.s, r.err
	case <-c.Done():
		return "", c.Err()
	}
}

func (m *model) secretKey(k tea.KeyMsg) tea.Cmd {
	p := m.secretReq
	switch k.String() {
	case "enter":
		m.secretReq = nil
		p.resp <- secretResult{s: p.input.Value()}
		return nil
	case "esc", "ctrl+c":
		m.secretReq = nil
		p.resp <- secretResult{err: errors.New("cancelled by the operator")}
		return nil
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(k)
	return cmd
}

func (m *model) dialogView(w int, title, body string) string {
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("214")).Width(w-2).Padding(0, 1)
	return box.Render(bold.Render(title)+"\n\n"+body) + "\n"
}

// --- suggestions -----------------------------------------------------------

// currentToken is the word under the cursor (the input's last word).
func (m *model) currentToken() string {
	v := m.ta.Value()
	if i := strings.LastIndexAny(v, " \n"); i >= 0 {
		return v[i+1:]
	}
	return v
}

func (m *model) suggest() {
	m.sugg, m.suggIdx = nil, 0
	v := m.ta.Value()
	tok := m.currentToken()
	switch {
	case strings.HasPrefix(v, "/") && !strings.ContainsAny(v, " \n"):
		for _, c := range m.cmds {
			if strings.HasPrefix(c.Name, v) {
				m.sugg = append(m.sugg, suggestion{insert: c.Name, label: c.Name + " " + dim.Render(c.Args), desc: c.Desc})
			}
		}
	case strings.HasPrefix(tok, "@") && len(tok) >= 1:
		if m.files == nil {
			m.files = m.a.ProjectFiles(5000)
		}
		q := strings.ToLower(tok[1:])
		for _, f := range m.files {
			if q == "" || strings.Contains(strings.ToLower(f), q) {
				m.sugg = append(m.sugg, suggestion{insert: "@" + f, label: "@" + f})
				if len(m.sugg) == 8 {
					break
				}
			}
		}
	}
	if len(m.sugg) > 8 {
		m.sugg = m.sugg[:8]
	}
}

func (m *model) acceptSuggestion() {
	s := m.sugg[m.suggIdx]
	v := m.ta.Value()
	v = v[:len(v)-len(m.currentToken())] + s.insert + " "
	m.ta.SetValue(v)
	m.ta.CursorEnd()
	m.sugg = nil
}

// --- view ------------------------------------------------------------------

func (m *model) View() string {
	var b strings.Builder
	w := max(40, m.width)
	if m.running {
		if s := strings.TrimSpace(m.live.String()); s != "" {
			b.WriteString("\n" + lastLines("⏺ "+strings.TrimLeft(m.markdown(s), "\n "), 16) + "\n")
		}
		// running tool calls: blinking bullet, like Claude Code
		blink := "⏺"
		if time.Now().UnixMilli()/500%2 == 1 {
			blink = " "
		}
		for i, p := range m.pending {
			b.WriteString("\n" + blink + " " + toolHeader(p) + "\n")
			status := "Running…"
			if m.approval != nil || m.choice != nil || m.secretReq != nil {
				status = "Waiting for your answer…"
			} else if i == len(m.pending)-1 && m.progress != "" {
				status = oneLine(m.progress, w-8)
			}
			b.WriteString(faint.Render("  ⎿  ") + dim.Render(status) + "\n")
		}
		verb := m.verb
		if verb == "" {
			verb = verbs[0]
		}
		if strings.TrimSpace(m.think) != "" && m.live.Len() == 0 {
			verb = "Thinking"
		}
		if m.approval == nil && m.choice == nil && m.secretReq == nil { // paused while asking
			u, _ := m.a.Usage()
			fmt.Fprintf(&b, "\n%s %s %s\n", m.sp.View(), accentSt.Render(verb+"…"),
				dim.Render(fmt.Sprintf("(%s · ↑ %s tokens · esc to interrupt)", time.Since(m.started).Round(time.Second), humanTok(u.Input+u.Output))))
			if m.steering {
				b.WriteString(faint.Render("  ⎿  ") + dim.Render("your message goes in after this step · esc to send it now") + "\n")
			}
		}
	}
	b.WriteString("\n")
	switch {
	case m.secretReq != nil:
		b.WriteString(m.dialogView(w, m.secretReq.prompt, m.secretReq.input.View()+"\n"+dim.Render("enter to submit · esc to cancel · never stored or sent to the model")))
		return b.String()
	case m.choice != nil:
		var lines []string
		for i, o := range m.choice.options {
			prefix := "  "
			label := fmt.Sprintf("%d. %s", i+1, o)
			if i == m.choice.sel {
				prefix, label = accentSt.Render("❯ "), accentSt.Render(fmt.Sprintf("%d. ", i+1))+o
			}
			lines = append(lines, prefix+label)
		}
		b.WriteString(m.dialogView(w, m.choice.prompt, strings.Join(lines, "\n")))
		return b.String()
	case m.approval != nil:
		b.WriteString(m.approvalView(w))
		return b.String()
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Width(w-2).Padding(0, 1)
	b.WriteString(box.Render("> " + m.ta.View()))
	b.WriteString("\n")
	switch {
	case m.ta.Value() == "?":
		b.WriteString(dim.Render(shortcuts) + "\n")
	case len(m.sugg) > 0:
		for i, s := range m.sugg {
			line := "  " + s.label
			if s.desc != "" {
				line += "  " + dim.Render(oneLine(s.desc, w-len(s.insert)-12))
			}
			if i == m.suggIdx {
				line = accentSt.Render("❯") + line[1:]
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString(m.footer(w))
	return b.String()
}

const shortcuts = `  ! for bash mode            / for commands         @ for file paths       # to memorize
  shift+tab to cycle modes   esc to interrupt       ctrl+r expand output   ctrl+o show thinking
  ↑↓ history                 \⏎ or ctrl+j newline   ctrl+c clear / exit    tab to complete`

func (m *model) footer(w int) string {
	left := dim.Render("? for shortcuts")
	switch m.a.Policy.Mode {
	case agent.ModeAcceptEdits:
		left = okSt.Render("⏵⏵ accept edits on") + dim.Render(" (shift+tab to cycle)")
	case agent.ModeReadOnly:
		left = lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Render("⏸ read-only mode on") + dim.Render(" (shift+tab to cycle)")
	case agent.ModeBypass:
		left = errSt.Render("⏵⏵ bypass permissions on") + dim.Render(" (shift+tab to cycle)")
	}
	if m.hint != "" {
		left = warnSt.Render(m.hint)
	}
	scope := "general"
	if m.a.Node() {
		scope = "node:" + m.a.Ctx.Profile.Name
	}
	right := scope + " · " + m.a.Provider.Name() + "/" + m.a.Model
	if pct := m.a.ContextPercent(); pct > 0 {
		right += fmt.Sprintf(" · ctx %d%%", pct)
	}
	right = dim.Render(right)
	gap := w - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		return " " + left + "\n " + right
	}
	return " " + left + strings.Repeat(" ", gap) + right
}

func (m *model) approvalView(w int) string {
	p := m.approval
	title, body := describe(p)
	border := lipgloss.Color("214")
	if p.tier == toolkit.TierOnChain {
		border = lipgloss.Color("203")
	}
	var b strings.Builder
	b.WriteString(bold.Render(title) + "\n\n")
	b.WriteString(body + "\n\n")
	if p.tier == toolkit.TierOnChain {
		b.WriteString(errSt.Render("This broadcasts a transaction.") + " ")
	}
	b.WriteString("Do you want to proceed?\n")
	for i, o := range p.options() {
		prefix := "  "
		label := fmt.Sprintf("%d. %s", i+1, o.label)
		if i == p.sel {
			prefix = accentSt.Render("❯ ")
			label = accentSt.Render(fmt.Sprintf("%d. ", i+1)) + o.label
		}
		b.WriteString(prefix + label + "\n")
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Width(w-2).Padding(0, 1)
	return box.Render(strings.TrimRight(b.String(), "\n")) + "\n"
}

// describe renders an approval request: a title and the detail worth
// showing (command, diff, tx document, reasons).
func describe(p *pendingApproval) (string, string) {
	d := p.detail
	var lines []string
	shown := map[string]bool{toolkit.RuleHint: true}
	str := func(k string) string {
		shown[k] = true
		if v, ok := d[k]; ok && v != nil {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}
	title := "Approval needed"
	switch {
	case p.tier == toolkit.TierOnChain:
		title = "Transaction"
		lines = append(lines, p.prompt)
		shown["doc"] = true
	case str("command") != "":
		title = "Bash command"
		if h := str("host"); h != "" && h != "local" {
			title += " on " + h
		}
		lines = append(lines, "  "+bold.Render(str("command")))
		if desc := str("description"); desc != "" {
			lines = append(lines, "  "+dim.Render(desc))
		}
		str("cwd")
	case str("diff") != "":
		title = "Edit " + str("path")
		lines = append(lines, colorDiff(str("diff")))
		str("host")
	case str("new_file") != "":
		title = "Create " + str("path")
		lines = append(lines, "  "+str("new_file"))
		str("host")
	default:
		lines = append(lines, p.prompt)
	}
	if why := str("why"); why != "" {
		lines = append(lines, dim.Render("why: "+why))
	}
	if notes := str("notes"); notes != "" {
		lines = append(lines, warnSt.Render(notes))
	}
	for k, v := range d {
		if shown[k] || strings.HasPrefix(k, "_") {
			continue
		}
		lines = append(lines, dim.Render(k+": ")+oneLine(fmt.Sprint(v), 200))
	}
	return title, strings.Join(lines, "\n")
}

// --- rendering helpers -------------------------------------------------------

func (m *model) banner() string {
	cwd := m.a.WorkRoot
	if home, _ := os.UserHomeDir(); home != "" && strings.HasPrefix(cwd, home) {
		cwd = "~" + strings.TrimPrefix(cwd, home)
	}
	// a fixed-width box that never wraps: long paths lose their middle
	inner := min(max(40, m.width-4), 58)
	fit := func(s string) string {
		if lipgloss.Width(s) <= inner-4 {
			return s
		}
		r := []rune(s)
		keep := inner - 5
		return string(r[:keep/3]) + "…" + string(r[len(r)-(keep-keep/3):])
	}
	fitEnd := func(s string) string {
		if r := []rune(s); len(r) > inner-4 {
			return string(r[:inner-5]) + "…"
		}
		return s
	}
	scope := "general — this machine"
	if m.a.Node() {
		p := m.a.Ctx.Profile
		scope = fmt.Sprintf("node %s (%s)", p.Name, p.ChainID)
	}
	body := accentSt.Render("✻") + " Welcome to " + bold.Render("cometcli") + "!\n\n" +
		dim.Render(fit("  /help for help, /status for your current setup")) + "\n\n" +
		dim.Render(fit("  cwd: "+cwd)) + "\n" +
		dim.Render(fitEnd("  "+scope+" · "+m.a.Provider.Name()+"/"+m.a.Model))
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1).Width(inner).Render(body)
	out := []string{box}
	if art := cometArt(m.width); art != "" && len(m.a.History()) == 0 {
		out = []string{art, "", box}
	}
	for _, n := range m.notes {
		out = append(out, result(dim.Render(n)))
	}
	if h := m.a.History(); len(h) > 0 {
		// resumed: show where we left off
		for i := len(h) - 1; i >= 0; i-- {
			if h[i].Role == "assistant" && strings.TrimSpace(h[i].Text) != "" {
				out = append(out, "", dim.Render(" last answer:"), "⏺ "+strings.TrimLeft(m.markdown(h[i].Text), "\n "))
				break
			}
		}
		return strings.Join(out, "\n")
	}
	tips := []string{
		`Ask about this machine, e.g. "why is the disk filling up?"`,
		"Work on a validator: " + bold.Render("/one <profile>") + " (or start with cometcli one <profile>)",
		bold.Render("!") + " runs a command in your terminal · " + bold.Render("#") + " saves to memory · " + bold.Render("@") + " attaches a file",
	}
	if m.a.Node() {
		tips = []string{
			`Ask about this node, e.g. "is my validator healthy?"`,
			bold.Render("/incident") + " works a problem end to end: triage → known case → fix → verify",
			bold.Render("/one off") + " returns to general mode · " + bold.Render("!") + " runs a command in your terminal",
		}
	}
	out = append(out, "", dim.Render(" Tips for getting started:"), "")
	wrap := lipgloss.NewStyle().Width(max(30, m.width-5))
	for i, t := range tips {
		lines := strings.Split(wrap.Render(t), "\n") // hanging indent under the number
		for j := range lines {
			lines[j] = strings.TrimRight(lines[j], " ")
			if j > 0 {
				lines[j] = "    " + lines[j]
			}
		}
		out = append(out, dim.Render(fmt.Sprintf(" %d. ", i+1))+strings.Join(lines, "\n"))
	}
	return strings.Join(out, "\n")
}

// markdown renders assistant text with no outer margin; lines after the
// first are indented to sit under the "● " bullet.
func (m *model) markdown(s string) string {
	w := min(max(40, m.width-4), 120)
	if m.md == nil || m.mdWidth != w {
		cfg := styles.DarkStyleConfig
		if m.mdStyle == "light" {
			cfg = styles.LightStyleConfig
		}
		zero := uint(0)
		cfg.Document.Margin = &zero
		cfg.Document.BlockPrefix, cfg.Document.BlockSuffix = "", ""
		// Claude Code look: "- " bullets, headings as bold text
		cfg.Item.BlockPrefix = "- "
		// inline code: just a color, no padding or background
		codeColor := "#B1B9F9"
		cfg.Code.Prefix, cfg.Code.Suffix, cfg.Code.BackgroundColor, cfg.Code.Color = "", "", nil, &codeColor
		t := true
		for _, h := range []*ansi.StyleBlock{&cfg.H1, &cfg.H2, &cfg.H3, &cfg.H4, &cfg.H5, &cfg.H6} {
			h.Prefix, h.Suffix, h.BackgroundColor, h.Bold = "", "", nil, &t
		}
		if r, err := glamour.NewTermRenderer(glamour.WithStyles(cfg), glamour.WithWordWrap(w)); err == nil {
			m.md, m.mdWidth = r, w
		}
	}
	out := s
	if m.md != nil {
		if r, err := m.md.Render(s); err == nil {
			out = r
		}
	}
	lines := strings.Split(strings.Trim(out, "\n"), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}

func userLine(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		p := "  "
		if i == 0 {
			p = "> "
		}
		lines[i] = userSt.Render(p + l + " ")
	}
	return "\n" + strings.Join(lines, "\n")
}

// result indents text under the previous line with Claude Code's ⎿.
func result(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if i == 0 {
			lines[i] = faint.Render("  ⎿  ") + l
		} else {
			lines[i] = "     " + l
		}
	}
	return strings.Join(lines, "\n")
}

func toolHeader(e agent.Event) string {
	a := e.Args
	arg := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := a[k]; ok && v != nil {
				return oneLine(fmt.Sprint(v), 160)
			}
		}
		return ""
	}
	name := e.Tool
	var s string
	switch strings.TrimPrefix(name, "↳ ") {
	case "bash":
		s = arg("command")
	case "read", "write", "edit":
		s = arg("file_path")
	case "glob", "grep":
		s = arg("pattern")
		if p := arg("path"); p != "" {
			s += " in " + p
		}
	case "web_fetch":
		s = arg("url")
	case "task":
		s = arg("description")
	case "use_node":
		s = arg("profile")
	default:
		s = agent.CompactArgs(a)
	}
	label := toolLabel(name)
	if s == "" {
		return bold.Render(label)
	}
	return bold.Render(label) + "(" + s + ")"
}

func toolLabel(name string) string {
	prefix := ""
	if strings.HasPrefix(name, "↳ ") {
		prefix, name = "↳ ", strings.TrimPrefix(name, "↳ ")
	}
	switch name {
	case "bash":
		name = "Bash"
	case "read":
		name = "Read"
	case "write":
		name = "Write"
	case "edit":
		name = "Update"
	case "glob":
		name = "Search"
	case "grep":
		name = "Grep"
	case "web_fetch":
		name = "Fetch"
	case "task":
		name = "Task"
	case "tool_search":
		name = "Load tools"
	case "use_node":
		name = "Use node"
	default:
		if strings.HasPrefix(name, "mcp__") {
			srv, tool, _ := strings.Cut(strings.TrimPrefix(name, "mcp__"), "__")
			name = srv + " · " + tool + " (MCP)"
		}
	}
	return prefix + name
}

// colorDiff colors +/- lines of a diff; other text passes through.
func colorDiff(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+ ") || l == "+":
			lines[i] = addSt.Render(l)
		case strings.HasPrefix(l, "- ") || l == "-":
			lines[i] = delSt.Render(l)
		case strings.HasPrefix(l, "--- "):
			lines[i] = dim.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

func todoLines(ts []agent.Todo) string {
	var out []string
	for _, t := range ts {
		switch t.Status {
		case "completed":
			out = append(out, okSt.Render("☒ ")+faint.Strikethrough(true).Render(t.Content))
		case "in_progress":
			out = append(out, accentSt.Render("☐ ")+bold.Render(t.Content))
		default:
			out = append(out, "☐ "+t.Content)
		}
	}
	return strings.Join(out, "\n")
}

// verbs for the spinner, one picked per turn (Claude Code style)
var verbs = []string{"Thinking", "Pondering", "Investigating", "Digging", "Checking", "Inspecting", "Crunching",
	"Mulling", "Percolating", "Synthesizing", "Ruminating", "Deliberating", "Tinkering", "Sleuthing", "Untangling"}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if n > 3 && len(s) > n {
		s = s[:n-1] + "…"
	}
	return s
}

func humanTok(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return strconv.Itoa(n)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
