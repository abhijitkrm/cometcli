package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

	say("cometcli init — profile setup wizard\n")

	name := ask(r, out, "profile name", "myval")
	var p config.Profile
	p.Name = name
	p.Role = "validator"

	// --- comet rpc: probe for chain-id + moniker ---
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
			p.ChainID = ask(r, out, "chain-id", "")
		}
	} else {
		p.ChainID = ask(r, out, "chain-id", "")
	}
	p.Bech32Prefix = ask(r, out, "bech32 prefix", "cosmos")

	// --- grpc: probe for bond denom + validator prefix ---
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
	p.Binary = ask(r, out, "node binary name", "evmd")
	p.Home = ask(r, out, "node home dir", filepath.Join("~", "."+p.Binary))
	transport := ask(r, out, "host transport (local|ssh)", "local")
	p.Transport.Type = transport
	if transport == "ssh" {
		p.Transport.Host = ask(r, out, "ssh host", host)
		p.Transport.User = ask(r, out, "ssh user", "ops")
		if port := ask(r, out, "ssh port", "22"); port != "" {
			fmt.Sscanf(port, "%d", &p.Transport.Port)
		}
		p.Transport.KeyFile = ask(r, out, "ssh key file", filepath.Join("~", ".ssh", "id_ed25519"))
	}

	// --- service supervision: detect best guess ---
	guess := detectService(&p)
	p.Service.Type = ask(r, out, "service manager (systemd|docker|launchd|none)", guess)
	if p.Service.Type != "none" && p.Service.Type != "" {
		defUnit := map[string]string{
			"systemd": p.Binary + ".service", "docker": p.Binary, "launchd": p.Binary,
		}[p.Service.Type]
		p.Service.Unit = ask(r, out, "service unit/container name", defUnit)
	}

	// --- signer key ---
	if askYN(r, out, "create an ops signing key now?", true) {
		keyName := ask(r, out, "key name", "ops")
		p.Signer.Key = keyName
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
func detectService(p *config.Profile) string {
	if p.Transport.Type == "ssh" {
		return "systemd"
	}
	// docker container named like the binary?
	if out, err := os.ReadFile("/proc/1/cgroup"); err == nil &&
		strings.Contains(string(out), "docker") {
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
