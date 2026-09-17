package cli

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tui"
)

// UICmd is the multi-pane terminal app: overview, fleet, logs, and a
// read-only tool runner — all fanning out over the same registry as the CLI.
func UICmd(reg *toolkit.Registry) *cobra.Command {
	var interval int
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Full-screen terminal app (overview, fleet, logs, tools)",
		RunE: func(cmd *cobra.Command, _ []string) error {
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
