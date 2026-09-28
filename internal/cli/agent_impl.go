package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// runAgentImpl builds the agent on a context and runs REPL (empty oneshot)
// or a single turn.
func runAgentImpl(cmd *cobra.Command, reg *toolkit.Registry, oneshot string) error {
	c, err := NewCtx(cmd, true)
	if err != nil {
		return err
	}
	defer c.Close()
	a, err := agent.New(c, reg)
	if err != nil {
		return err
	}
	if safe, _ := cmd.Flags().GetBool("safe"); safe {
		a.Safe = true
		c.Approver = toolkit.DenyApprover // hard-refuse anything that slips past filtering
	}
	if b, _ := cmd.Flags().GetInt("budget"); b > 0 {
		a.MaxCalls = b
	}
	if n, _ := cmd.Flags().GetInt("max-iter"); n > 0 {
		a.MaxIter = n
	}
	if oneshot != "" {
		a.OnText = func(t string) { fmt.Fprintln(c.Out, t) }
		a.OnToolCall = func(name string, args map[string]any) {
			fmt.Fprintf(c.Out, "◐ %s %v\n", name, args)
		}
		_, err := a.Run(cmd.Context(), oneshot)
		return err
	}
	repl := agent.NewREPL(a, cmd.InOrStdin(), c.Out)
	return repl.Run()
}
