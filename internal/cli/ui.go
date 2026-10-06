package cli

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tui"
)

// UICmd is the chat-first terminal app: an agent conversation on the front
// pane (Claude Code style) with overview/fleet/logs/send dashboards behind
// it — all over the same registry and approval gate as the CLI.
func UICmd(reg *toolkit.Registry) *cobra.Command {
	var interval int
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Terminal app — chat with the agent, dashboards on tabs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUI(cmd, reg, time.Duration(interval)*time.Second)
		},
	}
	cmd.Flags().IntVar(&interval, "interval", 5, "refresh interval seconds")
	sessionFlags(cmd)
	return cmd
}

// isTerminal reports whether a stream (stdin or stdout) is a terminal.
func isTerminal(s any) bool {
	f, ok := s.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// runUI opens the chat-first TUI (shared by `cometcli ui` and bare `cometcli`).
func runUI(cmd *cobra.Command, reg *toolkit.Registry, interval time.Duration) error {
	if !isTerminal(cmd.InOrStdin()) {
		return fmt.Errorf("cometcli ui needs an interactive terminal — run it in a real shell (or use --json commands for scripting)")
	}
	c, err := NewCtx(cmd, true)
	if err != nil {
		return err
	}
	defer c.Close()
	defer c.Audit.Close()
	return tui.RunApp(c, reg, interval, func(a *agent.Agent) (string, error) {
		return applyAgentFlags(cmd, a)
	})
}
