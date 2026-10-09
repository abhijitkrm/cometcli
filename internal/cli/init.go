package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path"
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
	"github.com/abhijitkrm/cometcli/internal/client/host"
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
		askSSH(&p, r, out, note)
	}
	h, err := connectHost(ctx, &p, r, out, say)
	if err != nil {
		return err
	}
	if c, ok := h.(io.Closer); ok {
		defer c.Close()
	}
	// endpoints on the node machine are reached through the ssh connection
	via := func(ep string) host.DialFunc {
		if host.OnNode(p.Transport, ep) {
			return host.NodeDialer(h, p.Transport)
		}
		return nil
	}

	note("how the node process is supervised — picks the backend for start/stop/restart/logs")
	p.Service.Type = ask(r, out, "service manager (systemd|docker|launchd|none)", detectService(ctx, h, &p))

	// --- auto-discovery: read port bindings, mounts, binary, running process ---
	var d discovered
	switch p.Service.Type {
	case "docker":
		pickDocker(ctx, h, &p, r, out, say, note)
		if p.Service.Unit != "" {
			d = inspectDocker(ctx, h, p.Service.Unit)
			d.binary = containerBinary(ctx, h, p.Service.Unit)
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
		if dd := inspectLocal(ctx, h, via("127.0.0.1:1")); dd.binary != "" || dd.home != "" {
			d = dd
			say("  ✓ found node: binary %s, home %s", orDash(d.binary), orDash(d.home))
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
	if cc, err := comet.NewVia(cometEP, via(cometEP)); err == nil {
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
		gc, err := grpc.DialVia(dctx, grpcEP, via(grpcEP))
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
	note("Ethereum JSON-RPC port (default 8545) — enables evm commands; \"none\" for a plain Cosmos SDK chain")
	evmEP := ask(r, out, "EVM JSON-RPC endpoint (none to skip)", def(d.evm, fmt.Sprintf("http://%s:8545", host)))
	if strings.EqualFold(evmEP, "none") || evmEP == "-" {
		evmEP = ""
	}
	if evmEP != "" {
		p.Endpoints.EVM = evmEP
		ectx, ec := context.WithTimeout(ctx, 5*time.Second)
		if id, err := evm.NewVia(evmEP, via(evmEP)).ChainID(ectx); err == nil {
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
	if p.Transport.Type == "ssh" {
		note("the node's data dir on " + p.Transport.Host + " — for docker, the mounted host path")
	} else {
		note("the node's data dir as seen from THIS host — for docker, the mounted host path")
	}
	p.Home = ask(r, out, "node home dir", def(d.home, filepath.Join("~", "."+p.Binary)))

	// the node's own app.toml tells us what fee the mempool will accept
	if gp, denom := minGasPriceDenom(ctx, h, p.Home); gp != "" {
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

	// --- which validator is this node, and what can sign for it ---
	tc := &toolkit.Context{Context: ctx, Profile: &p}
	tc.SetHost(h)
	setupSigner(tc, d, r, out, say, note)
	tc.Close() // also closes h; SSH.Close is idempotent

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
func detectService(ctx context.Context, h host.Host, p *config.Profile) string {
	has := func(bin string) bool { _, code, err := run(ctx, h, "command -v "+bin); return err == nil && code == 0 }
	if has("docker") && len(dockerContainers(ctx, h, p)) > 0 {
		return "docker"
	}
	for _, s := range []string{"systemctl:systemd", "docker:docker", "launchctl:launchd"} {
		bin, svc, _ := strings.Cut(s, ":")
		if has(bin) {
			return svc
		}
	}
	return "none"
}

// run is a short discovery command on the node machine.
func run(ctx context.Context, h host.Host, cmd string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return h.Run(ctx, cmd)
}

// askSSH fills the ssh transport; defaults come from ~/.ssh/config, and
// only what differs from it is saved in the profile.
func askSSH(p *config.Profile, r *bufio.Reader, out io.Writer, note func(string)) {
	note("an address or a ~/.ssh/config alias — user, port, key and ProxyJump are read from there")
	p.Transport.Host = ask(r, out, "ssh host", "")
	tg := host.Resolve(p.Transport)
	if tg.HostName != tg.Alias {
		note("~/.ssh/config: " + tg.Alias + " → " + tg.String())
	}
	if u := ask(r, out, "ssh user", tg.User); u != tg.User {
		p.Transport.User = u
	}
	if port := ask(r, out, "ssh port", strconv.Itoa(tg.Port)); port != strconv.Itoa(tg.Port) {
		fmt.Sscanf(port, "%d", &p.Transport.Port)
	}
	def := ""
	if len(tg.Keys) > 0 {
		def = tg.Keys[0]
	}
	note("private key — blank uses ssh-agent and ~/.ssh/id_*; passphrase-protected keys are asked for or taken from ssh-agent")
	if k := ask(r, out, "ssh key file", def); k != def {
		p.Transport.KeyFile = k
	}
	if len(tg.Jumps) == 0 {
		note("a bastion to go through, like ssh -J (blank for a direct connection)")
		p.Transport.Jump = ask(r, out, "jump host", "")
	}
}

// connectHost opens the profile's transport, asking to trust a new host
// key and for key passphrases as ssh would.
func connectHost(ctx context.Context, p *config.Profile, r *bufio.Reader, out io.Writer, say func(string, ...any)) (host.Host, error) {
	if p.Transport.Type != "ssh" {
		return host.Connect(ctx, p)
	}
	restore := interactiveSSH(r, out)
	defer restore()
	h, err := host.Connect(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("%w\nfix the ssh settings and run cometcli init again", err)
	}
	who, _, _ := run(ctx, h, "echo \"$(id -un)@$(hostname)\"")
	say("  ✓ connected over ssh: %s", strings.TrimSpace(who))
	return h, nil
}

// interactiveSSH lets the ssh transport ask on this terminal: trust a new
// host key, unlock a key file. It returns a func that undoes it.
func interactiveSSH(r *bufio.Reader, out io.Writer) func() {
	trust, pass := host.TrustHost, host.Passphrase
	host.TrustHost = func(h, fp string) bool {
		fmt.Fprintf(out, "  the authenticity of host %s can't be established.\n  its key fingerprint is %s.\n", h, fp)
		return askYN(r, out, "  trust it and add it to known_hosts?", false)
	}
	host.Passphrase = func(keyFile string) ([]byte, error) {
		pw, err := readHidden(r, out, "  passphrase for "+keyFile+": ")
		return []byte(strings.TrimRight(pw, "\r\n")), err
	}
	return func() { host.TrustHost, host.Passphrase = trust, pass }
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
func pickDocker(ctx context.Context, h host.Host, p *config.Profile, r *bufio.Reader, out io.Writer, say func(string, ...any), note func(string)) {
	names := dockerContainers(ctx, h, p)
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
func inspectDocker(ctx context.Context, h host.Host, container string) (d discovered) {
	out, _, err := run(ctx, h, "docker inspect "+shq(container))
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
	if err := json.Unmarshal([]byte(out), &meta); err != nil || len(meta) == 0 {
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
		if _, code, err := run(ctx, h, "test -f "+shq(path.Join(m.Source, "config", "genesis.json"))); err == nil && code == 0 {
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

func containerBinary(ctx context.Context, h host.Host, container string) string {
	for _, b := range binCandidates {
		if _, code, err := run(ctx, h, "docker exec "+shq(container)+" "+b+" version"); err == nil && code == 0 {
			return b
		}
	}
	return ""
}

// inspectLocal finds a node running on the host: binary on PATH, `--home`
// from the process args, and which default ports are actually listening
// (dialed through dial on a remote host).
func inspectLocal(ctx context.Context, h host.Host, dial host.DialFunc) (d discovered) {
	for _, b := range binCandidates {
		if _, code, err := run(ctx, h, "command -v "+b); err == nil && code == 0 {
			d.binary = b
			break
		}
	}
	if d.binary != "" {
		if out, _, err := run(ctx, h, "ps -eo args"); err == nil {
			for _, line := range strings.Split(out, "\n") {
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
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	open := func(port string) bool {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		c, err := dial(dctx, "tcp", "127.0.0.1:"+port)
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

// minGasPriceDenom reads app.toml's minimum-gas-prices ("1000000000atest")
// as amount and denom — the denom is what the node accepts fees in.
func minGasPriceDenom(ctx context.Context, h host.Host, home string) (string, string) {
	if _, local := h.(*host.Local); local {
		home = expandHome(home)
	}
	b, err := h.ReadFile(ctx, path.Join(home, "config", "app.toml"))
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

// dockerContainers lists running container names on the node's host.
func dockerContainers(ctx context.Context, h host.Host, p *config.Profile) []string {
	out, _, err := run(ctx, h, "docker ps --format '{{.Names}}'")
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
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
func setupSigner(tc *toolkit.Context, d discovered, r *bufio.Reader, out io.Writer, say func(string, ...any), note func(string)) {
	p := tc.Profile
	if p.Metadata == nil {
		p.Metadata = map[string]string{}
	}
	valoper, verr := common.ValoperFromNode(tc)
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
		h, _ := tc.Host()
		show := fmt.Sprintf("docker exec %s %s keys show %s -a --keyring-backend test --home %s 2>&1",
			shq(p.Service.Unit), shq(bin), shq(acc), shq(d.containerHome))
		if outs, _, err := run(tc, h, show); err == nil && strings.Contains(outs, acc) {
			p.Signer.Mode, p.Signer.ContainerHome, p.Signer.ContainerKeyring = "container", d.containerHome, "test"
			say("  ✓ the node's keyring has the operator key — transactions are signed inside %s", p.Service.Unit)
			return
		}
		if _, code, err := run(tc, h, "docker exec "+shq(p.Service.Unit)+" ls "+shq(d.containerHome+"/keyring-file")); err == nil && code == 0 {
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

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
