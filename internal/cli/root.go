// Package cli wires the tool registry into cobra commands. Every tool
// becomes `cometcli <domain> <verb>` with flags auto-generated from its
// JSON schema — the same schema the agent uses.
package cli

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

var (
	flagProfile string
	flagJSON    bool
	flagYes     bool
)

// NewRoot builds the root command and registers all subcommands.
func NewRoot(reg *toolkit.Registry, extra []*cobra.Command) *cobra.Command {
	root := &cobra.Command{
		Use:   "cometcli",
		Short: "Agentic SRE terminal for Cosmos-EVM validators",
		Long: `cometcli — an agentic SRE terminal for Cosmos-EVM validators.

Every capability is a deterministic subcommand (cometcli val status,
cometcli doctor, cometcli tx unjail) AND a tool the agent can call.
Run 'cometcli agent' for the AI SRE, or 'cometcli ask "..."' for
one-shot questions.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&flagProfile, "profile", "", "profile to use (env COMETCLI_PROFILE)")
	pf.BoolVar(&flagJSON, "json", false, "emit structured JSON")
	pf.BoolVarP(&flagYes, "yes", "y", false, "auto-approve observe/diagnose prompts (never on-chain)")

	for _, c := range toolGroupCommands(reg) {
		root.AddCommand(c)
	}
	root.AddCommand(profileCmd(), auditCmd(), versionCmd(), initCmd())
	root.AddCommand(extra...)
	return root
}

// toolGroupCommands organizes tools into <domain> parent commands.
func toolGroupCommands(reg *toolkit.Registry) []*cobra.Command {
	groups := map[string]*cobra.Command{}
	for _, t := range reg.All() {
		domain, _, ok := strings.Cut(t.Name(), ".")
		if !ok {
			continue
		}
		parent, ok := groups[domain]
		if !ok {
			parent = &cobra.Command{Use: domain, Short: domain + " operations"}
			groups[domain] = parent
		}
		parent.AddCommand(toolCmd(t))
	}
	preferred := []string{"node", "val", "chain", "evm", "keys", "tx", "upgrade", "snap", "mon", "sec", "net", "runbook", "fleet"}
	var out []*cobra.Command
	for _, d := range preferred {
		if p, ok := groups[d]; ok {
			out = append(out, p)
			delete(groups, d)
		}
	}
	var rest []string
	for d := range groups {
		rest = append(rest, d)
	}
	sort.Strings(rest)
	for _, d := range rest {
		out = append(out, groups[d])
	}
	return out
}

// toolCmd generates a cobra command whose flags mirror the tool schema.
func toolCmd(t toolkit.Tool) *cobra.Command {
	verb := t.Name()[strings.Index(t.Name(), ".")+1:]
	cmd := &cobra.Command{
		Use:   verb,
		Short: fmt.Sprintf("[%s] %s", t.Tier(), t.Desc()),
		RunE: func(cmd *cobra.Command, pos []string) error {
			args := argsFromFlags(cmd)
			// Positional args bind to required schema fields in order —
			// `cometcli tx get <hash>` works like `evmd q tx <hash>`.
			req, _ := t.Schema()["required"].([]string)
			for i, name := range req {
				if i < len(pos) && args[name] == nil {
					args[name] = pos[i]
				}
			}
			return RunTool(cmd, t, args)
		},
	}
	props, _ := t.Schema()["properties"].(map[string]any)
	var names []string
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		p, _ := props[name].(map[string]any)
		desc, _ := p["description"].(string)
		switch p["type"] {
		case "boolean":
			cmd.Flags().Bool(name, false, desc)
		case "integer":
			cmd.Flags().Int64(name, 0, desc)
		default:
			cmd.Flags().String(name, "", desc)
		}
	}
	return cmd
}

// argsFromFlags maps explicitly-set flags into tool Args.
func argsFromFlags(cmd *cobra.Command) toolkit.Args {
	args := toolkit.Args{}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		switch f.Value.Type() {
		case "bool":
			v, _ := cmd.Flags().GetBool(f.Name)
			args[f.Name] = v
		case "int64":
			v, _ := cmd.Flags().GetInt64(f.Name)
			args[f.Name] = v
		default:
			args[f.Name] = f.Value.String()
		}
	})
	return args
}

// RunTool executes a tool with standard output/audit handling. Exported so
// hand-written commands (doctor, agent) can drive tools too.
func RunTool(cmd *cobra.Command, t toolkit.Tool, args toolkit.Args) error {
	c, err := NewCtx(cmd, !strings.HasPrefix(t.Name(), "keys."))
	if err != nil {
		return err
	}
	defer c.Close()
	if !toolkit.IsLongRunning(t) {
		sub, cancel := toolkit.WithDeadline(c, 90*time.Second)
		defer cancel()
		defer sub.Close()
		c = sub
	}
	res, err := t.Run(c, args)
	profile := ""
	if c.Profile != nil {
		profile = c.Profile.Name
	}
	var data map[string]any
	if res != nil {
		data = res.Data
	}
	if c.Audit != nil {
		c.Audit.Tool(profile, t.Name(), t.Tier().String(), args, data, err)
	}
	if err != nil {
		return err
	}
	if flagJSON {
		fmt.Fprintln(c.Out, res.JSON())
	} else if res.Text != "" {
		fmt.Fprintln(c.Out, res.Text)
	}
	return nil
}

// NewCtx builds a toolkit.Context for one command invocation.
// requireProfile=false tolerates a missing profile (e.g. keys commands).
func NewCtx(cmd *cobra.Command, requireProfile bool) (*toolkit.Context, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	prof, err := cfg.ActiveProfile(flagProfile)
	if err != nil && requireProfile {
		return nil, err
	}
	var lg *audit.Logger
	if prof != nil {
		lg, _ = audit.Open(prof.Name)
	}
	c := &toolkit.Context{
		Context: cmd.Context(),
		Profile: prof,
		Cfg:     cfg,
		Out:     cmd.OutOrStdout(),
		Audit:   lg,
	}
	if flagYes {
		c.AutoApproveBelow = toolkit.TierLocalChange
	}
	c.Approver = StdinApprover(cmd.InOrStdin())
	return c, nil
}

// StdinApprover prompts y/N on the terminal.
func StdinApprover(in io.Reader) toolkit.Approver {
	reader := bufio.NewReader(in)
	return func(c *toolkit.Context, prompt string, tier toolkit.Tier, detail map[string]any) (bool, error) {
		fmt.Fprintf(c.Out, "\n⚠  [%s] %s\n", tier, prompt)
		for k, v := range detail {
			fmt.Fprintf(c.Out, "   %s: %v\n", k, v)
		}
		fmt.Fprint(c.Out, "Proceed? [y/N] ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return false, err
		}
		return strings.EqualFold(strings.TrimSpace(line), "y"), nil
	}
}
