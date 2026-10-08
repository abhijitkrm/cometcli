// Package drill builds a throwaway local validator network in docker and
// runs incident drills on it: inject a real fault, let the agent work it,
// score whether triage named the right cause and whether the node was
// actually fixed. Drill profiles carry metadata drill=true; nothing here
// touches a profile without it.
package drill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// Net describes the drill network.
type Net struct {
	Image      string // an evmd image (evmd on PATH inside it)
	Dir        string // where node homes and the compose file live
	Validators int
	ChainID    string
	PortBase   int // comet RPC of node i = PortBase + 10*i (gRPC +2033, EVM +1488)
	Subnet     string
}

// Defaults fills unset fields.
func (n *Net) Defaults() {
	if n.Validators == 0 {
		n.Validators = 4
	}
	if n.ChainID == "" {
		n.ChainID = "cometcli-drill-1"
	}
	if n.PortBase == 0 {
		n.PortBase = 47057
	}
	if n.Subnet == "" {
		n.Subnet = "172.30.9"
	}
	if n.Dir == "" {
		d, _ := config.Path("drill")
		n.Dir = d
	}
}

const project = "cometcli-drill"

// Container is node i's container name.
func Container(i int) string { return fmt.Sprintf("%s-%d", project, i) }

// Profile is node i's profile name.
func Profile(i int) string { return fmt.Sprintf("drill%d", i) }

func (n *Net) rpc(i int) int  { return n.PortBase + 10*i }
func (n *Net) grpc(i int) int { return n.PortBase + 2033 + 10*i }
func (n *Net) evm(i int) int  { return n.PortBase + 1488 + 10*i }

func (n *Net) home(i int) string {
	return filepath.Join(n.Dir, "net", fmt.Sprintf("node%d", i), "evmd")
}

func (n *Net) compose() string { return filepath.Join(n.Dir, "compose.yml") }

// docker runs a docker command, returning combined output.
func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	out := strings.TrimSpace(b.String())
	if err != nil {
		return out, fmt.Errorf("docker %s: %v: %s", strings.Join(args[:min(2, len(args))], " "), err, lastLine(out))
	}
	return out, nil
}

func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return ls[len(ls)-1]
}

// Up creates the network: genesis with fast slashing (20-block window,
// 30s jail) and 60s votes, one container per validator, and a drill
// profile per node in cfg.
func (n *Net) Up(ctx context.Context, cfg *config.Config, log func(string)) error {
	n.Defaults()
	if n.Image == "" {
		return fmt.Errorf("an evmd image is required (--image)")
	}
	if out, _ := docker(ctx, "ps", "-a", "--filter", "label=com.docker.compose.project="+project, "-q"); out != "" {
		return fmt.Errorf("a drill network already exists — `cometcli drill testnet down` first")
	}
	_ = os.RemoveAll(filepath.Join(n.Dir, "net"))
	work, out := filepath.Join(n.Dir, "work"), filepath.Join(n.Dir, "net")
	for _, d := range []string{work, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	uid := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	log("generating genesis for " + strconv.Itoa(n.Validators) + " validators")
	if _, err := docker(ctx, "run", "--rm", "--user", uid, "-e", "HOME=/work", "-w", "/work", "-v", work+":/work", "-v", out+":/out",
		"--entrypoint", "evmd", n.Image, "testnet", "init-files", "--validator-count", strconv.Itoa(n.Validators),
		"--output-dir", "/out", "--chain-id", n.ChainID, "--keyring-backend", "test",
		"--starting-ip-address", n.Subnet+".2", "--commit-timeout", "1s", "--home", "/work/h"); err != nil {
		return err
	}
	denom, err := n.patchGenesis()
	if err != nil {
		return err
	}
	for i := 0; i < n.Validators; i++ {
		if err := n.patchConfigs(i, denom); err != nil {
			return err
		}
	}
	if err := os.WriteFile(n.compose(), []byte(n.composeFile(uid)), 0o644); err != nil {
		return err
	}
	log("starting containers")
	if _, err := docker(ctx, "compose", "-f", n.compose(), "up", "-d"); err != nil {
		return err
	}
	log("waiting for blocks")
	if err := n.waitBlocks(ctx, 3, 2*time.Minute); err != nil {
		return err
	}
	return n.addProfiles(ctx, cfg, denom)
}

// patchGenesis sets fast slashing and voting, returns the fee denom.
func (n *Net) patchGenesis() (string, error) {
	p := filepath.Join(n.home(0), "config", "genesis.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	var g map[string]any
	if err := json.Unmarshal(raw, &g); err != nil {
		return "", err
	}
	app, _ := g["app_state"].(map[string]any)
	set := func(module, key string, v any) {
		m, _ := app[module].(map[string]any)
		if m == nil {
			return
		}
		params, _ := m["params"].(map[string]any)
		if params != nil {
			params[key] = v
		}
	}
	set("slashing", "signed_blocks_window", "20")
	set("slashing", "downtime_jail_duration", "30s")
	set("gov", "voting_period", "60s")
	set("gov", "expedited_voting_period", "30s")
	set("gov", "max_deposit_period", "60s")
	denom := "atest"
	if evm, ok := app["evm"].(map[string]any); ok {
		if params, ok := evm["params"].(map[string]any); ok {
			if d, _ := params["evm_denom"].(string); d != "" {
				denom = d
			}
		}
	}
	patched, _ := json.MarshalIndent(g, "", "  ")
	for i := 0; i < n.Validators; i++ {
		if err := os.WriteFile(filepath.Join(n.home(i), "config", "genesis.json"), patched, 0o644); err != nil {
			return "", err
		}
	}
	return denom, nil
}

var (
	addrBookRe = regexp.MustCompile(`(?m)^addr_book_strict = true`)
	grpcAddrRe = regexp.MustCompile(`(?m)^address = "localhost:9090"`)
	minGasRe   = regexp.MustCompile(`(?m)^minimum-gas-prices = ".*"`)
)

func (n *Net) patchConfigs(i int, denom string) error {
	edit := func(name string, fn func(string) string) error {
		p := filepath.Join(n.home(i), "config", name)
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(p, []byte(fn(string(raw))), 0o644)
	}
	if err := edit("config.toml", func(s string) string { return addrBookRe.ReplaceAllString(s, "addr_book_strict = false") }); err != nil {
		return err
	}
	return edit("app.toml", func(s string) string {
		s = grpcAddrRe.ReplaceAllString(s, `address = "0.0.0.0:9090"`)
		return minGasRe.ReplaceAllString(s, fmt.Sprintf(`minimum-gas-prices = "1000000000%s"`, denom))
	})
}

func (n *Net) composeFile(uid string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\nnetworks:\n  drill:\n    name: %s\n    ipam:\n      config: [{subnet: %s.0/24}]\nservices:\n", project, project, n.Subnet)
	for i := 0; i < n.Validators; i++ {
		fmt.Fprintf(&b, `  node%d:
    image: %s
    pull_policy: never
    container_name: %s
    user: "%s"
    restart: unless-stopped
    entrypoint: ["evmd"]
    command: ["start", "--home", "/data/node", "--chain-id", "%s", "--json-rpc.api", "eth,net,web3,txpool"]
    volumes: ["%s:/data/node"]
    ports: ["127.0.0.1:%d:26657", "127.0.0.1:%d:9090", "127.0.0.1:%d:8545"]
    networks:
      drill: {ipv4_address: %s.%d}
`, i, n.Image, Container(i), uid, n.ChainID, n.home(i), n.rpc(i), n.grpc(i), n.evm(i), n.Subnet, i+2)
	}
	return b.String()
}

// Height is node i's latest height (0 when unreachable).
func (n *Net) Height(ctx context.Context, i int) int64 {
	out, err := docker(ctx, "exec", Container(i), "sh", "-c", "wget -qO- localhost:26657/status 2>/dev/null || curl -s localhost:26657/status")
	if err != nil {
		return 0
	}
	var st struct {
		Result struct {
			SyncInfo struct {
				Height string `json:"latest_block_height"`
			} `json:"sync_info"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(out), &st)
	h, _ := strconv.ParseInt(st.Result.SyncInfo.Height, 10, 64)
	return h
}

func (n *Net) waitBlocks(ctx context.Context, h int64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.Height(ctx, 0) >= h {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("the drill network produced no blocks in %s — check `docker logs %s`", timeout, Container(0))
}

func (n *Net) addProfiles(ctx context.Context, cfg *config.Config, denom string) error {
	evmChain := uint64(0)
	if raw, err := os.ReadFile(filepath.Join(n.home(0), "config", "app.toml")); err == nil {
		if m := regexp.MustCompile(`(?m)^evm-chain-id = (\d+)`).FindSubmatch(raw); m != nil {
			evmChain, _ = strconv.ParseUint(string(m[1]), 10, 64)
		}
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*config.Profile{}
	}
	for i := 0; i < n.Validators; i++ {
		valoper, err := docker(ctx, "exec", Container(i), "evmd", "keys", "show", fmt.Sprintf("node%d", i), "--bech", "val", "-a",
			"--keyring-backend", "test", "--home", "/data/node")
		if err != nil {
			return err
		}
		fb := n.grpc((i + 1) % n.Validators)
		cfg.Profiles[Profile(i)] = &config.Profile{
			ChainID: n.ChainID, EVMChainID: evmChain, Bech32Prefix: "cosmos", Role: "validator",
			Home: n.home(i), Binary: "evmd",
			Endpoints: config.Endpoints{
				Comet: fmt.Sprintf("tcp://127.0.0.1:%d", n.rpc(i)), GRPC: fmt.Sprintf("127.0.0.1:%d", n.grpc(i)),
				EVM: fmt.Sprintf("http://127.0.0.1:%d", n.evm(i)), FallbackGRPC: fmt.Sprintf("127.0.0.1:%d", fb),
			},
			Transport: config.Transport{Type: "local"},
			Service:   config.Service{Type: "docker", Unit: Container(i)},
			Signer: config.Signer{Mode: "container", ContainerKey: fmt.Sprintf("node%d", i), ContainerKeyring: "test",
				ContainerHome: "/data/node", ContainerBinary: "evmd"},
			Metadata: map[string]string{"drill": "true", "fee_denom": denom, "gas_price": "1000000000", "valoper": lastLine(valoper)},
		}
	}
	return cfg.Save()
}

// Down removes the containers, network, node data and drill profiles.
func (n *Net) Down(ctx context.Context, cfg *config.Config) error {
	n.Defaults()
	if _, err := os.Stat(n.compose()); err == nil {
		if _, err := docker(ctx, "compose", "-f", n.compose(), "down", "-v"); err != nil {
			return err
		}
	}
	_ = os.RemoveAll(filepath.Join(n.Dir, "net"))
	_ = os.RemoveAll(filepath.Join(n.Dir, "work"))
	for name, p := range cfg.Profiles {
		if IsDrill(p) {
			delete(cfg.Profiles, name)
		}
	}
	return cfg.Save()
}

// IsDrill reports whether a profile belongs to the drill network.
func IsDrill(p *config.Profile) bool { return p != nil && p.Metadata["drill"] == "true" }
