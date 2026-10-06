package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/audit"
)

// auditSessionsCmd lists agent sessions recorded in today's (or --all) logs.
func auditSessionsCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List recorded agent sessions (replayable with `audit replay <id>`)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			files, err := audit.List()
			if err != nil {
				return err
			}
			if !all && len(files) > 1 {
				files = files[:1]
			}
			out := cmd.OutOrStdout()
			n := 0
			for _, f := range files {
				ss, err := audit.Sessions(f)
				if err != nil {
					return err
				}
				for _, s := range ss {
					first := strings.ReplaceAll(s[1], "\n", " ")
					if len(first) > 70 {
						first = first[:70] + "…"
					}
					fmt.Fprintf(out, "%s  %s\n", s[0], first)
					n++
				}
			}
			if n == 0 {
				fmt.Fprintln(out, "no agent sessions recorded")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "scan every audit file, not just today's")
	return cmd
}

// auditReplayCmd reproduces a recorded agent session deterministically:
// recorded LLM turns and tool outputs are fed back through the real agent
// loop. No LLM call, no node access.
func auditReplayCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "replay <session-id>",
		Short: "Replay a recorded agent session deterministically (offline, no node access)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			evs, err := audit.ReadSession(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			pr := &agent.Printer{Out: out}
			rep, err := agent.Replay(cmd.Context(), evs, func(e agent.Event) {
				if e.Kind == "prompt" {
					fmt.Fprintf(out, "\n❯ %s\n", e.Text)
					return
				}
				pr.Handle(e)
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "\n— replayed session %s (profile %s): %d prompt(s), %d LLM turn(s), %d tool call(s)\n",
				rep.Session, rep.Profile, rep.Prompts, rep.Rounds, rep.Tools)
			for _, p := range rep.Problems {
				fmt.Fprintf(out, "  ! %s\n", p)
			}
			if len(rep.Problems) > 0 {
				return fmt.Errorf("replay diverged from the recording (%d problem(s))", len(rep.Problems))
			}
			return nil
		},
	}
}
