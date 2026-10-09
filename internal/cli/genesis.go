package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/networktool"
)

func genesisCmd(reg *toolkit.Registry) *cobra.Command {
	cmd := &cobra.Command{Use: "genesis", Short: "Create a new network: keys on each validator, genesis, gentxs, start"}
	var vals, homeTmpl, keyring, mnemonicsTo, dockerNet, accountsFile string
	var portStep, portBase int
	var yes bool
	create := &cobra.Command{
		Use:   "create <chain-id>",
		Short: "Create a new network from its spec on the validators' hosts and start it",
		Long: `Runs node-setup's distributed genesis workflow with cometcli as the
coordinator. For each validator profile (its host, local or over SSH):
init in the image — node and consensus keys stay there — and the operator
key in the node's own keyring. Then a base genesis from the spec with every
validator funded (and --accounts), each validator's gentx signed on its own
host, collect + strict validate-genesis, the final genesis on every node,
configs rendered for the validator role with every other validator as a
peer, and every node started. Mnemonics are shown only here, or written to
--mnemonics-to files (0600) — never anywhere else.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			spec, _, err := netspec.Load(args[0])
			if err != nil {
				return err
			}
			for _, f := range spec.Review(false) {
				if f.Sev == netspec.Fail {
					return fmt.Errorf("the spec has a problem: %s", f.What)
				}
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			names := splitCSV(vals)
			if len(names) == 0 {
				return fmt.Errorf("--validators: the profiles of the genesis validators")
			}
			var profiles []*config.Profile
			hosts := map[string]int{}
			for _, n := range names {
				p, ok := cfg.Profiles[n]
				if !ok {
					return fmt.Errorf("no profile %s", n)
				}
				p.Name = n
				profiles = append(profiles, p)
				hosts[p.Transport.Host]++
			}
			homeFor := func(p *config.Profile) string {
				t := homeTmpl
				if t == "" {
					if spec.HostHome != "" && hosts[p.Transport.Host] == 1 {
						t = spec.HostHome
					} else {
						t = "~/.cometcli-nodes/{profile}"
					}
				}
				return strings.ReplaceAll(t, "{profile}", p.Name)
			}
			kr := networktool.KeyringOpts{Backend: keyring}
			if keyring != "test" && keyring != "file" {
				return fmt.Errorf("--keyring: test or file")
			}
			var extra []networktool.Account
			if accountsFile != "" {
				if extra, err = readAccounts(accountsFile); err != nil {
					return err
				}
			}

			fmt.Fprintf(out, "new network %s (%s, image %s)\n", spec.Chain.ID, spec.Chain.Denom, networktool.ImageFor(spec))
			for i, p := range profiles {
				fmt.Fprintf(out, "  validator %-12s %s  home %s  port offset %d\n", p.Name, hostOf(p), homeFor(p), portBase+i*portStep)
			}
			if len(extra) > 0 {
				fmt.Fprintf(out, "  + %d extra genesis accounts\n", len(extra))
			}
			if !yes && !confirm(cmd, "create keys on each validator's host, build the genesis and start the network?") {
				return fmt.Errorf("cancelled")
			}
			if keyring == "file" {
				pw, err := keyringPassword()
				if err != nil {
					return err
				}
				kr.Password = pw
			}

			ctxFor := func(p *config.Profile) *toolkit.Context {
				c := &toolkit.Context{Context: cmd.Context(), Profile: p, Cfg: cfg, Out: out,
					Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return true, nil }}
				c.AutoApproveBelow = toolkit.TierOnChain // confirmed once above; transactions still can't run here
				return c
			}
			if dockerNet != "" {
				c := ctxFor(profiles[0])
				h, err := c.Host()
				if err != nil {
					return err
				}
				_, _, _ = h.Run(c, "docker network inspect "+dockerNet+" >/dev/null 2>&1 || docker network create "+dockerNet+" >/dev/null")
				c.Close()
			}

			// 1. each validator, on its own host
			var nodes []*networktool.GenesisNode
			for _, p := range profiles {
				c := ctxFor(p)
				defer c.Close()
				n, mnemonic, err := networktool.PrepareValidator(c, spec, homeFor(p), kr)
				if err != nil {
					return err
				}
				nodes = append(nodes, n)
				fmt.Fprintf(out, "✓ %s: node %s, operator %s\n", p.Name, n.NodeID[:12], n.Account)
				if err := showMnemonic(out, mnemonicsTo, p.Name, n.Account, mnemonic); err != nil {
					return err
				}
			}
			// 2. base genesis, on the first validator's host
			base, err := networktool.BuildBase(nodes[0].Ctx, spec, nodes, extra)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "✓ base genesis: %d validators funded, %d extra accounts\n", len(nodes), len(extra))
			// 3. gentxs, each on its own host
			gentxs := map[string][]byte{}
			for _, n := range nodes {
				g, err := networktool.Gentx(n, spec, base, kr)
				if err != nil {
					return err
				}
				gentxs[n.Moniker] = g
			}
			fmt.Fprintf(out, "✓ %d gentxs signed on their hosts\n", len(gentxs))
			// 4. collect + strict validation
			final, err := networktool.Collect(nodes[0].Ctx, spec, gentxs)
			if err != nil {
				return err
			}
			gp, _ := networktool.GenesisPath(spec.Chain.ID)
			if err := os.WriteFile(gp, final, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(out, "✓ final genesis valid (%d bytes) → %s\n", len(final), gp)
			// 5. every node: the final genesis, its configs, start
			addrOf := func(i int) string {
				if dockerNet != "" {
					return unitOf(profiles[i])
				}
				return hostOf(profiles[i])
			}
			provision, _ := reg.Get("node.provision")
			for i, n := range nodes {
				h, _ := n.Ctx.Host()
				if err := h.WriteFile(n.Ctx, filepath.Join(n.Home, "config", "genesis.json"), final, 0o644); err != nil {
					return err
				}
				var peers []string
				for j, m := range nodes {
					if j != i {
						peers = append(peers, fmt.Sprintf("%s@%s:%d", m.NodeID, addrOf(j), 26656+portBaseFor(dockerNet, portBase, portStep, j)))
					}
				}
				pargs := toolkit.Args{"role": "validator", "network": spec.Chain.ID, "home": n.Home, "reconfigure": true,
					"peers": strings.Join(peers, ","), "port_offset": float64(portBase + i*portStep)}
				if dockerNet != "" {
					pargs["docker_network"] = dockerNet
				}
				if _, err := provision.Run(n.Ctx, pargs); err != nil {
					return fmt.Errorf("%s: %w", n.Moniker, err)
				}
				fmt.Fprintf(out, "✓ %s started\n", n.Moniker)
			}
			// 6. the chain produces blocks
			if w, ok := reg.Get("wait.until"); ok {
				c := ctxFor(profiles[0])
				c.Profile = cfg.Profiles[profiles[0].Name]
				c.Profile.Name = profiles[0].Name
				defer c.Close()
				res, err := w.Run(c, toolkit.Args{"condition": "height", "value": "3", "timeout": float64(180), "interval": float64(3)})
				if err != nil {
					return fmt.Errorf("the network didn't start producing blocks: %w", err)
				}
				fmt.Fprintln(out, res.Text)
			}
			fmt.Fprintf(out, "\nnetwork %s is up. Next: cometcli network check --network %s, and back up every validator's priv_validator_key.json off-machine.\n", spec.Chain.ID, spec.Chain.ID)
			return nil
		},
	}
	f := create.Flags()
	f.StringVar(&vals, "validators", "", "profiles of the genesis validators, comma-separated")
	f.StringVar(&homeTmpl, "home", "", "node home on each host; {profile} is replaced (default: the spec's host_home, or ~/.cometcli-nodes/{profile} when hosts are shared)")
	f.StringVar(&keyring, "keyring", "test", "operator keyring on the nodes: test (unencrypted, test networks) or file (password)")
	f.StringVar(&mnemonicsTo, "mnemonics-to", "", "write each operator mnemonic to <dir>/<profile>.mnemonic (0600) instead of showing it")
	f.StringVar(&accountsFile, "accounts", "", "extra genesis accounts: lines of '<address> <amount>' (bech32 or 0x)")
	f.StringVar(&dockerNet, "docker-network", "", "put every node on this docker network (several validators on one machine)")
	f.IntVar(&portBase, "port-offset", 0, "host port offset of the first validator")
	f.IntVar(&portStep, "port-step", 0, "host port offset added per further validator (several on one machine)")
	f.BoolVar(&yes, "yes", false, "don't ask for confirmation")
	cmd.AddCommand(create)
	return cmd
}

// portBaseFor is the port a peer listens on as seen by other peers: on a
// shared docker network the container port, otherwise the host port.
func portBaseFor(dockerNet string, base, step, i int) int {
	if dockerNet != "" {
		return 0
	}
	return base + i*step
}

func unitOf(p *config.Profile) string {
	if p.Service.Unit != "" {
		return p.Service.Unit
	}
	return "primium-validator"
}

func hostOf(p *config.Profile) string {
	if p.Transport.Type == "ssh" && p.Transport.Host != "" {
		return p.Transport.Host
	}
	return "local"
}

func splitCSV(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func confirm(cmd *cobra.Command, q string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", q)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// showMnemonic gives the operator their validator's mnemonic: on this
// terminal, or into a 0600 file — nowhere else.
func showMnemonic(out interface{ Write([]byte) (int, error) }, dir, name, account, mnemonic string) error {
	if mnemonic == "" {
		return nil
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		p := filepath.Join(dir, name+".mnemonic")
		if err := os.WriteFile(p, []byte(mnemonic+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(out, "  operator mnemonic → %s (0600)\n", p)
		return nil
	}
	fmt.Fprintf(out, "\n  %s operator %s — WRITE THIS DOWN, it is shown once:\n  %s\n\n", name, account, mnemonic)
	return nil
}

// readAccounts parses '<address> <amount>' lines.
func readAccounts(path string) ([]networktool.Account, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []networktool.Account
	for i, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(strings.SplitN(l, "#", 2)[0])
		if l == "" {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 {
			return nil, fmt.Errorf("%s:%d: want '<address> <amount>'", path, i+1)
		}
		if _, err := strconv.ParseFloat(strings.TrimRight(f[1], "abcdefghijklmnopqrstuvwxyz"), 64); err != nil {
			return nil, fmt.Errorf("%s:%d: amount %q", path, i+1, f[1])
		}
		out = append(out, networktool.Account{Address: f[0], Amount: strings.TrimRight(f[1], "abcdefghijklmnopqrstuvwxyz")})
	}
	return out, nil
}

// keyringPassword is the file keyring's password for the new operator
// keys: COMETCLI_CONTAINER_KEYRING_PASSWORD (what the container signer
// reads later too), else asked twice on the terminal — a typo here locks
// the keys.
func keyringPassword() (string, error) {
	pw := os.Getenv("COMETCLI_CONTAINER_KEYRING_PASSWORD")
	if pw == "" {
		var err error
		if pw, err = TTYSecret(nil, "Keyring password for the validators' operator keys (8+ characters)"); err != nil {
			return "", fmt.Errorf("%w — or set COMETCLI_CONTAINER_KEYRING_PASSWORD", err)
		}
		again, err := TTYSecret(nil, "Again")
		if err != nil {
			return "", err
		}
		if again != pw {
			return "", fmt.Errorf("the passwords don't match")
		}
	}
	if len(pw) < 8 {
		return "", fmt.Errorf("the file keyring needs a password of at least 8 characters")
	}
	return pw, nil
}
