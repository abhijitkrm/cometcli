package cli

import (
	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// runAgent is implemented in the agent milestone; stubbed until the
// provider loop lands.
func runAgent(cmd *cobra.Command, reg *toolkit.Registry) error {
	return runAgentImpl(cmd, reg, "")
}

func runAsk(cmd *cobra.Command, reg *toolkit.Registry, args []string) error {
	q := args[0]
	for _, s := range args[1:] {
		q += " " + s
	}
	return runAgentImpl(cmd, reg, q)
}
