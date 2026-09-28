package cli

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/spf13/cobra"

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
			if f, ok := cmd.InOrStdin().(*os.File); !ok || !term.IsTerminal(int(f.Fd())) {
				return fmt.Errorf("cometcli ui needs an interactive terminal — run it in a real shell (or use --json commands for scripting)")
			}
			c, err := NewCtx(cmd, true)
			if err != nil {
				return err
			}
			defer c.Close()
			return tui.RunApp(c, reg, time.Duration(interval)*time.Second)
		},
	}
	cmd.Flags().IntVar(&interval, "interval", 5, "refresh interval seconds")
	return cmd
}
