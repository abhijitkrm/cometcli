package networktool

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/keys"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

// CometdutyImage is the cometduty release the generated files run; it must
// understand labels, api_token and webhook.secret.
const CometdutyImage = "ghcr.io/abhijitkrm/cometduty:v0.3.0"

// MonitorOpts shape the cometduty files for a network.
type MonitorOpts struct {
	Image      string // default CometdutyImage
	WebhookURL string // where alerts go (e.g. cometcli watch --listen); "" = no webhook
	Secret     string // webhook signing secret (with WebhookURL)
	APIToken   string // enables the silence API; "" = off
	// TelegramToken and TelegramChat send every alert to a Telegram chat
	// too (cometduty's own notifier); "" = no Telegram.
	TelegramToken string
	TelegramChat  string
	ChainOn       string // profile whose host runs the chain-role instance
	MetricsPort   int    // first sidecar's /metrics port (default 28686)
	// MetricsBind is where /metrics and the silence API listen (default
	// 127.0.0.1: cometcli reaches them through ssh; open it only for a
	// Prometheus on another machine).
	MetricsBind string
}

// MonitorFiles is what one host gets.
type MonitorFiles struct {
	Profile string            // the node it watches ("" for the chain instance)
	Dir     string            // directory name under the export root
	Files   map[string][]byte // name → content
	Port    int               // its /metrics port on the host
}

// RandomSecret returns 32 random bytes as hex.
func RandomSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// hostKey groups profiles that share a machine.
func hostKey(p *config.Profile) string {
	if p.Transport.Type == "ssh" {
		return p.Transport.Host
	}
	return "local"
}

// httpURL turns tcp://h:p (CometBFT's form) into http://h:p.
func httpURL(s string) string {
	if strings.HasPrefix(s, "tcp://") {
		return "http://" + strings.TrimPrefix(s, "tcp://")
	}
	if s != "" && !strings.Contains(s, "://") {
		return "http://" + s
	}
	return s
}

// cometMetricsURL is the node's own prometheus endpoint: CometBFT's 26660,
// shifted by the same offset as its RPC port.
func cometMetricsURL(rpc string) string {
	u, err := url.Parse(rpc)
	if err != nil || u.Port() == "" {
		return ""
	}
	port, _ := strconv.Atoi(u.Port())
	return fmt.Sprintf("http://%s:%d", u.Hostname(), port-26657+26660)
}

// CometdutyFiles renders a cometduty sidecar (config + compose) for each
// profile, plus the chain-role instance beside opts.ChainOn. Profiles must
// be on one chain. Nothing is written or run here.
func CometdutyFiles(chainID string, profiles []*config.Profile, o MonitorOpts) ([]MonitorFiles, error) {
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no nodes of %s", chainID)
	}
	if o.Image == "" {
		o.Image = CometdutyImage
	}
	if o.MetricsPort == 0 {
		o.MetricsPort = 28686
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	perHost := map[string]int{}
	var out []MonitorFiles
	chainNode := ChainHost(profiles, o.ChainOn)
	if chainNode == nil {
		return nil, fmt.Errorf("--chain-on %s isn't one of the nodes", o.ChainOn)
	}
	for _, p := range profiles {
		if p.ChainID != chainID {
			return nil, fmt.Errorf("profile %s is on %s, not %s", p.Name, p.ChainID, chainID)
		}
		if p.Endpoints.Comet == "" {
			return nil, fmt.Errorf("profile %s has no endpoints.comet", p.Name)
		}
		i := perHost[hostKey(p)]
		perHost[hostKey(p)]++
		port := o.MetricsPort + i
		node := map[string]any{
			"url": httpURL(p.Endpoints.Comet), "name": p.Name, "alert_if_down": true,
			"labels": map[string]string{"profile": p.Name},
		}
		if m := cometMetricsURL(httpURL(p.Endpoints.Comet)); m != "" {
			node["metrics_url"] = m
		}
		if p.Endpoints.EVM != "" {
			node["evm_rpc"] = httpURL(p.Endpoints.EVM)
		}
		docker := p.Service.Type == "docker" && p.Service.Unit != ""
		if p.Home != "" {
			node["home"] = "/node-home"
		}
		if docker {
			node["logs"] = map[string]any{"enabled": true, "source": "docker",
				"docker_host": fmt.Sprintf("tcp://127.0.0.1:%d", 2375+i), "container": p.Service.Unit}
		}
		chain := map[string]any{
			"chain_id": chainID, "role": "sidecar",
			"alerts": sidecarAlerts(o),
			"nodes":  []any{node},
		}
		if v := valoperOf(p); v != "" && p.IsValidator() {
			node["validator"] = p.Name
			chain["validators"] = []any{map[string]any{"valoper_address": v, "label": p.Name,
				"labels": map[string]string{"profile": p.Name}}}
		}
		cfgYAML, err := dutyConfig(o, port, map[string]any{chainID: chain})
		if err != nil {
			return nil, err
		}
		out = append(out, MonitorFiles{Profile: p.Name, Dir: p.Name, Port: port, Files: map[string][]byte{
			"config.yml":  cfgYAML,
			"compose.yml": []byte(dutyCompose(o.Image, "cometduty-"+p.Name, p.Home, docker, 2375+i)),
		}})
	}
	// the chain-role instance: chain-wide alerts from one node's RPC
	port := o.MetricsPort + perHost[hostKey(chainNode)]
	chain := map[string]any{
		"chain_id": chainID, "role": "chain",
		"alerts": map[string]any{"stalled_enabled": true, "stalled_minutes": 2, "alert_if_no_servers": true,
			"set_watch_enabled": true, "webhook": map[string]any{"enabled": o.WebhookURL != ""},
			"telegram": map[string]any{"enabled": o.TelegramChat != ""}},
		"nodes": []any{map[string]any{"url": httpURL(chainNode.Endpoints.Comet), "name": chainNode.Name,
			"labels": map[string]string{"profile": chainNode.Name}}},
	}
	cfgYAML, err := dutyConfig(o, port, map[string]any{chainID: chain})
	if err != nil {
		return nil, err
	}
	out = append(out, MonitorFiles{Dir: "chain", Port: port, Files: map[string][]byte{
		"config.yml":  cfgYAML,
		"compose.yml": []byte(dutyCompose(o.Image, "cometduty-chain-"+netspec.SafeName(chainID), "", false, 0)),
	}})
	return out, nil
}

// ChainHost is the profile whose host runs the chain-role instance.
func ChainHost(profiles []*config.Profile, chainOn string) *config.Profile {
	var first *config.Profile
	for _, p := range profiles {
		if p.Name == chainOn {
			return p
		}
		if first == nil || p.Name < first.Name {
			first = p
		}
	}
	if chainOn != "" {
		return nil
	}
	return first
}

func sidecarAlerts(o MonitorOpts) map[string]any {
	return map[string]any{
		"consecutive_enabled": true, "consecutive_missed": 15, "consecutive_priority": "critical",
		"percentage_enabled": true, "percentage_missed": 10, "percentage_priority": "warning",
		"alert_if_inactive": true, "alert_if_no_servers": false,
		"lag_enabled": true, "lag_blocks": 100,
		"evm_down_enabled": true, "evm_lag_enabled": true, "evm_lag_blocks": 30,
		"catching_up_severity": "warning", "catching_up_grace_minutes": 2, "catching_up_stuck_minutes": 15,
		"disk_free_bytes_alert": int64(5) << 30, "signer_stall_minutes": 5,
		"webhook":  map[string]any{"enabled": o.WebhookURL != ""},
		"telegram": map[string]any{"enabled": o.TelegramChat != ""},
	}
}

func dutyConfig(o MonitorOpts, port int, chains map[string]any) ([]byte, error) {
	bind := o.MetricsBind
	if bind == "" {
		bind = "127.0.0.1"
	}
	root := map[string]any{
		"prometheus_enabled": true, "prometheus_listen_port": port, "prometheus_bind": bind,
		"node_down_alert_minutes": 3, "node_down_alert_severity": "critical",
		"flap_suppression_minutes": 15,
		"chains":                   chains,
	}
	if o.WebhookURL != "" {
		root["webhook"] = map[string]any{"enabled": true, "url": o.WebhookURL, "secret": o.Secret}
	}
	if o.APIToken != "" {
		root["api_token"] = o.APIToken
	}
	if o.TelegramChat != "" {
		root["telegram"] = map[string]any{"enabled": true, "api_key": o.TelegramToken, "channel": o.TelegramChat}
	}
	b, err := yaml.Marshal(root)
	if err != nil {
		return nil, err
	}
	return append([]byte("# cometduty config written by cometcli network export (keep it mode 0600)\n"), b...), nil
}

// dutyCompose runs cometduty on the host network, reading the node home
// read-only as its owner (USER is filled in at deploy: the home's uid:gid).
func dutyCompose(image, name, home string, logs bool, proxyPort int) string {
	var b strings.Builder
	b.WriteString("# written by cometcli network export\nservices:\n  cometduty:\n")
	fmt.Fprintf(&b, "    image: %s\n    container_name: %s\n    restart: unless-stopped\n    read_only: true\n    network_mode: host\n", image, name)
	b.WriteString("    user: \"${USER_ID:-65532}\"\n")
	b.WriteString("    volumes:\n      - ./config.yml:/config/config.yml:ro\n      - ./state:/data\n")
	if home != "" {
		fmt.Fprintf(&b, "      - %s:/node-home:ro\n", home)
	}
	if logs {
		b.WriteString("  docker-proxy:\n    image: tecnativa/docker-socket-proxy:0.3.0\n")
		fmt.Fprintf(&b, "    container_name: %s-docker-proxy\n    restart: unless-stopped\n", name)
		b.WriteString("    environment: {CONTAINERS: \"1\", LOGS: \"1\", POST: \"0\", NETWORKS: \"0\", IMAGES: \"0\", EXEC: \"0\", INFO: \"0\", VERSION: \"0\", EVENTS: \"0\"}\n")
		b.WriteString("    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock:ro\n")
		fmt.Fprintf(&b, "    ports: [\"127.0.0.1:%d:2375\"]\n", proxyPort)
	}
	return b.String()
}

// MonitorDir is where a node's monitor files live on its host.
func MonitorDir(chainID, dir string) string {
	return path.Join("$HOME/.cometcli-monitor", netspec.SafeName(chainID), dir)
}

// DeployMonitor writes one host's files to MonitorDir and starts cometduty
// there, after the operator approves. It runs cometduty as the owner of
// the node home, so the read-only home mount is readable.
func DeployMonitor(c *toolkit.Context, chainID string, f MonitorFiles, home string) (string, error) {
	h, err := c.Host()
	if err != nil {
		return "", err
	}
	dirOut, code, err := h.Run(c, "echo "+MonitorDir(chainID, f.Dir))
	dir := strings.TrimSpace(dirOut)
	if err != nil || code != 0 || dir == "" {
		return "", fmt.Errorf("resolve the monitor dir: %v", err)
	}
	owner := "65532"
	if home != "" {
		if o, code, _ := h.Run(c, "stat -c '%u:%g' "+common.ShellQ(home)+" 2>/dev/null || stat -f '%u:%g' "+common.ShellQ(home)); code == 0 && strings.TrimSpace(o) != "" {
			owner = strings.TrimSpace(o)
		}
	}
	if err := c.Approve(fmt.Sprintf("start cometduty on %s (%s, /metrics on :%d)", h, dir, f.Port), toolkit.TierLocalChange,
		map[string]any{"dir": dir, "image": CometdutyImage, "runs_as": owner, "metrics_port": f.Port}); err != nil {
		return "", err
	}
	if _, code, err := h.Run(c, fmt.Sprintf("mkdir -p %s/state && chmod 700 %s && chown %s %s/state 2>/dev/null || sudo -n chown %s %s/state",
		common.ShellQ(dir), common.ShellQ(dir), owner, common.ShellQ(dir), owner, common.ShellQ(dir))); err != nil || code != 0 {
		return "", fmt.Errorf("create %s: %v (exit %d)", dir, err, code)
	}
	sum := "sha256sum " + common.ShellQ(dir) + "/config.yml 2>/dev/null | cut -c1-64"
	before, _, _ := h.Run(c, sum)
	for name, content := range f.Files {
		if err := h.WriteFile(c, path.Join(dir, name), content, 0o600); err != nil {
			return "", fmt.Errorf("write %s: %w", name, err)
		}
	}
	// the container user must read config.yml
	_, _, _ = h.Run(c, fmt.Sprintf("chown %s %s/config.yml 2>/dev/null || sudo -n chown %s %s/config.yml", owner, common.ShellQ(dir), owner, common.ShellQ(dir)))
	out, code, err := h.Run(c, fmt.Sprintf("cd %s && USER_ID=%s docker compose up -d 2>&1 | tail -3", common.ShellQ(dir), owner))
	if err != nil || code != 0 {
		return "", fmt.Errorf("docker compose up failed (exit %d): %v %s", code, err, strings.TrimSpace(out))
	}
	// compose only recreates for a changed compose file: a running
	// cometduty must be restarted to read a new config (its state is kept)
	after, _, _ := h.Run(c, sum)
	if b := strings.TrimSpace(before); b != "" && b != strings.TrimSpace(after) {
		if out, code, err := h.Run(c, fmt.Sprintf("cd %s && USER_ID=%s docker compose restart cometduty 2>&1 | tail -3", common.ShellQ(dir), owner)); err != nil || code != 0 {
			return "", fmt.Errorf("restart cometduty for the new config (exit %d): %v %s", code, err, strings.TrimSpace(out))
		}
		return fmt.Sprintf("cometduty restarted with the new config in %s (as %s), metrics on :%d", dir, owner, f.Port), nil
	}
	return fmt.Sprintf("cometduty running in %s (as %s), metrics on :%d", dir, owner, f.Port), nil
}

// StandalonePort is the standalone instance's default /metrics port: apart
// from the sidecars' 28686+, so both layouts can share a host.
const StandalonePort = 28696

// StandaloneFiles renders one cometduty instance in the standalone role
// for a whole network: every validator's signing, the active set, chain
// health, and each node it can reach. It runs on the opts.ChainOn host
// (default: the first node); a node whose RPC is loopback-only on another
// host can't be reached from there and is returned in skipped. rpcs are
// extra endpoints (a public RPC) used as data sources, not paged when down.
// It has no node home or logs: disk, signer-state and log alerts need
// sidecars.
func StandaloneFiles(chainID string, profiles []*config.Profile, o MonitorOpts, rpcs []string) (MonitorFiles, []string, error) {
	if o.Image == "" {
		o.Image = CometdutyImage
	}
	if o.MetricsPort == 0 {
		o.MetricsPort = StandalonePort
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	on := ChainHost(profiles, o.ChainOn)
	if on == nil {
		return MonitorFiles{}, nil, fmt.Errorf("--chain-on %s isn't one of the nodes", o.ChainOn)
	}
	reach := func(p *config.Profile, u string) bool {
		return u != "" && (!isLoopback(u) || hostKey(p) == hostKey(on))
	}
	var nodes, validators []any
	var skipped []string
	seen := map[string]bool{}
	for _, p := range profiles {
		if p.ChainID != chainID {
			return MonitorFiles{}, nil, fmt.Errorf("profile %s is on %s, not %s", p.Name, p.ChainID, chainID)
		}
		if v := valoperOf(p); v != "" && p.IsValidator() && !seen[v] {
			seen[v] = true
			validators = append(validators, map[string]any{"valoper_address": v, "label": p.Name,
				"labels": map[string]string{"profile": p.Name}})
		}
		rpc := httpURL(p.Endpoints.Comet)
		if !reach(p, rpc) {
			skipped = append(skipped, p.Name)
			continue
		}
		node := map[string]any{"url": rpc, "name": p.Name, "alert_if_down": true,
			"labels": map[string]string{"profile": p.Name}}
		if m := cometMetricsURL(rpc); m != "" {
			node["metrics_url"] = m
		}
		if evm := httpURL(p.Endpoints.EVM); reach(p, evm) {
			node["evm_rpc"] = evm
		}
		if valoperOf(p) != "" && p.IsValidator() {
			node["validator"] = p.Name
		}
		nodes = append(nodes, node)
	}
	for i, r := range rpcs {
		nodes = append(nodes, map[string]any{"url": httpURL(r), "name": fmt.Sprintf("rpc-%d", i+1), "alert_if_down": false})
	}
	if len(nodes) == 0 {
		return MonitorFiles{}, skipped, fmt.Errorf("no node's RPC is reachable from %s (loopback-only on other hosts) — pass --rpc with an endpoint it can reach, or use the sidecar layout", on.Name)
	}
	alerts := sidecarAlerts(o)
	alerts["stalled_enabled"], alerts["stalled_minutes"] = true, 2
	alerts["alert_if_no_servers"], alerts["set_watch_enabled"] = true, true
	delete(alerts, "disk_free_bytes_alert") // no node home here
	delete(alerts, "signer_stall_minutes")
	chain := map[string]any{"chain_id": chainID, "alerts": alerts, "nodes": nodes}
	if len(validators) > 0 {
		chain["validators"] = validators
	}
	cfgYAML, err := dutyConfig(o, o.MetricsPort, map[string]any{chainID: chain})
	if err != nil {
		return MonitorFiles{}, nil, err
	}
	return MonitorFiles{Dir: "standalone", Port: o.MetricsPort, Files: map[string][]byte{
		"config.yml":  cfgYAML,
		"compose.yml": []byte(dutyCompose(o.Image, "cometduty-"+netspec.SafeName(chainID), "", false, 0)),
	}}, skipped, nil
}

func isLoopback(u string) bool {
	h := hostOfURL(u)
	return h == "127.0.0.1" || h == "localhost" || h == "::1" || h == "0.0.0.0"
}

func hostOfURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return p.Hostname()
}

// valoperOf is a validator profile's operator address: metadata.valoper,
// or derived from metadata.account (same bytes) when only that was saved.
func valoperOf(p *config.Profile) string {
	if v := p.Metadata["valoper"]; v != "" {
		return v
	}
	if a := p.Metadata["account"]; a != "" {
		if v, err := keys.ValoperFromAcc(a); err == nil {
			return v
		}
	}
	return ""
}
