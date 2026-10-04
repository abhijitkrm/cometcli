package cli

import (
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
	defer c.Audit.Close()
	a, err := agent.New(c, reg)
	if err != nil {
		return err
	}
	if err := applyAgentFlags(cmd, a); err != nil {
		return err
	}
	if oneshot != "" {
		pr := &agent.Printer{Out: c.Out}
		a.OnEvent = pr.Handle
		_, err := a.Run(cmd.Context(), oneshot)
		return err
	}
	repl := agent.NewREPL(a, cmd.InOrStdin(), c.Out)
	return repl.Run()
}

// applyAgentFlags layers --mode/--safe/--autopilot/--budget/--max-iter/
// --no-stream over the profile's agent defaults.
func applyAgentFlags(cmd *cobra.Command, a *agent.Agent) error {
	if m, _ := cmd.Flags().GetString("mode"); m != "" {
		mode, err := agent.ParseMode(m)
		if err != nil {
			return err
		}
		a.Policy.Mode = mode
	}
	if safe, _ := cmd.Flags().GetBool("safe"); safe {
		a.Policy.Mode = agent.ModeReadOnly
	}
	if tiers, _ := cmd.Flags().GetStringSlice("autopilot"); len(tiers) > 0 {
		for _, t := range tiers {
			if err := a.Policy.SetAutopilot(t, true); err != nil {
				return err
			}
		}
	}
	if b, _ := cmd.Flags().GetInt("budget"); b > 0 {
		a.MaxCalls = b
	}
	if n, _ := cmd.Flags().GetInt("max-iter"); n > 0 {
		a.MaxIter = n
	}
	if ns, _ := cmd.Flags().GetBool("no-stream"); ns {
		a.Stream = false
	}
	if a.Policy.ReadOnly() {
		// hard-refuse anything that slips past filtering
		a.Ctx.Approver = toolkit.DenyApprover
	}
	return nil
}
