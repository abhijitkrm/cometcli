package networktool

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

// RegisterProvision adds node.provision.
func RegisterProvision(r *toolkit.Registry) { r.Register(provisionTool{}) }

type provisionTool struct{}

func (provisionTool) Name() string { return "node.provision" }
func (provisionTool) Desc() string {
	return "Set up a new node for this profile from the network spec and join it to the network: init in the image, " +
		"install the network's genesis, render config.toml/app.toml/client.toml for the role (validator, archive, rpc), " +
		"write a docker-compose.yml beside the node home and start it, then wait for sync. Refuses a home that already " +
		"has a validator key (double-sign risk); reconfigure=true re-renders configs and compose only."
}
func (provisionTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"role":           toolkit.Enum("node role (default: the profile's)", "validator", "archive", "rpc"),
		"network":        toolkit.Str("chain id or spec path (default: the profile's chain id)"),
		"moniker":        toolkit.Str("node moniker (default: the profile name)"),
		"home":           toolkit.Str("node home on the host (default: the spec's host_home, else ~/.cometcli-nodes/<profile>)"),
		"image":          toolkit.Str("image to run (default: the spec's image naming and tag)"),
		"peers":          toolkit.Str("persistent peers, id@host:26656, comma-separated"),
		"peers_from":     toolkit.Str("profiles of running nodes to peer with (their node id and address are looked up)"),
		"external_ip":    toolkit.Str("public IP to advertise (default: the SSH host, or detected on the host)"),
		"docker_network": toolkit.Str("join an existing docker network (several nodes on one machine)"),
		"port_offset":    toolkit.Int("shift host ports (several nodes on one machine)"),
		"public_rpc":     toolkit.Bool("publish RPC/REST/gRPC/EVM on all interfaces (default: archive and rpc yes, validator no)"),
		"prod":           toolkit.Bool("public-network hardening (CORS, unlock, …) for every role"),
		"reconfigure":    toolkit.Bool("re-render configs and compose for an existing node; never re-inits"),
	})
}
func (provisionTool) Tier() toolkit.Tier                 { return toolkit.TierLocalChange }
func (provisionTool) Timeout(toolkit.Args) time.Duration { return 30 * time.Minute }

const containerHome = "/data/node0/evmd"

func run(c *toolkit.Context, h host.Host, script string, limit time.Duration) (string, int, error) {
	sub, cancel := toolkit.WithDeadline(c, limit)
	defer cancel()
	res, err := host.Exec(sub, h, script, 4<<20)
	c.LogShell(script, res.Code)
	return res.Output, res.Code, err
}

func (provisionTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	p := c.Profile
	if p == nil {
		return nil, fmt.Errorf("node.provision needs a profile for the node's host (cometcli profile add <name> --ssh-host … --service docker --unit <container>)")
	}
	netName := a.String("network", p.ChainID)
	spec, _, err := netspec.Load(netName)
	if err != nil {
		return nil, err
	}
	role := netspec.Role(a.String("role", string(netspec.RoleOf(p.Role))))
	if role != netspec.Validator && role != netspec.Archive && role != netspec.RPC {
		return nil, fmt.Errorf("role %q: want validator, archive or rpc", role)
	}
	moniker := a.String("moniker", p.Name)
	unit := p.Service.Unit
	if unit == "" {
		unit = "primium-" + string(role)
	}
	image := a.String("image", "")
	if image == "" {
		image = spec.Image.Name + ":" + spec.Image.Tag
		if spec.Image.Naming != "" {
			image = strings.NewReplacer("{tag}", spec.Image.Tag, "{version}", strings.TrimPrefix(spec.Image.Tag, "v")).Replace(spec.Image.Naming)
		}
	}
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	step := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		b.WriteString(line + "\n")
		if c.Progress != nil {
			c.Progress(line)
		}
	}

	// the image must be on the host already (build it: upgrade build)
	if _, code, _ := run(c, h, "docker image inspect "+common.ShellQ(image)+" >/dev/null 2>&1", time.Minute); code != 0 {
		return nil, fmt.Errorf("image %s isn't on %s — build it there first: cometcli --profile %s upgrade build --tag %s", image, p.Name, p.Name, spec.Image.Tag)
	}
	// the home: absolute, writable, and never someone's existing validator
	home := a.String("home", spec.HostHome)
	if home == "" {
		home = "~/.cometcli-nodes/" + p.Name
	}
	out, _, _ := run(c, h, fmt.Sprintf(`h=%s; case "$h" in "~"|"~/"*) h="$HOME${h#\~}";; esac; mkdir -p "$h" 2>/dev/null; if [ -w "$h" ]; then echo "ok $h $(id -u):$(id -g)"; else echo "denied $h $(id -un)"; fi; test -f "$h/config/priv_validator_key.json" && echo has-key || true`, shellHome(home)), time.Minute)
	f := strings.Fields(out)
	if len(f) < 3 || f[0] != "ok" {
		who := "you"
		if len(f) >= 3 {
			who = f[2]
		}
		return nil, fmt.Errorf("%s isn't writable on %s — create it for %s once: sudo mkdir -p %s && sudo chown -R %s %s", home, p.Name, who, home, who, home)
	}
	home, user := f[1], f[2]
	exists := strings.Contains(out, "has-key")
	reconfigure := a.Bool("reconfigure", false)
	if exists && !reconfigure {
		return nil, fmt.Errorf("%s already holds a node (config/priv_validator_key.json) — re-initialising would replace its keys and state (double-sign risk). To re-render its configs only: reconfigure=true", home)
	}

	// peers and the advertised address
	peers := splitList(a.String("peers", ""))
	for _, name := range splitList(a.String("peers_from", "")) {
		peer, err := peerOf(c, name, a.String("docker_network", ""))
		if err != nil {
			return nil, fmt.Errorf("peer %s: %w", name, err)
		}
		peers = append(peers, peer)
	}
	if len(peers) == 0 && !exists {
		return nil, fmt.Errorf("no peers to join through — pass peers (id@host:26656) or peers_from (profiles of running nodes)")
	}
	ext := a.String("external_ip", "")
	extAddr := ""
	switch {
	case ext == "" && a.String("docker_network", "") != "":
		// on a shared docker network peers dial the container by name
		extAddr = unit + ":26656"
	default:
		if ext == "" {
			ext = externalIP(c, h)
		}
		if ext != "" {
			extAddr = net.JoinHostPort(ext, strconv.Itoa(26656+int(a.Int("port_offset", 0))))
		}
	}

	detail := map[string]any{"role": role, "image": image, "home": home, "container": unit, "peers": strings.Join(peers, ","), "advertise": orDash(extAddr)}
	verb := "set up a new " + string(role) + " node"
	if exists {
		verb = "re-render the configs of the " + string(role) + " node"
	}
	if err := c.Approve(fmt.Sprintf("%s on %s (%s) for %s", verb, p.Name, home, spec.Chain.ID), toolkit.TierLocalChange, detail); err != nil {
		return nil, err
	}

	// run as the home's owner, working in the home: the image's own
	// workdir isn't writable by that user ("mkdir data: permission denied")
	dockerRun := fmt.Sprintf("docker run --rm --user %s -e HOME=%s -w %s -v %s:%s --entrypoint evmd %s", user, containerHome, containerHome, common.ShellQ(home), containerHome, common.ShellQ(image))
	if !exists {
		if out, code, err := run(c, h, fmt.Sprintf("%s init %s --chain-id %s --home %s >/dev/null 2>&1 || { echo init-failed; %s init %s --chain-id %s --home %s 2>&1 | grep -m2 -i -E 'panic|error'; }", dockerRun, common.ShellQ(moniker), common.ShellQ(spec.Chain.ID), containerHome, dockerRun, common.ShellQ(moniker), common.ShellQ(spec.Chain.ID), containerHome), 3*time.Minute); err != nil || code != 0 || strings.Contains(out, "init-failed") {
			return nil, fmt.Errorf("init failed: %v %s", err, strings.TrimSpace(out))
		}
		step("✓ initialised %s in %s", moniker, home)
		gen, err := genesisFor(spec.Chain.ID)
		if err != nil {
			return nil, err
		}
		if err := h.WriteFile(c, path.Join(home, "config", "genesis.json"), gen, 0o644); err != nil {
			return nil, fmt.Errorf("install genesis: %w", err)
		}
		sum := sha256.Sum256(gen)
		step("✓ genesis installed (sha256 %s…)", hex.EncodeToString(sum[:])[:16])
		// joining a running network: its genesis is already law, so the
		// CLI's stricter checks are a note, not a blocker
		if out, code, _ := run(c, h, dockerRun+" genesis validate-genesis --home "+containerHome+" 2>&1 | grep -i -m1 'error validating' ", 2*time.Minute); code == 0 && strings.TrimSpace(out) != "" {
			step("! validate-genesis: %s (the network runs with it — continuing)", strings.TrimSpace(out))
		}
	}

	// configs, rendered for the role from the spec
	version := spec.Image.Tag
	if m := regexp.MustCompile(`v\d+\.\d+\.\d+`).FindString(image); m != "" {
		version = m
	}
	patches := spec.Patches(role, version, netspec.NodeParams{Moniker: moniker, ExternalAddress: extAddr, PersistentPeers: peers, Prod: a.Bool("prod", false)})
	for _, file := range []string{"config.toml", "app.toml", "client.toml"} {
		fp := path.Join(home, "config", file)
		raw, err := h.ReadFile(c, fp)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		out := netspec.Apply(string(raw), file, patches)
		var check map[string]any
		if _, err := toml.Decode(out, &check); err != nil {
			return nil, fmt.Errorf("rendered %s doesn't parse (nothing written): %w", file, err)
		}
		if err := h.WriteFile(c, fp, []byte(out), 0o644); err != nil {
			return nil, err
		}
	}
	step("✓ config.toml, app.toml, client.toml rendered for a %s", role)

	compose := netspec.Compose{Image: image, Container: unit, HostHome: home, Command: spec.Command(role), User: user,
		PublicRPC: a.Bool("public_rpc", role != netspec.Validator), Network: a.String("docker_network", ""),
		Services: spec.Services, PortOffset: int(a.Int("port_offset", 0))}
	cf := path.Join(home, "docker-compose.yml")
	if err := h.WriteFile(c, cf, []byte(compose.YAML()), 0o644); err != nil {
		return nil, err
	}
	if out, code, err := run(c, h, fmt.Sprintf("cd %s && docker compose -f docker-compose.yml up -d --force-recreate 2>&1 | tail -3", common.ShellQ(home)), 5*time.Minute); err != nil || code != 0 {
		return nil, fmt.Errorf("start failed: %v %s", err, out)
	}
	step("✓ %s started (%s)", unit, cf)

	// the profile now describes this node
	off := int(a.Int("port_offset", 0))
	np := *p
	np.Role, np.Home, np.ChainID, np.Binary = string(role), home, spec.Chain.ID, "evmd"
	np.Service = config.Service{Type: "docker", Unit: unit}
	np.Endpoints.Comet = fmt.Sprintf("tcp://127.0.0.1:%d", 26657+off)
	if spec.Services.GRPC {
		np.Endpoints.GRPC = fmt.Sprintf("127.0.0.1:%d", def(spec.Services.GRPCPort, 9090)+off)
	}
	if spec.Services.JSONRPC {
		np.Endpoints.EVM = fmt.Sprintf("http://127.0.0.1:%d", def(spec.Services.RPCPort, 8545)+off)
	}
	np.Signer.ContainerHome = containerHome
	if c.Cfg != nil {
		if err := c.Cfg.UpsertProfile(&np); err == nil {
			step("✓ profile %s updated (role %s, %s)", np.Name, role, np.Endpoints.Comet)
		}
	}
	switch role {
	case netspec.Validator:
		fmt.Fprintf(&b, "\nnext, once it has caught up (wait.until synced):\n"+
			"  1. the operator creates the validator key in the node's own keyring, in their terminal:\n"+
			"       docker exec -it %s evmd keys add %s --keyring-backend file --home %s\n"+
			"  2. fund that address, then: cometcli --profile %s val create --amount <stake> --moniker %s\n",
			unit, moniker, containerHome, p.Name, moniker)
	default:
		fmt.Fprintf(&b, "\nnext: cometcli --profile %s wait until --condition synced, then cometcli network check --nodes %s\n", p.Name, p.Name)
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"home": home, "container": unit, "image": image, "role": string(role), "peers": peers}}, nil
}

func def(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// shellHome quotes a home path, leaving a leading ~ for the script to
// expand.
func shellHome(h string) string {
	if strings.HasPrefix(h, "~") {
		return "'" + strings.ReplaceAll(h, "'", `'\''`) + "'"
	}
	return common.ShellQ(h)
}

// peerOf is a running node's id@address: its address is the docker
// container name on a shared network, else its SSH host.
func peerOf(c *toolkit.Context, name, dockerNet string) (string, error) {
	if c.Cfg == nil {
		return "", fmt.Errorf("no config")
	}
	pp, ok := c.Cfg.Profiles[name]
	if !ok {
		return "", fmt.Errorf("no profile %s", name)
	}
	pp.Name = name
	pc := &toolkit.Context{Context: c.Context, Profile: pp, Cfg: c.Cfg}
	defer pc.Close()
	cc, err := pc.Comet()
	if err != nil {
		return "", err
	}
	st, err := cc.Status(pc)
	if err != nil {
		return "", err
	}
	addr := ""
	switch {
	case dockerNet != "" && pp.Service.Unit != "":
		addr = pp.Service.Unit
	case pp.Transport.Type == "ssh":
		addr = host.Resolve(pp.Transport).HostName
	default:
		addr = endpointHost(pp.Endpoints.Comet)
	}
	return fmt.Sprintf("%s@%s:26656", st.NodeInfo.ID(), addr), nil
}

func endpointHost(ep string) string {
	if _, rest, ok := strings.Cut(ep, "://"); ok {
		ep = rest
	}
	h, _, err := net.SplitHostPort(ep)
	if err != nil {
		return ep
	}
	return h
}

// externalIP is the address peers should dial: the SSH host when it's
// an IP, else what the host sees as its public IP.
func externalIP(c *toolkit.Context, h host.Host) string {
	if c.Profile.Transport.Type == "ssh" {
		if ip := net.ParseIP(host.Resolve(c.Profile.Transport).HostName); ip != nil {
			return ip.String()
		}
	}
	out, code, _ := run(c, h, "curl -s --max-time 5 https://checkip.amazonaws.com || curl -s --max-time 5 https://ifconfig.me", 15*time.Second)
	if ip := net.ParseIP(strings.TrimSpace(out)); code == 0 && ip != nil {
		return ip.String()
	}
	return ""
}

// genesisFor reads the network's saved genesis (network import --from-node).
func genesisFor(chainID string) ([]byte, error) {
	p, err := GenesisPath(chainID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("no genesis saved for %s — capture it from a running node: cometcli network import --from-node <profile>", chainID)
	}
	return b, nil
}

// GenesisPath is where a network's genesis.json is kept, beside its spec.
func GenesisPath(chainID string) (string, error) {
	p, err := netspec.Path(chainID)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(p, ".yaml") + ".genesis.json", nil
}

// FetchGenesis downloads a node's genesis.json byte for byte (in chunks).
func FetchGenesis(c *toolkit.Context) ([]byte, error) {
	cc, err := c.Comet()
	if err != nil {
		return nil, err
	}
	var out []byte
	for chunk := uint(0); ; chunk++ {
		res, err := cc.RPC.GenesisChunked(c, chunk)
		if err != nil {
			return nil, fmt.Errorf("genesis chunk %d: %w", chunk, err)
		}
		b, err := base64.StdEncoding.DecodeString(res.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
		if int(chunk)+1 >= res.TotalChunks {
			return out, nil
		}
	}
}

// PeersOf is id@address of every other running node of the chain among
// the profiles (for a playbook that adds peers to a lonely node).
func PeersOf(c *toolkit.Context, chainID, except string) []string {
	if c.Cfg == nil {
		return nil
	}
	var out []string
	for name, p := range c.Cfg.Profiles {
		if name == except || p.ChainID != chainID {
			continue
		}
		if peer, err := peerOf(c, name, ""); err == nil {
			out = append(out, peer)
		}
	}
	sort.Strings(out)
	return out
}
