package agent

import (
	"bufio"
	"fmt"
	"io"
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

// REPL is the interactive agent terminal.
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

// Run starts the loop until /exit or EOF.
func (r *REPL) Run() error {
	a := r.Agent
	a.OnText = func(t string) { r.renderMD(t) }
	a.OnToolCall = func(name string, args map[string]any) {
		fmt.Fprintf(r.Out, "%s %s %s\n", toolSt.Render("◐ tool:"), callSt.Render(name), toolSt.Render(compactArgs(args)))
	}
	a.OnToolResult = func(name, summary string, err error) {
		if err != nil {
			fmt.Fprintf(r.Out, "%s %s %s\n", toolSt.Render("✗"), callSt.Render(name), errSt.Render(err.Error()))
		} else {
			fmt.Fprintf(r.Out, "%s %s %s\n", toolSt.Render("✓"), callSt.Render(name), toolSt.Render(summary))
		}
	}

	fmt.Fprintf(r.Out, "%s — %s on %s (%s)\n%s\n\n",
		promptSt.Render("cometcli agent"), a.Provider.Name(), a.Model,
		a.Ctx.Profile.Name, "type /help, /reset, /profile, /audit, /exit")

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
			if done := r.slash(line); done {
				return nil
			}
			continue
		}
		if _, err := a.Run(r.Agent.Ctx, line); err != nil {
			fmt.Fprintf(r.Out, "%s %v\n", errSt.Render("error:"), err)
		}
		fmt.Fprintln(r.Out)
	}
}

func (r *REPL) slash(cmd string) bool {
	fields := strings.Fields(cmd)
	switch fields[0] {
	case "/exit", "/quit", "/q":
		return true
	case "/help":
		fmt.Fprintln(r.Out, `Commands:
  /profile        show active profile
  /reset          clear conversation
  /tools          list available tools
  /audit          show audit log path
  /mode           show approval mode
  /exit           quit`)
	case "/profile":
		p := r.Agent.Ctx.Profile
		fmt.Fprintf(r.Out, "%s (%s, chain %s, role %s)\n", p.Name, p.Transport.Type, p.ChainID, p.Role)
	case "/reset":
		r.Agent.Reset()
		fmt.Fprintln(r.Out, "conversation cleared")
	case "/tools":
		for _, t := range r.Agent.Reg.All() {
			fmt.Fprintf(r.Out, "  %-24s [%s] %s\n", t.Name(), t.Tier(), t.Desc())
		}
	case "/audit":
		if r.Agent.Audit() != nil {
			fmt.Fprintln(r.Out, r.Agent.Audit().Path())
		}
	case "/mode":
		fmt.Fprintln(r.Out, "approvals: observe/diagnose auto · local-change prompts · on-chain always prompts")
	default:
		fmt.Fprintln(r.Out, "unknown command: "+fields[0])
	}
	return false
}

func (r *REPL) renderMD(text string) {
	if r.md != nil {
		if out, err := r.md.Render(text); err == nil {
			fmt.Fprint(r.Out, out)
			return
		}
	}
	fmt.Fprintln(r.Out, text)
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
		s = s[:100] + "…"
	}
	return s
}
