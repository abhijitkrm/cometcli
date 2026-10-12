package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/networktool"
)

// Workflow B: a genesis with validators the coordinator can't reach (see
// networktool/workflowb.go). Validator-side commands run on this machine,
// or on --profile's host.

// keyMachine is the context for a validator-side step: --profile's host,
// or this machine.
func keyMachine(cmd *cobra.Command, profile string) (*toolkit.Context, *config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	p := &config.Profile{Name: "local", Transport: config.Transport{Type: "local"}}
	if profile != "" {
		pp, ok := cfg.Profiles[profile]
		if !ok {
			return nil, nil, fmt.Errorf("no profile %s", profile)
		}
		pp.Name = profile
		p = pp
	}
	c := &toolkit.Context{Context: cmd.Context(), Profile: p, Cfg: cfg, Out: cmd.OutOrStdout(),
		Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return true, nil }}
	c.AutoApproveBelow = toolkit.TierOnChain
	return c, cfg, nil
}

func keyringFor(backend string) (networktool.KeyringOpts, error) {
	kr := networktool.KeyringOpts{Backend: backend}
	switch backend {
	case "file":
		pw, err := keyringPassword()
		if err != nil {
			return kr, err
		}
		kr.Password = pw
	case "test":
	default:
		return kr, fmt.Errorf("--keyring: file (password, the default) or test (unencrypted, test networks only)")
	}
	return kr, nil
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func offlineNote(o networktool.Offline) string {
	if o.Online {
		return "made on a CONNECTED machine (--allow-online): " + o.Detail
	}
	return "made with no network"
}

const backupReminder = `back up, off this machine: the node home's config/priv_validator_key.json and the operator mnemonic.
never run this key on two machines at once — that is a double sign, and the validator is tombstoned.`

// coordinator state between genesis base and collect: the validators the
// coordinator runs itself, and where the base was built
type coordState struct {
	ChainID string     `json:"chain_id"`
	Builder string     `json:"builder"`
	Keyring string     `json:"keyring"` // of your own validators' operator keys
	Own     []ownState `json:"own"`
}

type ownState struct {
	Profile    string `json:"profile"`
	Moniker    string `json:"moniker"`
	Home       string `json:"home"`
	User       string `json:"user"`
	PortOffset int    `json:"port_offset"`
}

func coordPath(chainID string) (string, error) {
	p, err := netspec.Path(chainID)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(p, ".yaml") + ".coordinator.json", nil
}

func genesisBCmds() []*cobra.Command {
	var (
		profile, home, moniker, ip, recoverFrom, mnemonicTo, keyring, out string
		p2pPort                                                           int
		allowOnline                                                       bool
	)
	join := &cobra.Command{
		Use:   "join <chain-id | spec.yaml>",
		Short: "Validator: make this node's keys (offline) and the join file for the coordinator",
		Long: `Run on the validator's own machine — with no network if you can (load the
image first: host prepare --offline_ready). Makes the node and consensus
keys and the operator key in the node's own keyring, inside a container
with no network. A new operator mnemonic goes to a 0600 file on this
machine (--mnemonic-to), never to the screen. Writes <moniker>.join.json:
the account, node id and consensus public key — nothing secret.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			spec, _, err := netspec.Load(args[0])
			if err != nil {
				return err
			}
			if moniker == "" || home == "" {
				return fmt.Errorf("--moniker and --home are required")
			}
			c, _, err := keyMachine(cmd, profile)
			if err != nil {
				return err
			}
			defer c.Close()
			if ip == "" && c.Profile.Transport.Type == "ssh" {
				ip = host.Resolve(c.Profile.Transport).HostName
			}
			if ip == "" {
				return fmt.Errorf("--ip: the public IP other validators will dial")
			}
			kr, err := keyringFor(keyring)
			if err != nil {
				return err
			}
			j, mfile, err := networktool.PrepareJoin(c, spec, networktool.JoinOpts{Home: home, Moniker: moniker, IP: ip, P2PPort: p2pPort,
				Keyring: kr, RecoverFrom: recoverFrom, MnemonicTo: mnemonicTo, AllowOnline: allowOnline})
			if err != nil {
				return err
			}
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			f := filepath.Join(out, j.Moniker+".join.json")
			if err := writeJSON(f, j); err != nil {
				return err
			}
			fmt.Fprintf(w, "✓ %s: node %s, operator %s — keys %s\n", j.Moniker, j.NodeID[:12], j.Account, offlineNote(j.Offline))
			if mfile != "" {
				fmt.Fprintf(w, "✓ operator mnemonic → %s on %s (0600) — never shown\n", mfile, c.Profile.Name)
			}
			fmt.Fprintf(w, "→ send %s to the coordinator\n\n%s\n", f, backupReminder)
			return nil
		},
	}
	jf := join.Flags()
	jf.StringVar(&profile, "profile", "", "make the keys on this profile's host instead of this machine")
	jf.StringVar(&home, "home", "", "the new node home")
	jf.StringVar(&moniker, "moniker", "", "this validator's moniker")
	jf.StringVar(&ip, "ip", "", "public IP peers dial (default: the profile's SSH host)")
	jf.IntVar(&p2pPort, "p2p-port", 26656, "public P2P port")
	jf.StringVar(&recoverFrom, "recover-from", "", "recover the operator key from a mnemonic file on the key machine (else a new key)")
	jf.StringVar(&mnemonicTo, "mnemonic-to", "", "where a new mnemonic goes on the key machine (default <home>.operator.mnemonic)")
	jf.StringVar(&keyring, "keyring", "file", "operator keyring: file (password) or test (unencrypted, test networks)")
	jf.BoolVar(&allowOnline, "allow-online", false, "make keys on a connected machine (marked in the join file)")
	jf.StringVar(&out, "out", ".", "directory for the join file")

	var (
		own, builder, kitDir, accountsFile, homeTmpl, mnemonicsTo, bkeyring string
		portOffset                                                          int
		yes                                                                 bool
	)
	base := &cobra.Command{
		Use:   "base <chain-id> [join files…]",
		Short: "Coordinator: base genesis funding every validator, written as a kit to hand out",
		Long: `Builds the base genesis from the network spec, funding every validator in
the join files (and --own validators, whose keys cometcli makes on their
hosts, as genesis create does), plus --accounts. Writes the kit: the base
genesis, the spec, kit.json and a README, all checksummed. Hand the kit
to every outside validator; they sign with genesis gentx --kit.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			spec, specPath, err := netspec.Load(args[0])
			if err != nil {
				return err
			}
			for _, f := range spec.Review(false) {
				if f.Sev == netspec.Fail {
					return fmt.Errorf("the spec has a problem: %s", f.What)
				}
			}
			if kitDir == "" {
				kitDir = spec.Chain.ID + "-kit"
			}
			var joins []*networktool.Join
			for _, f := range args[1:] {
				j, err := networktool.ReadJoin(f, spec)
				if err != nil {
					return err
				}
				joins = append(joins, j)
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			var ownProfiles []*config.Profile
			for _, n := range splitCSV(own) {
				p, ok := cfg.Profiles[n]
				if !ok {
					return fmt.Errorf("no profile %s", n)
				}
				p.Name = n
				ownProfiles = append(ownProfiles, p)
			}
			if builder == "" && len(ownProfiles) > 0 {
				builder = ownProfiles[0].Name
			}
			if builder == "" {
				return fmt.Errorf("--builder: a profile whose host has docker and the image builds the base genesis")
			}
			if len(joins)+len(ownProfiles) == 0 {
				return fmt.Errorf("no validators: pass join files and/or --own profiles")
			}
			var extra []networktool.Account
			if accountsFile != "" {
				if extra, err = readAccounts(accountsFile); err != nil {
					return err
				}
			}
			fmt.Fprintf(w, "base genesis for %s: %d outside validators, %d of yours (%s), builder %s\n", spec.Chain.ID, len(joins), len(ownProfiles), strings.Join(splitCSV(own), ","), builder)
			if !yes && !confirm(cmd, "make the keys of your validators on their hosts and build the base genesis?") {
				return fmt.Errorf("cancelled")
			}
			ctxFor := func(p *config.Profile) *toolkit.Context {
				c := &toolkit.Context{Context: cmd.Context(), Profile: p, Cfg: cfg, Out: w,
					Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return true, nil }}
				c.AutoApproveBelow = toolkit.TierOnChain
				return c
			}
			kit := &networktool.Kit{Version: networktool.FormatVersion, ChainID: spec.Chain.ID, Denom: spec.Chain.Denom,
				Stake: spec.Genesis.InitialStake, Image: networktool.ImageFor(spec), Created: nowUTC()}
			var nodes []*networktool.GenesisNode
			state := coordState{ChainID: spec.Chain.ID, Builder: builder, Keyring: bkeyring}
			// your validators: keys made where they stay
			if len(ownProfiles) > 0 {
				var kr networktool.KeyringOpts
				if kr, err = keyringFor(bkeyring); err != nil {
					return err
				}
				for _, p := range ownProfiles {
					c := ctxFor(p)
					defer c.Close()
					h := homeTmpl
					if h == "" {
						h = "~/.cometcli-nodes/" + spec.Chain.ID + "-{profile}"
					}
					h = strings.ReplaceAll(h, "{profile}", p.Name)
					n, mnemonic, err := networktool.PrepareValidator(c, spec, h, kr)
					if err != nil {
						return fmt.Errorf("%s: %w", p.Name, err)
					}
					if err := showMnemonic(w, mnemonicsTo, p.Name, n.Account, mnemonic); err != nil {
						return err
					}
					addr := host.Resolve(p.Transport).HostName + ":" + strconv.Itoa(26656+portOffset)
					kit.Validators = append(kit.Validators, networktool.KitValidator{Moniker: n.Moniker, Account: n.Account, NodeID: n.NodeID, Address: addr})
					nodes = append(nodes, n)
					state.Own = append(state.Own, ownState{Profile: p.Name, Moniker: n.Moniker, Home: n.Home, User: n.User, PortOffset: portOffset})
					fmt.Fprintf(w, "✓ %s (yours): node %s, operator %s\n", p.Name, n.NodeID[:12], n.Account)
				}
			}
			for _, j := range joins {
				kit.Validators = append(kit.Validators, networktool.KitValidator{Moniker: j.Moniker, Account: j.Account, NodeID: j.NodeID,
					ConsKey: j.ConsKey, Address: fmt.Sprintf("%s:%d", j.IP, j.P2PPort), Offline: !j.Offline.Online && j.Offline.Checked})
				nodes = append(nodes, &networktool.GenesisNode{Moniker: j.Moniker, Account: j.Account, NodeID: j.NodeID})
				fmt.Fprintf(w, "✓ %s (outside): node %s, operator %s — keys %s\n", j.Moniker, j.NodeID[:12], j.Account, offlineNote(j.Offline))
			}
			if err := uniqueValidators(kit.Validators); err != nil {
				return err
			}
			bp, ok := cfg.Profiles[builder]
			if !ok {
				return fmt.Errorf("no profile %s", builder)
			}
			bp.Name = builder
			bc := ctxFor(bp)
			defer bc.Close()
			gen, err := networktool.BuildBase(bc, spec, nodes, extra)
			if err != nil {
				return err
			}
			id, err := networktool.WriteKit(kitDir, kit, gen, specPath)
			if err != nil {
				return err
			}
			cp, err := coordPath(spec.Chain.ID)
			if err != nil {
				return err
			}
			if err := writeJSON(cp, state); err != nil {
				return err
			}
			fmt.Fprintf(w, "✓ base genesis: %d validators funded, %d extra accounts\n✓ kit → %s (id %.16s…)\n", len(kit.Validators), len(extra), kitDir, id)
			fmt.Fprintf(w, "→ send the kit to each outside validator: cometcli genesis gentx --kit <kit> --home <their node home>\n")
			fmt.Fprintf(w, "→ then: cometcli genesis collect --kit %s <their .gentx.json files>\n", kitDir)
			return nil
		},
	}
	bf := base.Flags()
	bf.StringVar(&own, "own", "", "profiles of validators you run (keys made on their hosts), comma-separated")
	bf.StringVar(&builder, "builder", "", "profile whose host builds the base genesis (default: the first --own)")
	bf.StringVar(&kitDir, "kit", "", "kit directory to write (default <chain-id>-kit)")
	bf.StringVar(&accountsFile, "accounts", "", "extra genesis accounts: lines of '<address> <amount>'")
	bf.StringVar(&homeTmpl, "home", "", "node home of --own validators; {profile} is replaced (default ~/.cometcli-nodes/<chain>-{profile})")
	bf.StringVar(&mnemonicsTo, "mnemonics-to", "", "write --own operator mnemonics to <dir>/<profile>.mnemonic (0600) instead of showing them")
	bf.StringVar(&bkeyring, "keyring", "file", "operator keyring of --own validators: file or test")
	bf.IntVar(&portOffset, "port-offset", 0, "host port offset of --own validators (beside another network)")
	bf.BoolVar(&yes, "yes", false, "don't ask for confirmation")

	var gprofile, ghome, gmoniker, gkit, gkeyring, gout string
	var gallow bool
	gentx := &cobra.Command{
		Use:   "gentx --kit <dir> --home <node home>",
		Short: "Validator: sign this node's gentx against the coordinator's kit (offline)",
		Long: `Verifies the kit's checksums, checks this home is the node the kit lists,
then signs the gentx in a container with no network. Writes
<moniker>.gentx.json — the gentx and public node info, nothing secret —
which is the only file to send back.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			k, base, spec, id, err := networktool.ReadKit(gkit)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "✓ kit %s verified (id %.16s…, %d validators)\n", k.ChainID, id, len(k.Validators))
			c, _, err := keyMachine(cmd, gprofile)
			if err != nil {
				return err
			}
			defer c.Close()
			if gmoniker == "" {
				gmoniker = c.Profile.Name
			}
			kr, err := keyringFor(gkeyring)
			if err != nil {
				return err
			}
			r, err := networktool.SignGentx(c, k, id, base, spec, ghome, gmoniker, kr, gallow)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(gout, 0o755); err != nil {
				return err
			}
			f := filepath.Join(gout, r.Moniker+".gentx.json")
			if err := writeJSON(f, r); err != nil {
				return err
			}
			fmt.Fprintf(w, "✓ %s gentx signed — %s\n→ send %s back to the coordinator\n", r.Moniker, offlineNote(r.Offline), f)
			return nil
		},
	}
	gf := gentx.Flags()
	gf.StringVar(&gkit, "kit", "", "the kit directory from the coordinator")
	gf.StringVar(&ghome, "home", "", "this validator's node home (from genesis join)")
	gf.StringVar(&gmoniker, "moniker", "", "this validator's moniker (default: the profile name)")
	gf.StringVar(&gprofile, "profile", "", "sign on this profile's host instead of this machine")
	gf.StringVar(&gkeyring, "keyring", "file", "operator keyring: file or test")
	gf.BoolVar(&gallow, "allow-online", false, "sign on a connected machine (marked in the gentx file)")
	gf.StringVar(&gout, "out", ".", "directory for the gentx file")

	var ckit, cpkgs string
	var cnoStart, callowMissing bool
	collect := &cobra.Command{
		Use:   "collect --kit <dir> [gentx files…]",
		Short: "Coordinator: verify the gentxs, build the final genesis, write each validator's package",
		Long: `Checks every returned gentx against the kit before using it: same kit,
a validator in it, the agreed stake, the account the kit funds, the
consensus key from its join file, its node id. Your own validators sign
on their hosts. Then collect-gentxs and strict validate-genesis, and one
package per validator: the final genesis, the spec, and every other
validator as a peer. Your own validators are installed and started.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			k, base, spec, id, err := networktool.ReadKit(ckit)
			if err != nil {
				return err
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			var st coordState
			if cp, err := coordPath(k.ChainID); err == nil {
				if raw, err := os.ReadFile(cp); err == nil {
					_ = json.Unmarshal(raw, &st)
				}
			}
			if st.Builder == "" {
				return fmt.Errorf("no coordinator state for %s — run genesis collect where genesis base ran", k.ChainID)
			}
			ctxFor := func(name string) (*toolkit.Context, error) {
				p, ok := cfg.Profiles[name]
				if !ok {
					return nil, fmt.Errorf("no profile %s", name)
				}
				p.Name = name
				c := &toolkit.Context{Context: cmd.Context(), Profile: p, Cfg: cfg, Out: w,
					Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return true, nil }}
				c.AutoApproveBelow = toolkit.TierOnChain
				return c, nil
			}
			gentxs := map[string][]byte{}
			got := map[string]bool{}
			for _, f := range args {
				raw, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				var r networktool.GentxReturn
				if err := json.Unmarshal(raw, &r); err != nil {
					return fmt.Errorf("%s: %w", f, err)
				}
				if err := networktool.CheckReturn(k, id, &r); err != nil {
					return fmt.Errorf("%s rejected: %w", f, err)
				}
				if got[r.Moniker] {
					return fmt.Errorf("two gentxs for %s", r.Moniker)
				}
				got[r.Moniker] = true
				gentxs[r.Moniker] = r.Gentx
				fmt.Fprintf(w, "✓ %s: gentx checked — %s\n", r.Moniker, offlineNote(r.Offline))
			}
			if len(st.Own) > 0 {
				kr, err := keyringFor(st.Keyring)
				if err != nil {
					return err
				}
				for _, o := range st.Own {
					c, err := ctxFor(o.Profile)
					if err != nil {
						return err
					}
					defer c.Close()
					var me networktool.KitValidator
					for _, v := range k.Validators {
						if v.Moniker == o.Moniker {
							me = v
						}
					}
					n := &networktool.GenesisNode{Ctx: c, Home: o.Home, User: o.User, Moniker: o.Moniker, Account: me.Account, NodeID: me.NodeID, Image: k.Image}
					g, err := networktool.GentxAt(n, spec, base, kr, me.Address)
					if err != nil {
						return err
					}
					gentxs[o.Moniker] = g
					got[o.Moniker] = true
					fmt.Fprintf(w, "✓ %s (yours): gentx signed on its host\n", o.Moniker)
				}
			}
			var missing []string
			for _, v := range k.Validators {
				if !got[v.Moniker] {
					missing = append(missing, v.Moniker)
				}
			}
			if len(missing) > 0 && !callowMissing {
				return fmt.Errorf("no gentx yet from %s — collect when all are in, or --allow-missing to launch without them (their accounts stay funded)", strings.Join(missing, ", "))
			}
			bc, err := ctxFor(st.Builder)
			if err != nil {
				return err
			}
			defer bc.Close()
			final, err := networktool.CollectAt(bc, spec, base, gentxs)
			if err != nil {
				return err
			}
			gp, err := networktool.GenesisPath(k.ChainID)
			if err != nil {
				return err
			}
			if err := os.WriteFile(gp, final, 0o600); err != nil {
				return err
			}
			// the spec beside it, so provision and network check know the network
			if sp, err := netspec.Path(k.ChainID); err == nil {
				if _, err := os.Stat(sp); err != nil {
					raw, _ := os.ReadFile(filepath.Join(ckit, "spec.yaml"))
					_ = os.WriteFile(sp, raw, 0o644)
				}
			}
			fmt.Fprintf(w, "✓ final genesis valid (%d bytes, %d gentxs) → %s\n", len(final), len(gentxs), gp)
			if cpkgs == "" {
				cpkgs = k.ChainID + "-packages"
			}
			pk, err := networktool.WritePackages(cpkgs, k, final, filepath.Join(ckit, "spec.yaml"))
			if err != nil {
				return err
			}
			for _, o := range st.Own {
				if cnoStart {
					break
				}
				c, err := ctxFor(o.Profile)
				if err != nil {
					return err
				}
				defer c.Close()
				p, gen, sp, err := networktool.ReadPackage(pk[o.Moniker])
				if err != nil {
					return err
				}
				if _, err := networktool.InstallPackage(c, p, gen, sp, o.Home, o.PortOffset, ""); err != nil {
					return fmt.Errorf("%s: %w", o.Moniker, err)
				}
				fmt.Fprintf(w, "✓ %s (yours) started\n", o.Moniker)
			}
			fmt.Fprintf(w, "packages → %s/<moniker>/ — send each outside validator theirs: cometcli genesis install --package <dir> --home <node home>\n", cpkgs)
			return nil
		},
	}
	cf := collect.Flags()
	cf.StringVar(&ckit, "kit", "", "the kit genesis base wrote")
	cf.StringVar(&cpkgs, "packages", "", "directory for the packages (default <chain-id>-packages)")
	cf.BoolVar(&cnoStart, "no-start", false, "don't install and start your own validators")
	cf.BoolVar(&callowMissing, "allow-missing", false, "build the genesis without validators that haven't sent a gentx")

	var iprofile, ihome, ipkg, iip string
	var ioffset int
	install := &cobra.Command{
		Use:   "install --package <dir> --home <node home>",
		Short: "Validator: install the final genesis from your package and start the node",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			p, gen, sp, err := networktool.ReadPackage(ipkg)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "✓ package for %s on %s verified (genesis %.16s…, %d peers)\n", p.Moniker, p.ChainID, p.Genesis, len(p.Peers))
			c, _, err := keyMachine(cmd, iprofile)
			if err != nil {
				return err
			}
			defer c.Close()
			if c.Profile.Service.Unit == "" {
				c.Profile.Service.Unit = netspec.ContainerName(p.ChainID, netspec.Validator)
			}
			c.Progress = func(s string) { fmt.Fprintln(w, s) }
			res, err := networktool.InstallPackage(c, p, gen, sp, ihome, ioffset, iip)
			if err != nil {
				return err
			}
			_ = res
			fmt.Fprintf(w, "✓ %s started on %s\n", p.Moniker, p.ChainID)
			return nil
		},
	}
	inf := install.Flags()
	inf.StringVar(&ipkg, "package", "", "your package directory from the coordinator")
	inf.StringVar(&ihome, "home", "", "this validator's node home (from genesis join)")
	inf.StringVar(&iprofile, "profile", "", "install on this profile's host instead of this machine")
	inf.StringVar(&iip, "ip", "", "public IP to advertise (default: the profile's SSH host, or detected)")
	inf.IntVar(&ioffset, "port-offset", 0, "host port offset (beside another network)")

	return []*cobra.Command{join, base, gentx, collect, install}
}

func uniqueValidators(vs []networktool.KitValidator) error {
	seen := map[string]string{}
	for _, v := range vs {
		for kind, val := range map[string]string{"moniker": v.Moniker, "account": v.Account, "node id": v.NodeID, "consensus key": v.ConsKey} {
			if val == "" {
				continue
			}
			k := kind + "=" + val
			if who, ok := seen[k]; ok {
				return fmt.Errorf("%s and %s share a %s (%s) — every validator needs its own", who, v.Moniker, kind, val)
			}
			seen[k] = v.Moniker
		}
	}
	return nil
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

// networkRemoveCmd stops a network's nodes and deletes their data.
func networkRemoveCmd() *cobra.Command {
	var profiles string
	var keepProfiles, yes bool
	cmd := &cobra.Command{
		Use:   "remove <chain-id> --profiles a,b",
		Short: "Stop a network's nodes and delete their data (test networks): homes, builder files, profiles",
		Long: `For each profile: stops its container and deletes its node home — only
when that home's genesis is for <chain-id>, so another network's data is
never touched. Also deletes the chain's genesis builder files on those
hosts, and the profiles (unless --keep-profiles). Reports the space freed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			chain := args[0]
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			names := splitCSV(profiles)
			if len(names) == 0 {
				return fmt.Errorf("--profiles: the nodes of %s to remove", chain)
			}
			for _, n := range names {
				p, ok := cfg.Profiles[n]
				if !ok {
					return fmt.Errorf("no profile %s", n)
				}
				if p.ChainID != chain {
					return fmt.Errorf("profile %s is on %s, not %s — not removed", n, p.ChainID, chain)
				}
				fmt.Fprintf(w, "  %s: %s on %s (container %s)\n", n, orDash(p.Home), hostOf(p), orDash(p.Service.Unit))
			}
			if !yes && !confirm(cmd, fmt.Sprintf("stop these %s nodes and DELETE their homes (keys and data)?", chain)) {
				return fmt.Errorf("cancelled")
			}
			hosts := map[string]bool{}
			for _, n := range names {
				p := cfg.Profiles[n]
				p.Name = n
				c := &toolkit.Context{Context: cmd.Context(), Profile: p, Cfg: cfg}
				msg, err := networktool.RemoveNode(c, chain, p.Home, p.Service.Unit)
				if err != nil {
					c.Close()
					return fmt.Errorf("%s: %w", n, err)
				}
				fmt.Fprintf(w, "✓ %s: %s\n", n, msg)
				if hk := hostOf(p); !hosts[hk] {
					hosts[hk] = true
					if b := networktool.RemoveBuilder(c, chain); b != "" {
						fmt.Fprintf(w, "✓ %s: %s\n", n, b)
					}
					if h, err := c.Host(); err == nil {
						if r, _, _ := h.Run(c, `df -h "$HOME" | tail -1 | awk '{print $4 " free (" $5 " used)"}'`); r != "" {
							fmt.Fprintf(w, "  %s disk: %s", hk, r)
						}
					}
				}
				c.Close()
				if !keepProfiles {
					delete(cfg.Profiles, n)
				}
			}
			if !keepProfiles {
				if err := cfg.Save(); err != nil {
					return err
				}
				fmt.Fprintf(w, "✓ profiles removed: %s\n", strings.Join(names, ", "))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&profiles, "profiles", "", "the network's node profiles, comma-separated")
	f.BoolVar(&keepProfiles, "keep-profiles", false, "keep the profiles")
	f.BoolVar(&yes, "yes", false, "don't ask for confirmation")
	return cmd
}

// operatorKeyCmd makes a joining validator's operator key in its node's
// own keyring.
func operatorKeyCmd() *cobra.Command {
	var keyring, mnemonicTo, name string
	cmd := &cobra.Command{
		Use:   "operator-key",
		Short: "Make this validator's operator key in its node's own keyring (validators joining after genesis)",
		Long: `For a validator joining a running network (node provision, then this,
then fund the address and val create): creates the operator key inside
the node container's keyring. The mnemonic goes to a 0600 file on the
node's host (default <home>.operator.mnemonic) and is never shown; the
profile is set to sign there.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			c, _, err := keyMachine(cmd, flagProfile)
			if err != nil {
				return err
			}
			defer c.Close()
			if flagProfile == "" {
				return fmt.Errorf("--profile: the validator's profile")
			}
			if name == "" {
				name = c.Profile.Name
			}
			kr, err := keyringFor(keyring)
			if err != nil {
				return err
			}
			acc, mfile, err := networktool.OperatorKey(c, name, kr, mnemonicTo)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "✓ operator key %s → %s, in %s's keyring\n✓ mnemonic → %s on the node's host (0600) — never shown\n", name, acc, c.Profile.Service.Unit, mfile)
			fmt.Fprintf(w, "→ fund %s, then: cometcli --profile %s val create --amount <stake>\n\n%s\n", acc, c.Profile.Name, backupReminder)
			return nil
		},
	}
	cmd.Flags().StringVar(&keyring, "keyring", "file", "file (password) or test (unencrypted, test networks)")
	cmd.Flags().StringVar(&mnemonicTo, "mnemonic-to", "", "where the mnemonic goes on the node's host (0600)")
	cmd.Flags().StringVar(&name, "name", "", "key name (default: the profile name)")
	return cmd
}
