package cli

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/serve"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// ServeCmd runs the local web chat: one agent session over a loopback-only
// HTTP API with an embedded page. Same tools, policy, redaction, and audit
// trail as the TUI.
func ServeCmd(reg *toolkit.Registry) *cobra.Command {
	var addr string
	var open bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Local web chat for the agent (loopback only, token-protected)",
		Long: `Serve a browser chat UI for the agent on 127.0.0.1.

The server prints a one-time link carrying a random token; only requests with
that token (as a cookie or Bearer header) and a loopback Host are accepted.
Approvals (local-change diffs, on-chain transactions) appear as modals in the
page. To reach it from another machine, tunnel it: ssh -L 8765:127.0.0.1:8765 host`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := NewCtx(cmd, true)
			if err != nil {
				return err
			}
			defer c.Close()
			defer c.Audit.Close()
			a, agentErr := agent.New(c, reg)
			if agentErr == nil {
				if err := applyAgentFlags(cmd, a); err != nil {
					return err
				}
			}
			ln, err := serve.Listen(addr)
			if err != nil {
				return err
			}
			s := serve.New(c, reg, a, agentErr)
			url := fmt.Sprintf("http://%s/?token=%s", ln.Addr(), s.Token)
			fmt.Fprintf(c.Out, "cometcli web chat for profile %s\n  %s\n", c.Profile.Name, url)
			if agentErr != nil {
				fmt.Fprintf(c.Out, "  (agent unavailable: %v)\n", agentErr)
			}
			fmt.Fprintln(c.Out, "ctrl+c to stop")
			if open {
				openBrowser(url)
			}
			return s.Serve(cmd.Context(), ln)
		},
	}
	agentFlags(cmd)
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8765", "listen address (loopback only; port 0 picks a free one)")
	cmd.Flags().BoolVar(&open, "open", false, "open the chat in the default browser")
	return cmd
}

func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	_ = c.Start()
}
