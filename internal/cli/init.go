package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/client/comet"
	"github.com/abhijitkrm/cometcli/internal/client/evm"
	"github.com/abhijitkrm/cometcli/internal/client/grpc"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/keys"
)

// initCmd is the interactive first-run wizard: probe a node's endpoints,
// derive what we can (chain-id, evm-chain-id, bech32 prefix, bond denom),
// and write a working profile.
func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Interactive setup wizard — probe a node and create a profile",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

func ask(r *bufio.Reader, out io.Writer, label, def string) string {
	if def != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func askYN(r *bufio.Reader, out io.Writer, label string, def bool) bool {
	hint := " [y/N]: "
	if def {
		hint = " [Y/n]: "
	}
	fmt.Fprint(out, label+hint)
	line, _ := r.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}

// endpointHost extracts the host from tcp://host:port / http://host:port / host:port.
func endpointHost(ep string) string {
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "tcp://"), "http://")
	ep = strings.TrimPrefix(ep, "https://")
	if h, _, err := net.SplitHostPort(ep); err == nil {
		return h
	}
	return ep
}

func runInit(ctx context.Context, in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	say := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }
	// note prints a one-line explanation above the next prompt.
	note := func(s string) { fmt.Fprintf(out, "  %s\n", s) }

	say("cometcli init — profile setup wizard\n")

	note("a short name for this node — used with --profile and in fleet views")
	name := ask(r, out, "profile name", "myval")
	var p config.Profile
	p.Name = name
	p.Role = "validator"

	// --- comet rpc: probe for chain-id + moniker ---
	note("the node's CometBFT RPC — for docker use the published host port (see `docker ps`)")
	cometEP := ask(r, out, "CometBFT RPC endpoint", "tcp://127.0.0.1:26657")
	p.Endpoints.Comet = cometEP
	host := endpointHost(cometEP)
	if cc, err := comet.New(cometEP); err == nil {
		if st, err := cc.Status(ctx); err == nil {
			p.ChainID = st.NodeInfo.Network
			mon := string(st.NodeInfo.Moniker)
			say("  ✓ chain-id %s, node %q, height %d",
				p.ChainID, mon, st.SyncInfo.LatestBlockHeight)
			if p.Name == "myval" && mon != "" {
				p.Name = mon
			}
			if p.ChainID == "" {
				p.ChainID = ask(r, out, "chain-id", "")
			}
		} else {
			say("  ! RPC probe failed: %v", err)
			note("the chain's network id, e.g. mychain-1 — from genesis or your chain's docs")
			p.ChainID = ask(r, out, "chain-id", "")
		}
	} else {
		p.ChainID = ask(r, out, "chain-id", "")
	}
	note("account address prefix — e.g. cosmos produces cosmos1... addresses")
	p.Bech32Prefix = ask(r, out, "bech32 prefix", "cosmos")

	// --- grpc: probe for bond denom + validator prefix ---
	note("Cosmos SDK gRPC port (default 9090) — powers staking/gov/bank queries")
	grpcEP := ask(r, out, "gRPC endpoint", net.JoinHostPort(host, "9090"))
	if grpcEP != "" {
		p.Endpoints.GRPC = grpcEP
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		gc, err := grpc.Dial(dctx, grpcEP)
		cancel()
		if err == nil {
			defer gc.Close()
			qctx, qc := context.WithTimeout(ctx, 8*time.Second)
			defer qc()
			if pr, err := gc.Staking.Params(qctx, &stakingv1beta1.QueryParamsRequest{}); err == nil && pr.Params != nil {
				say("  ✓ bond denom: %s (unbonding %s)", pr.Params.BondDenom, pr.Params.UnbondingTime)
				if p.Metadata == nil {
					p.Metadata = map[string]string{}
				}
				p.Metadata["fee_denom"] = pr.Params.BondDenom
			}
			if vr, err := gc.Staking.Validators(qctx, &stakingv1beta1.QueryValidatorsRequest{}); err == nil {
				for _, v := range vr.Validators {
					if i := strings.Index(v.OperatorAddress, "1"); i > 0 {
						prefix := v.OperatorAddress[:i]
						// valoper/valcons/valpub share the account prefix
						for _, suf := range []string{"valoper", "valcons", "valpub"} {
							prefix = strings.TrimSuffix(prefix, suf)
						}
						if p.Bech32Prefix == "cosmos" && prefix != "cosmos" {
							say("  ✓ bech32 prefix detected: %s", prefix)
							p.Bech32Prefix = prefix
						}
						break
					}
				}
			}
		} else {
			say("  ! gRPC probe failed: %v", err)
		}
	}

	// --- evm json-rpc: probe chain id ---
	note("Ethereum JSON-RPC port (default 8545) — enables evm commands; blank disables them")
	evmEP := ask(r, out, "EVM JSON-RPC endpoint (blank to skip)", fmt.Sprintf("http://%s:8545", host))
	if evmEP != "" {
		p.Endpoints.EVM = evmEP
		ectx, ec := context.WithTimeout(ctx, 5*time.Second)
		if id, err := evm.New(evmEP).ChainID(ectx); err == nil {
			p.EVMChainID = id
			say("  ✓ evm chain-id: %d", id)
		} else {
			say("  ! EVM probe failed: %v", err)
		}
		ec()
	}

	// --- host-plane ---
	note("the node daemon binary — usually evmd; for docker, the binary inside the container")
	p.Binary = ask(r, out, "node binary name", "evmd")
	note("the node's data dir as seen from THIS host — for docker, the mounted host path")
	p.Home = ask(r, out, "node home dir", filepath.Join("~", "."+p.Binary))
	note("local = node runs on this machine; ssh = manage a remote host")
	transport := ask(r, out, "host transport (local|ssh)", "local")
	p.Transport.Type = transport
	if transport == "ssh" {
		p.Transport.Host = ask(r, out, "ssh host", host)
		p.Transport.User = ask(r, out, "ssh user", "ops")
		if port := ask(r, out, "ssh port", "22"); port != "" {
			fmt.Sscanf(port, "%d", &p.Transport.Port)
		}
		note("private key for ssh auth — password-protected keys need ssh-agent")
		p.Transport.KeyFile = ask(r, out, "ssh key file", filepath.Join("~", ".ssh", "id_ed25519"))
	}

	// --- service supervision: detect best guess ---
	guess := detectService(&p)
	note("how the node process is supervised — picks the backend for start/stop/restart/logs")
	p.Service.Type = ask(r, out, "service manager (systemd|docker|launchd|none)", guess)
	switch p.Service.Type {
	case "docker":
		// the unit is the *container* name, not the binary — offer a picker
		if names := dockerContainers(&p); len(names) > 0 {
			say("  running containers (validator-looking ones first):")
			for i, n := range names {
				say("    %d) %s", i+1, n)
			}
			note("pick the container running THIS validator — not sidecars like autoheal")
			choice := ask(r, out, "container (number or name)", names[0])
			if n, err := strconv.Atoi(choice); err == nil && n >= 1 && n <= len(names) {
				p.Service.Unit = names[n-1]
			} else {
				p.Service.Unit = choice
			}
		} else {
			p.Service.Unit = ask(r, out, "container name", p.Binary)
		}
	case "systemd":
		note("the systemd unit managing the node, e.g. evmd.service")
		p.Service.Unit = ask(r, out, "service unit", p.Binary+".service")
	case "launchd":
		note("the launchd label, e.g. com.example.evmd — see `launchctl list`")
		p.Service.Unit = ask(r, out, "launchd label", p.Binary)
	}

	// --- signer key ---
	note("an ops key signs transactions (send/delegate/unjail) — skip for read-only monitoring")
	if askYN(r, out, "create an ops signing key now?", true) {
		keyName := ask(r, out, "key name", "ops")
		p.Signer.Key = keyName
		note("file = password-encrypted file · os = OS keychain · test = plaintext, testnets only")
		p.Signer.Backend = ask(r, out, "keyring backend (file|os|test)", "file")
		ring, err := keys.Open(&p)
		if err != nil {
			say("  ! keyring: %v", err)
		} else {
			k, mnemonic, err := ring.Generate(keyName, "", keys.AlgoEthSecp256k1,
				keys.DefaultCoinType, 0, 0)
			if err != nil {
				say("  ! key generation failed: %v", err)
			} else {
				addr, _ := k.Bech32(p.Bech32Prefix)
				say("  ✓ key %q → %s", keyName, addr)
				say("\n  WRITE THIS DOWN — mnemonic (never stored but the keyring):\n  %s\n", mnemonic)
				if p.Signer.Backend == "file" {
					say("  (file backend: set COMETCLI_KEYRING_PASSWORD before signing)")
				}
			}
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.UpsertProfile(&p); err != nil {
		return err
	}
	cfg.Active = p.Name
	if err := cfg.Save(); err != nil {
		return err
	}

	say("\nprofile %q saved and activated.", p.Name)
	say("\nnext:")
	say("  cometcli doctor            # health checklist")
	say("  cometcli mon watch         # live dashboard")
	say("  cometcli val status        # validator info")
	return nil
}

// detectService makes a best-effort guess at how the node is supervised.
// If docker has running containers that look like chain nodes, prefer docker.
func detectService(p *config.Profile) string {
	if p.Transport.Type == "ssh" {
		return "systemd"
	}
	if _, err := exec.LookPath("docker"); err == nil && len(dockerContainers(p)) > 0 {
		return "docker"
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		return "systemd"
	}
	if _, err := exec.LookPath("docker"); err == nil {
		return "docker"
	}
	if _, err := exec.LookPath("launchctl"); err == nil {
		return "launchd"
	}
	return "none"
}

// dockerContainers lists running container names (local transport only).
func dockerContainers(p *config.Profile) []string {
	if p.Transport.Type == "ssh" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	// rank node-like containers first — sidecars (autoheal, monitor, …) last
	score := func(n string) int {
		switch {
		case strings.Contains(n, "validator") || strings.Contains(n, p.Binary):
			return 0
		case strings.Contains(n, "node") || strings.Contains(n, "sentry"):
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(names, func(i, j int) bool { return score(names[i]) < score(names[j]) })
	return names
}
