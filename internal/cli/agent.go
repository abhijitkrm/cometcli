package cli

import (
	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// agentFlags wires the bounded-run flags shared by agent and ask.
func agentFlags(cmd *cobra.Command) {
	cmd.Flags().String("mode", "", "approval posture: ops | readonly (default from profile agent.mode)")
	cmd.Flags().Bool("safe", false, "shorthand for --mode readonly: observe/diagnose tools only")
	cmd.Flags().StringSlice("autopilot", nil, "tiers that skip the confirm prompt (only local-change; on-chain never)")
	cmd.Flags().Bool("no-stream", false, "disable token streaming")
	cmd.Flags().Int("budget", 0, "max tool calls per turn (0 = unlimited)")
	cmd.Flags().Int("max-iter", 0, "max model iterations per turn (default 16)")
}

// AgentCmd opens the agentic REPL. Implemented in internal/agent.
func AgentCmd(reg *toolkit.Registry) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Open the AI SRE terminal (interactive REPL)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgent(cmd, reg)
		},
	}
	agentFlags(cmd)
	cmd.Flags().String("task", "", "run one task non-interactively and exit (for cron/CI)")
	return cmd
}

// AskCmd is a one-shot agent invocation.
func AskCmd(reg *toolkit.Registry) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ask <question>",
		Short: "Ask the agent a one-shot question",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAsk(cmd, reg, args)
		},
	}
	agentFlags(cmd)
	return cmd
}
