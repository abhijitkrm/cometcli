package cli

import (
	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/mcp"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// MCPCmd serves the tool registry over the Model Context Protocol on
// stdio. Point any MCP client at `cometcli mcp`:
//
//	"cometcli": { "command": "cometcli", "args": ["mcp"] }
//
// Observe/diagnose tools run freely; anything that would prompt (host
// mutations, on-chain txs) is denied — stdio has no human to approve.
func MCPCmd(reg *toolkit.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve cometcli tools over MCP (stdio) for external agents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := NewCtx(cmd, true)
			if err != nil {
				return err
			}
			defer c.Close()
			c.AutoApproveBelow = toolkit.TierLocalChange
			c.Approver = toolkit.DenyApprover
			return (&mcp.Server{
				Reg: reg, Ctx: c,
				Name: "cometcli", Version: Version,
			}).Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
