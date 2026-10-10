package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/drill"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func drillCmd(reg *toolkit.Registry) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drill",
		Short: "Incident drills: inject real faults on a throwaway docker network, score how the agent handles them",
	}
	logf := func(cmd *cobra.Command) func(string) {
		return func(s string) { fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", time.Now().Format("15:04:05"), s) }
	}

	var image, specName string
	var validators int
	tn := &cobra.Command{Use: "testnet", Short: "Create or remove the throwaway drill network"}
	up := &cobra.Command{
		Use:   "up",
		Short: "Create a local validator network in docker (fast slashing: 20-block window, 30s jail) with drill profiles — like your network with --spec",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			n := &drill.Net{Image: image, Validators: validators}
			if specName != "" {
				if n.Spec, _, err = netspec.Load(specName); err != nil {
					return err
				}
			}
			if err := n.Up(cmd.Context(), cfg, logf(cmd)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "drill network up: profiles drill0..drill%d on chain %s — run `cometcli drill run`; remove with `cometcli drill testnet down`\n", n.Validators-1, n.ChainID)
			if n.Spec != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "built from %s's spec (drill timing: 20-block window, 60s jail, 60s votes) — check it: cometcli network check --network %s\n", n.Spec.Chain.ID, n.ChainID)
			}
			return nil
		},
	}
	up.Flags().StringVar(&image, "image", "", "evmd docker image to run (required without --spec; with it, overrides the spec's image)")
	up.Flags().StringVar(&specName, "spec", "", "build the network like this one: its spec (chain id or file) — same image, genesis, configs; built with genesis create's steps")
	up.Flags().IntVar(&validators, "validators", 4, "validators (4 keeps the chain live with one node down)")
	down := &cobra.Command{
		Use:   "down",
		Short: "Remove the drill network, its data and its profiles",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if err := (&drill.Net{}).Down(cmd.Context(), cfg); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "drill network removed")
			return nil
		},
	}
	tn.AddCommand(up, down)

	list := &cobra.Command{
		Use:   "list",
		Short: "List the drill scenarios",
		Run: func(cmd *cobra.Command, _ []string) {
			for _, s := range drill.Scenarios {
				fmt.Fprintf(cmd.OutOrStdout(), "%-14s %s (expects %s)\n", s.Name, s.Desc, strings.Join(s.Expect, " | "))
			}
		},
	}

	var only []string
	var node int
	var model string
	run := &cobra.Command{
		Use:   "run",
		Short: "Run drills: inject each fault, let the agent work it with /incident, verify on the node, score",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			n := &drill.Net{}
			n.Defaults()
			if node < 0 {
				node = 3
			}
			p := cfg.Profiles[drill.Profile(node)]
			if !drill.IsDrill(p) {
				return fmt.Errorf("no drill profile %s — `cometcli drill testnet up --spec <network>` (or --image <evmd image>) first", drill.Profile(node))
			}
			p.Name = drill.Profile(node)
			factory := func(ctx context.Context, approve toolkit.Approver) (*agent.Agent, error) {
				lg, _ := audit.Open("drill")
				c := &toolkit.Context{Context: ctx, Cfg: cfg, Audit: lg, Approver: approve}
				a, err := agent.New(c, reg)
				if err != nil {
					return nil, err
				}
				if model != "" {
					a.Model = model
				}
				return a, nil
			}
			var results []drill.Result
			for _, s := range drill.Scenarios {
				if len(only) > 0 && !contains(only, s.Name) {
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "\n== %s on %s\n", s.Name, p.Name)
				e := &drill.Env{Net: n, Cfg: cfg, Reg: reg, Node: node, Profile: p}
				r := drill.RunScenario(cmd.Context(), e, s, factory, logf(cmd))
				results = append(results, r)
				fmt.Fprintf(cmd.OutOrStdout(), "   triage %s (%v) · fixed %v · %d steps · %d tokens · %s\n", r.TriageCase, r.TriageOK, r.Fixed, r.Steps, r.Tokens, r.Duration.Round(time.Second))
			}
			if len(results) == 0 {
				return fmt.Errorf("no scenarios matched — cometcli drill list")
			}
			path, _ := drill.Save(results)
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s\nresults: %s\n", drill.Scoreboard(results), path)
			return nil
		},
	}
	run.Flags().StringSliceVar(&only, "scenario", nil, "scenarios to run (default all)")
	run.Flags().IntVar(&node, "node", -1, "validator index to break (default the last)")
	run.Flags().StringVar(&model, "model", "", "model for the agent (default: your agent settings)")

	cmd.AddCommand(tn, list, run)
	return cmd
}
