package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

var (
	promptSt = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true)
	toolSt   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	callSt   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errSt    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

// REPL is the line-oriented agent terminal (`cometcli agent`). Approvals
// use whatever Approver the agent's context carries (stdin y/N here).
type REPL struct {
	Agent *Agent
	Out   io.Writer
	In    io.Reader
	md    *glamour.TermRenderer
}

// NewREPL creates the interactive terminal.
func NewREPL(a *Agent, in io.Reader, out io.Writer) *REPL {
	r, _ := glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(100))
	return &REPL{Agent: a, Out: out, In: in, md: r}
}

// Printer renders agent events as plain terminal lines. Streamed deltas
// print as they arrive; when nothing streamed, the round's full text is
// rendered as markdown (when md is non-nil).
type Printer struct {
	Out      io.Writer
	MD       *glamour.TermRenderer
	streamed bool
	thinking bool
}

// Handle renders one event.
func (p *Printer) Handle(e Event) {
	if p.thinking && e.Kind != EvThinking {
		fmt.Fprintln(p.Out)
		p.thinking = false
	}
	switch e.Kind {
	case EvThinking:
		if !p.thinking {
			fmt.Fprint(p.Out, toolSt.Render("∴ "))
			p.thinking = true
		}
		fmt.Fprint(p.Out, toolSt.Render(e.Text))
	case EvTodos:
		if p.streamed {
			fmt.Fprintln(p.Out)
			p.streamed = false
		}
		fmt.Fprintln(p.Out, toolSt.Render(RenderTodos(e.Todos)))
	case EvProgress:
		fmt.Fprintf(p.Out, "%s %s\n", toolSt.Render("…"), toolSt.Render(e.Text))
	case EvNotice:
		if p.streamed {
			fmt.Fprintln(p.Out)
			p.streamed = false
		}
		fmt.Fprintf(p.Out, "%s %s\n", toolSt.Render("·"), toolSt.Render(e.Text))
	case EvDelta:
		p.streamed = true
		fmt.Fprint(p.Out, e.Text)
	case EvText:
		if p.streamed {
			fmt.Fprintln(p.Out)
			p.streamed = false
			return
		}
		if p.MD != nil {
			if out, err := p.MD.Render(e.Text); err == nil {
				fmt.Fprint(p.Out, out)
				return
			}
		}
		fmt.Fprintln(p.Out, e.Text)
	case EvToolStart:
		fmt.Fprintf(p.Out, "%s %s %s\n", toolSt.Render("◐"), callSt.Render(e.Tool), toolSt.Render(CompactArgs(e.Args)))
	case EvToolResult:
		if e.Err != "" {
			fmt.Fprintf(p.Out, "  %s %s\n", toolSt.Render("✗"), errSt.Render(e.Err))
		} else {
			fmt.Fprintf(p.Out, "  %s %s\n", toolSt.Render("✓"), toolSt.Render(e.Text))
		}
	}
}

// Run starts the loop until /exit or EOF.
func (r *REPL) Run() error {
	a := r.Agent
	pr := &Printer{Out: r.Out, MD: r.md}
	a.OnEvent = pr.Handle

	scope := "general"
	if a.Node() {
		scope = a.Ctx.Profile.Name
	}
	fmt.Fprintf(r.Out, "%s — %s/%s on %s · %s\n%s\n\n",
		promptSt.Render("cometcli agent"), a.Provider.Name(), a.Model,
		scope, a.Policy, toolSt.Render("type /help for commands, ctrl+c cancels a running turn, /exit quits"))

	sc := bufio.NewScanner(r.In)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for {
		fmt.Fprint(r.Out, promptSt.Render("cometcli> "))
		if !sc.Scan() {
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			prompt, done := r.slash(line)
			if done {
				return nil
			}
			if prompt == "" {
				continue
			}
			line = prompt
		}
		r.turn(line)
		fmt.Fprintln(r.Out)
	}
}

// turn runs one agent turn; ctrl+c cancels the turn, not the REPL.
func (r *REPL) turn(line string) {
	ctx, cancel := context.WithCancel(r.Agent.Ctx)
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-ctx.Done():
		}
	}()
	if _, err := r.Agent.Run(ctx, line); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(r.Out, toolSt.Render("\n(cancelled)"))
			return
		}
		fmt.Fprintf(r.Out, "%s %v\n", errSt.Render("error:"), err)
	}
}

// slash handles a command; returns a prompt to submit (from /runbook) and
// whether to quit.
func (r *REPL) slash(cmd string) (prompt string, quit bool) {
	switch strings.Fields(cmd)[0] {
	case "/exit", "/quit", "/q":
		return "", true
	case "/help":
		fmt.Fprintln(r.Out, "Commands:\n"+HelpText(r.Agent)+"\n  /exit                         quit")
		return "", false
	}
	res, err := RunCommand(r.Agent, r.Agent.Ctx, r.Agent.Reg, cmd)
	switch {
	case errors.Is(err, ErrUnknownCommand):
		fmt.Fprintln(r.Out, "unknown command — /help")
	case err != nil:
		fmt.Fprintln(r.Out, errSt.Render(err.Error()))
	default:
		if res.Text != "" {
			fmt.Fprintln(r.Out, toolSt.Render(res.Text))
		}
		if res.Job != nil {
			if txt, err := res.Job(r.Agent.Ctx); err != nil {
				fmt.Fprintln(r.Out, errSt.Render(err.Error()))
			} else if txt != "" {
				fmt.Fprintln(r.Out, toolSt.Render(txt))
			}
		}
	}
	return res.Prompt, false
}

// CompactArgs renders tool args on one bounded line.
func CompactArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, args[k]))
	}
	s := strings.Join(parts, " ")
	if len(s) > 100 {
		s = safeCut(s, 100) + "…"
	}
	return s
}
