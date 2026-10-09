package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/abhijitkrm/cometcli/internal/netspec"
)

func networkImportCmd() *cobra.Command {
	var naming, repo string
	var force bool
	cmd := &cobra.Command{
		Use:   "import <network-config.env> [more.env …]",
		Short: "Create a network spec from node-setup network-config.env files",
		Long: `Reads node-setup's network-config.env (the run-genesis one is the most
complete; run-validator / run-archive ones fill in the rest) and writes
~/.cometcli/networks/<chain-id>.yaml — the spec network check compares the
chain and every node against. Keys with no spec field are kept under extra.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var spec *netspec.Spec
			for _, f := range args {
				raw, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				s := netspec.FromEnv(netspec.ParseEnv(string(raw)))
				if spec == nil {
					spec = s
				} else {
					spec.Merge(s)
				}
			}
			if naming != "" {
				spec.Image.Naming = naming
			}
			if repo != "" {
				spec.Image.Repo = repo
			}
			if spec.Chain.ID == "" {
				return fmt.Errorf("no COSMOS_CHAIN_ID in %v", args)
			}
			if p, err := netspec.Path(spec.Chain.ID); err == nil && !force {
				if _, err := os.Stat(p); err == nil {
					return fmt.Errorf("%s exists — pass --force to replace it", p)
				}
			}
			p, err := spec.Save()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "network %s → %s (%d settings kept under extra)\n", spec.Chain.ID, p, len(spec.Extra))
			for _, f := range spec.Review(false) {
				fmt.Fprintf(out, "%-4s %s\n", f.Sev, f.What)
			}
			fmt.Fprintf(out, "next: cometcli network check --network %s [--prod]\n", spec.Chain.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&naming, "image-naming", "", "how images built from releases are named, e.g. primium-{tag}")
	cmd.Flags().StringVar(&repo, "repo", "", "official source repo (default cosmos/evm)")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing spec")
	return cmd
}

func networkShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <chain-id>",
		Short: "Print a network spec",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, p, err := netspec.Load(args[0])
			if err != nil {
				return err
			}
			raw, _ := yaml.Marshal(s)
			fmt.Fprintf(cmd.OutOrStdout(), "# %s\n%s", p, raw)
			return nil
		},
	}
}
