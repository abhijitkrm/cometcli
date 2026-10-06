package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/agent"
)

// sessionsCmd lists saved agent sessions for --resume.
func sessionsCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List saved agent sessions (resume with -r <id> or -c)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := agent.ListSessions()
			if err != nil {
				return err
			}
			if flagJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(list)
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no saved sessions")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tUPDATED\tPROFILE\tMODEL\tTITLE")
			for i, s := range list {
				if i == 20 && !all {
					fmt.Fprintf(tw, "… %d more (--all)\t\t\t\t\n", len(list)-20)
					break
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Updated.Local().Format("Jan 02 15:04"), s.Profile, s.Model, s.Title)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "list every session")
	return cmd
}
