package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func runAgentImpl(cmd *cobra.Command, _ *toolkit.Registry, oneshot string) error {
	if oneshot != "" {
		fmt.Fprintln(cmd.OutOrStdout(), "agent mode lands in milestone M4 — for now use the deterministic subcommands")
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), "cometcli agent — interactive SRE terminal (M4)")
	return nil
}
