package cli

import (
	"fmt"
	"os"

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
	note, err := applyAgentFlags(cmd, a)
	if err != nil {
		return err
	}
	if note != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), note)
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

// applyAgentFlags layers the agent and session flags over the profile's
// agent defaults, turns on session saving, and resumes a session when
// asked (--continue / --resume). Flags a command doesn't define read as
// unset. The returned note describes a resumed session.
func applyAgentFlags(cmd *cobra.Command, a *agent.Agent) (string, error) {
	if m, _ := cmd.Flags().GetString("mode"); m != "" {
		mode, err := agent.ParseMode(m)
		if err != nil {
			return "", err
		}
		a.Policy.Mode = mode
	}
	if safe, _ := cmd.Flags().GetBool("safe"); safe {
		a.Policy.Mode = agent.ModeReadOnly
	}
	if tiers, _ := cmd.Flags().GetStringSlice("autopilot"); len(tiers) > 0 {
		for _, t := range tiers {
			if err := a.Policy.SetAutopilot(t, true); err != nil {
				return "", err
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
	if m, _ := cmd.Flags().GetString("model"); m != "" {
		a.Model = m
	}
	if e, _ := cmd.Flags().GetString("effort"); e != "" {
		if err := agent.ValidEffort(e); err != nil {
			return "", err
		}
		a.Effort = e
	}
	if n, _ := cmd.Flags().GetInt("max-tokens"); n > 0 {
		a.MaxTokens = n
	}
	a.Persist = true
	return resumeSession(cmd, a)
}

// resumeSession loads the session named by --resume, or the latest one in
// this directory for --continue.
func resumeSession(cmd *cobra.Command, a *agent.Agent) (string, error) {
	id, _ := cmd.Flags().GetString("resume")
	cont, _ := cmd.Flags().GetBool("continue")
	var sf *agent.SessionFile
	var err error
	switch {
	case id != "":
		sf, err = agent.LoadSession(id)
	case cont:
		cwd, _ := os.Getwd()
		profile := ""
		if a.Ctx.Profile != nil {
			profile = a.Ctx.Profile.Name
		}
		sf, err = agent.LatestSession(cwd, profile)
	default:
		return "", nil
	}
	if err != nil {
		return "", err
	}
	a.Restore(sf)
	return fmt.Sprintf("resumed session %s — %s (%d messages)", sf.ID, sf.Title, len(sf.History)), nil
}
