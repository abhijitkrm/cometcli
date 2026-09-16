package cli

import (
	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// AgentCmd opens the agentic REPL. Implemented in internal/agent.
func AgentCmd(reg *toolkit.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "agent",
		Short: "Open the AI SRE terminal (interactive REPL)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgent(cmd, reg)
		},
	}
}

// AskCmd is a one-shot agent invocation.
func AskCmd(reg *toolkit.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "ask <question>",
		Short: "Ask the agent a one-shot question",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAsk(cmd, reg, args)
		},
	}
}
