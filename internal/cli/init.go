package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
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
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
	"golang.org/x/term"
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

	// --- host-plane first: how the node runs decides what we can discover ---
	note("local = node runs on this machine; ssh = manage a remote host")
	p.Transport.Type = ask(r, out, "host transport (local|ssh)", "local")
	if p.Transport.Type == "ssh" {
		p.Transport.Host = ask(r, out, "ssh host", "")
		p.Transport.User = ask(r, out, "ssh user", "ops")
		if port := ask(r, out, "ssh port", "22"); port != "" {
			fmt.Sscanf(port, "%d", &p.Transport.Port)
		}
		note("private key for ssh auth — password-protected keys need ssh-agent")
		p.Transport.KeyFile = ask(r, out, "ssh key file", filepath.Join("~", ".ssh", "id_ed25519"))
	}

	note("how the node process is supervised — picks the backend for start/stop/restart/logs")
	p.Service.Type = ask(r, out, "service manager (systemd|docker|launchd|none)", detectService(&p))

	// --- auto-discovery: read port bindings, mounts, binary, running process ---
	var d discovered
	switch p.Service.Type {
	case "docker":
		pickDocker(&p, r, out, say, note)
		if p.Service.Unit != "" {
			d = inspectDocker(p.Service.Unit)
			d.binary = containerBinary(p.Service.Unit)
			if d.comet != "" || d.home != "" {
				say("  ✓ discovered from container: comet %s, home %s",
					orDash(d.comet), orDash(d.home))
			}
		}
	case "systemd", "launchd":
		note(map[string]string{
			"systemd": "the systemd unit managing the node, e.g. evmd.service",
			"launchd": "the launchd label, e.g. com.example.evmd — see `launchctl list`",
		}[p.Service.Type])
		p.Service.Unit = ask(r, out, "service unit", map[string]string{
			"systemd": "evmd.service",
		}[p.Service.Type])
		fallthrough
	default:
		if dd := inspectLocal(); dd.binary != "" || dd.home != "" {
			d = dd
			say("  ✓ found local node: binary %s, home %s", orDash(d.binary), orDash(d.home))
		}
	}

	def := func(found, fallback string) string {
		if found != "" {
			return found
		}
		return fallback
	}

	// --- comet rpc: probe for chain-id + moniker ---
	note("the node's CometBFT RPC — for docker this is the published host port")
	cometEP := ask(r, out, "CometBFT RPC endpoint", def(d.comet, "tcp://127.0.0.1:26657"))
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
	grpcEP := ask(r, out, "gRPC endpoint", def(d.grpc, net.JoinHostPort(host, "9090")))
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

	// --- optional REST/LCD ---
	note("Cosmos REST/LCD port (default 1317) — optional; some chains don't expose it")
	if lcd := ask(r, out, "REST endpoint (blank to skip)", def(d.lcd, "")); lcd != "" {
		p.Endpoints.LCD = lcd
	}

	// --- evm json-rpc: probe chain id ---
	note("Ethereum JSON-RPC port (default 8545) — enables evm commands; blank disables them")
	evmEP := ask(r, out, "EVM JSON-RPC endpoint (blank to skip)", def(d.evm, fmt.Sprintf("http://%s:8545", host)))
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

	// --- binary + home: defaults come from discovery ---
	note("the node daemon binary — usually evmd; for docker, the binary inside the container")
	p.Binary = ask(r, out, "node binary name", def(d.binary, "evmd"))
	note("the node's data dir as seen from THIS host — for docker, the mounted host path")
	p.Home = ask(r, out, "node home dir", def(d.home, filepath.Join("~", "."+p.Binary)))

	// the node's own app.toml tells us what fee the mempool will accept
	if p.Transport.Type != "ssh" {
		if gp, denom := minGasPriceDenom(expandHome(p.Home)); gp != "" {
			if p.Metadata == nil {
				p.Metadata = map[string]string{}
			}
			p.Metadata["gas_price"] = gp
			if denom != "" {
				// fees are paid in what the node accepts — on EVM chains
				// that's usually not the bonding denom
				p.Metadata["fee_denom"] = denom
			}
			say("  ✓ fees from app.toml: %s%s per gas", gp, denom)
		}
	}

	// --- which validator is this node, and what can sign for it ---
	setupSigner(&p, d, r, out, say, note)

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

// discovered holds what the wizard figured out on its own before asking.
type discovered struct {
	comet, grpc, lcd, evm string
	home, binary          string
	containerHome         string // the node home as the container sees it
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// pickDocker lists running containers and sets p.Service.Unit to the choice.
func pickDocker(p *config.Profile, r *bufio.Reader, out io.Writer, say func(string, ...any), note func(string)) {
	names := dockerContainers(p)
	if len(names) == 0 {
		note("no running containers found — enter the container name manually")
		p.Service.Unit = ask(r, out, "container name", "evmd")
		return
	}
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
}

// inspectDocker reads a container's published ports and bind mounts so the
// user doesn't have to type them — port mappings remap on restart, and the
// mounted home dir is what file checks need.
func inspectDocker(container string) (d discovered) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", container).Output()
	if err != nil {
		return d
	}
	var meta []struct {
		HostConfig struct {
			PortBindings map[string][]struct {
				HostPort string `json:"HostPort"`
			} `json:"PortBindings"`
		} `json:"HostConfig"`
		Mounts []struct {
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		} `json:"Mounts"`
	}
	if err := json.Unmarshal(out, &meta); err != nil || len(meta) == 0 {
		return d
	}
	hostPort := func(containerPort string) string {
		if b := meta[0].HostConfig.PortBindings[containerPort+"/tcp"]; len(b) > 0 {
			return b[0].HostPort
		}
		return ""
	}
	if p := hostPort("26657"); p != "" {
		d.comet = "tcp://127.0.0.1:" + p
	}
	if p := hostPort("9090"); p != "" {
		d.grpc = "127.0.0.1:" + p
	}
	if p := hostPort("1317"); p != "" {
		d.lcd = "http://127.0.0.1:" + p
	}
	if p := hostPort("8545"); p != "" {
		d.evm = "http://127.0.0.1:" + p
	}
	// home dir: the mount whose destination looks like a node home
	// (/.evmd, /home/x/.evmd, …). No fallback — a random mount like
	// /var/run/docker.sock is worse than asking the user.
	// best: a mount that actually holds a node home (config/genesis.json)
	for _, m := range meta[0].Mounts {
		if _, err := os.Stat(filepath.Join(m.Source, "config", "genesis.json")); err == nil {
			d.home, d.containerHome = m.Source, m.Destination
			break
		}
	}
	for _, m := range meta[0].Mounts {
		if d.home != "" {
			break
		}
		base := filepath.Base(m.Destination)
		if strings.HasPrefix(base, ".") || strings.Contains(base, "evmd") {
			d.home, d.containerHome = m.Source, m.Destination
		}
	}
	return d
}

// containerBinary probes which daemon binary exists inside the container.
var binCandidates = []string{"evmd", "simd", "cosmosd", "gaiad", "wasmd"}

func containerBinary(container string) string {
	for _, b := range binCandidates {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := exec.CommandContext(ctx, "docker", "exec", container, b, "version").Run()
		cancel()
		if err == nil {
			return b
		}
	}
	return ""
}

// inspectLocal finds a locally-running node: binary on PATH, `--home` from
// the process args, and which default ports are actually listening.
func inspectLocal() (d discovered) {
	for _, b := range binCandidates {
		if _, err := exec.LookPath(b); err == nil {
			d.binary = b
			break
		}
	}
	if d.binary != "" {
		if out, err := exec.Command("ps", "-eo", "args").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.Contains(line, d.binary) || strings.Contains(line, "cometcli") {
					continue
				}
				if h := homeFromArgs(line); h != "" {
					d.home = h
					break
				}
			}
		}
	}
	open := func(port string) bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 400*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	if open("26657") {
		d.comet = "tcp://127.0.0.1:26657"
	}
	if open("9090") {
		d.grpc = "127.0.0.1:9090"
	}
	if open("1317") {
		d.lcd = "http://127.0.0.1:1317"
	}
	if open("8545") {
		d.evm = "http://127.0.0.1:8545"
	}
	return d
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, path[2:])
		}
	}
	return path
}

// minGasPrice reads app.toml's minimum-gas-prices ("1000000000adex") and
// returns the numeric price, so txs default to what the mempool accepts.
// minGasPriceDenom reads app.toml's minimum-gas-prices ("1000000000atest")
// as amount and denom — the denom is what the node accepts fees in.
func minGasPriceDenom(home string) (string, string) {
	b, err := os.ReadFile(filepath.Join(home, "config", "app.toml"))
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "minimum-gas-prices") {
			continue
		}
		_, v, _ := strings.Cut(line, "=")
		v = strings.Trim(strings.TrimSpace(v), `",`)
		v, _, _ = strings.Cut(v, ",") // first denom when several are accepted
		i := 0
		for i < len(v) && (v[i] >= '0' && v[i] <= '9' || v[i] == '.') {
			i++
		}
		return v[:i], strings.TrimSpace(v[i:])
	}
	return "", ""
}

// homeFromArgs extracts `--home /path` or `--home=/path` from a cmdline.
func homeFromArgs(args string) string {
	if i := strings.Index(args, "--home="); i >= 0 {
		f := strings.Fields(args[i+7:])
		if len(f) > 0 {
			return f[0]
		}
	}
	if i := strings.Index(args, "--home "); i >= 0 {
		f := strings.Fields(args[i+7:])
		if len(f) > 0 {
			return f[0]
		}
	}
	return ""
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
	// rank node-like containers first — sidecars (autoheal, monitor, …) last.
	// p.Binary may not be known yet, so use name heuristics + common binaries.
	score := func(n string) int {
		switch {
		case strings.Contains(n, "validator"):
			return 0
		case p.Binary != "" && strings.Contains(n, p.Binary):
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

// setupSigner identifies the validator from the node's own consensus key
// and finds a way to sign as its operator account — the node's keyring
// first, then importing the operator key. It never makes up a new key for
// a validator: only the operator account can unjail, vote or edit it.
func setupSigner(p *config.Profile, d discovered, r *bufio.Reader, out io.Writer, say func(string, ...any), note func(string)) {
	if p.Metadata == nil {
		p.Metadata = map[string]string{}
	}
	vctx := &toolkit.Context{Context: context.Background(), Profile: p}
	valoper, verr := common.ValoperFromNode(vctx)
	vctx.Close()
	if verr != nil {
		say("  · not a validator on chain (yet): %v", verr)
		note("an ops key can send funds or create a validator later — skip for read-only monitoring")
		if askYN(r, out, "create a new ops key now?", false) {
			newOpsKey(p, r, out, say, note)
		}
		return
	}
	acc, _ := keys.AccFromValoper(valoper)
	p.Metadata["valoper"] = valoper
	say("  ✓ this node signs for validator %s\n    operator account %s — the account that unjails, votes and edits it", valoper, acc)

	// the node's own keyring: signing happens inside the container, the
	// key never leaves the node
	if p.Service.Type == "docker" && p.Service.Unit != "" && d.containerHome != "" {
		bin := p.Binary
		if bin == "" {
			bin = "evmd"
		}
		show := exec.Command("docker", "exec", p.Service.Unit, bin, "keys", "show", acc, "-a",
			"--keyring-backend", "test", "--home", d.containerHome)
		if outb, err := show.CombinedOutput(); err == nil && strings.Contains(string(outb), acc) {
			p.Signer.Mode, p.Signer.ContainerHome, p.Signer.ContainerKeyring = "container", d.containerHome, "test"
			say("  ✓ the node's keyring has the operator key — transactions are signed inside %s", p.Service.Unit)
			return
		}
		ls := exec.Command("docker", "exec", p.Service.Unit, "ls", d.containerHome+"/keyring-file")
		if err := ls.Run(); err == nil {
			p.Signer.Mode, p.Signer.ContainerHome = "container", d.containerHome
			say("  ✓ the node has an encrypted (file) keyring — cometcli signs inside %s and asks for its password when needed", p.Service.Unit)
			return
		}
	}
	note("no copy of the operator key was found on the node — import it to send transactions from cometcli")
	note("(read-only checks, triage and incidents work without it)")
	if !askYN(r, out, "import the operator key now? (you'll type its mnemonic; it isn't shown)", false) {
		say("  · skipped — import later: cometcli keys add --name operator --recover, then cometcli profile add %s --signer operator", p.Name)
		return
	}
	note("file = password-encrypted file · os = OS keychain · test = plaintext, testnets only")
	p.Signer.Backend = ask(r, out, "keyring backend (file|os|test)", "file")
	mnemonic, err := readHidden(r, out, "operator mnemonic: ")
	if err != nil || strings.TrimSpace(mnemonic) == "" {
		say("  ! no mnemonic read — skipped")
		return
	}
	ring, err := keys.Open(p)
	if err != nil {
		say("  ! keyring: %v", err)
		return
	}
	k, _, err := ring.Generate("operator", strings.TrimSpace(mnemonic), keys.AlgoEthSecp256k1, keys.DefaultCoinType, 0, 0)
	if err != nil {
		say("  ! import failed: %v", err)
		return
	}
	got, _ := k.Bech32(p.Bech32Prefix)
	if got != acc {
		_ = ring.Remove("operator")
		say("  ! that mnemonic is %s, not the operator account %s — not saved (check the coin type / algorithm)", got, acc)
		return
	}
	p.Signer.Key, p.Signer.Mode = "operator", "local"
	say("  ✓ operator key imported as \"operator\"")
}

// newOpsKey generates a fresh key (non-validator profiles only).
func newOpsKey(p *config.Profile, r *bufio.Reader, out io.Writer, say func(string, ...any), note func(string)) {
	keyName := ask(r, out, "key name", "ops")
	note("file = password-encrypted file · os = OS keychain · test = plaintext, testnets only")
	p.Signer.Backend = ask(r, out, "keyring backend (file|os|test)", "file")
	ring, err := keys.Open(p)
	if err != nil {
		say("  ! keyring: %v", err)
		return
	}
	k, mnemonic, err := ring.Generate(keyName, "", keys.AlgoEthSecp256k1, keys.DefaultCoinType, 0, 0)
	if err != nil {
		say("  ! key generation failed: %v", err)
		return
	}
	p.Signer.Key = keyName
	addr, _ := k.Bech32(p.Bech32Prefix)
	say("  ✓ key %q → %s", keyName, addr)
	say("\n  WRITE THIS DOWN — the mnemonic is shown once. Don't paste it into chats or tickets:\n  %s\n", mnemonic)
}

// readHidden reads a secret without echo on a terminal.
func readHidden(r *bufio.Reader, out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(out)
		return string(b), err
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}
