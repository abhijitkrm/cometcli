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
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"

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
	queue   []string

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
	cmds := []tea.Cmd{textarea.Blink, m.print(m.banner())}
	if m.initial != "" {
		cmds = append(cmds, m.submit(m.initial))
		m.initial = ""
	}
	return tea.Sequence(cmds...)
}

// print commits lines to the scrollback above the live region.
func (m *model) print(lines ...string) tea.Cmd {
	s := strings.Join(lines, "\n")
	if s == "" {
		return nil
	}
	m.printed = append(m.printed, s)
	return tea.Println(s)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		if v.Width <= 0 {
			return m, nil // some ptys report 0×0
		}
		m.width = v.Width
		m.ta.SetWidth(max(20, v.Width-6))
		return m, nil
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
		m.queue = append(m.queue, text)
		return m.print(userLine(text), result(dim.Render("queued — sent when the current turn ends")))
	}
	return tea.Sequence(m.print(userLine(text)), m.startTurn(m.a.ExpandMentions(text)))
}

func (m *model) startTurn(prompt string) tea.Cmd {
	ctx, cancel := context.WithCancel(m.a.Ctx.Context)
	m.cancel, m.running, m.started = cancel, true, time.Now()
	m.live.Reset()
	m.think = ""
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
	case agent.EvDelta:
		m.live.WriteString(e.Text)
		return nil
	case agent.EvThinking:
		m.think += e.Text
		return nil
	case agent.EvText:
		m.live.Reset()
		m.think = ""
		return m.print("", accentSt.Render("● ")+strings.TrimLeft(m.markdown(e.Text), "\n "))
	case agent.EvToolStart:
		m.think = ""
		return m.print("", accentSt.Render("● ")+toolHeader(e))
	case agent.EvToolResult:
		m.progress = ""
		if e.Err != "" {
			return m.print(result(errSt.Render(e.Err)))
		}
		out := e.Output
		if out == "" {
			out = e.Text
		}
		return m.print(result(colorDiff(out)))
	case agent.EvNotice:
		return m.print(result(dim.Render(e.Text)))
	case agent.EvProgress:
		m.progress = e.Text
		return nil
	case agent.EvTodos:
		return m.print("", accentSt.Render("● ")+bold.Render("Update todos"), result(todoLines(e.Todos)))
	}
	return nil
}

func (m *model) onDone(v doneMsg) tea.Cmd {
	m.running, m.cancel, m.progress = false, nil, ""
	var cmds []tea.Cmd
	if s := strings.TrimSpace(m.live.String()); s != "" && v.err != nil {
		cmds = append(cmds, m.print("", accentSt.Render("● ")+s)) // partial text before an error
	}
	m.live.Reset()
	m.think = ""
	switch {
	case v.err == nil:
	case errors.Is(v.err, context.Canceled):
		cmds = append(cmds, m.print(result(warnSt.Render("Interrupted")+dim.Render(" · what should cometcli do instead?"))))
		m.queue = nil
	default:
		cmds = append(cmds, m.print(result(errSt.Render(v.err.Error()))))
	}
	if len(m.queue) > 0 {
		next := strings.Join(m.queue, "\n\n")
		m.queue = nil
		cmds = append(cmds, m.startTurn(m.a.ExpandMentions(next)))
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
			b.WriteString(lastLines(lipgloss.NewStyle().Width(w-2).Render(accentSt.Render("● ")+s), 14) + "\n")
		} else if t := strings.TrimSpace(m.think); t != "" {
			b.WriteString(faint.Render(lastLines(lipgloss.NewStyle().Width(w-4).Render("∴ "+oneLine(t, 400)), 3)) + "\n")
		}
		if m.progress != "" {
			b.WriteString(accentSt.Render("  ⎿  ") + dim.Render(oneLine(m.progress, w-8)) + "\n")
		}
		u, _ := m.a.Usage()
		fmt.Fprintf(&b, "\n%s %s %s\n", m.sp.View(), accentSt.Render(working(m.started)),
			dim.Render(fmt.Sprintf("(%s · ↑ %s tokens · esc to interrupt)", time.Since(m.started).Round(time.Second), humanTok(u.Input+u.Output))))
	}
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
	b.WriteString(box.Render(accentSt.Render("❯ ") + m.ta.View()))
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

const shortcuts = `  ! run in your terminal    / commands         @ attach a file     # remember
  shift+tab cycle mode      esc interrupt      ↑↓ history          ctrl+j newline
  ctrl+c clear / exit       \⏎ newline         tab complete        ctrl+d exit`

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
	scope := "general mode — shell, files and web on this machine"
	if m.a.Node() {
		p := m.a.Ctx.Profile
		scope = fmt.Sprintf("node mode — %s (%s, %s)", p.Name, p.ChainID, orDefault(p.Transport.Type, "local"))
	}
	cwd := m.a.WorkRoot
	if home, _ := os.UserHomeDir(); home != "" && strings.HasPrefix(cwd, home) {
		cwd = "~" + strings.TrimPrefix(cwd, home)
	}
	inner := accentSt.Render("✻") + " " + bold.Render("cometcli") + "\n\n" +
		dim.Render("  "+scope) + "\n" +
		dim.Render("  "+m.a.Provider.Name()+"/"+m.a.Model+" · "+cwd)
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1).Render(inner)
	out := []string{box, dim.Render(" /help for commands · ! runs in your terminal · # remembers · @ attaches a file · shift+tab changes mode"), ""}
	for _, n := range m.notes {
		out = append(out, result(dim.Render(n)))
	}
	if h := m.a.History(); len(h) > 0 {
		// resumed: show where we left off
		for i := len(h) - 1; i >= 0; i-- {
			if h[i].Role == "assistant" && strings.TrimSpace(h[i].Text) != "" {
				out = append(out, dim.Render(" last answer:"), accentSt.Render("● ")+strings.TrimLeft(m.markdown(h[i].Text), "\n "))
				break
			}
		}
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

var verbs = []string{"Working", "Investigating", "Checking", "Digging", "Reasoning", "Inspecting"}

func working(start time.Time) string {
	return verbs[int(time.Since(start)/(6*time.Second))%len(verbs)] + "…"
}

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
