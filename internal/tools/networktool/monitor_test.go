package networktool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/abhijitkrm/cometcli/internal/config"
)

func monitorProfiles() []*config.Profile {
	val := func(name, host, rpc string) *config.Profile {
		return &config.Profile{Name: name, ChainID: "c-1", Role: "validator", Home: "/home/u/.cometcli-nodes/" + name,
			Transport: config.Transport{Type: "ssh", Host: host},
			Endpoints: config.Endpoints{Comet: rpc, EVM: "http://127.0.0.1:8545"},
			Service:   config.Service{Type: "docker", Unit: "c-1-validator"},
			Metadata:  map[string]string{"valoper": "cosmosvaloper1" + strings.Repeat("q", 38)}}
	}
	arch := val("arch", "10.0.0.3", "tcp://127.0.0.1:26657")
	arch.Role, arch.Metadata = "archive", nil
	return []*config.Profile{val("val2", "10.0.0.2", "tcp://127.0.0.1:26657"), val("val1", "10.0.0.1", "tcp://127.0.0.1:26657"),
		val("val1b", "10.0.0.1", "tcp://127.0.0.1:26757"), arch}
}

func TestCometdutyFiles(t *testing.T) {
	sets, err := CometdutyFiles("c-1", monitorProfiles(), MonitorOpts{WebhookURL: "https://cc.example/alerts", Secret: "sec", APIToken: "0123456789abcdef0123", ChainOn: "arch",
		TelegramToken: "123:tok", TelegramChat: "-10042"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 5 {
		t.Fatalf("%d sets", len(sets))
	}
	ports := map[string]int{}
	for _, s := range sets {
		ports[s.Dir] = s.Port
		var cfg map[string]any
		if err := yaml.Unmarshal(s.Files["config.yml"], &cfg); err != nil {
			t.Fatalf("%s: %v", s.Dir, err)
		}
		if cfg["api_token"] != "0123456789abcdef0123" || cfg["webhook"].(map[string]any)["secret"] != "sec" {
			t.Errorf("%s: secrets not set: %v", s.Dir, cfg)
		}
		tg, _ := cfg["telegram"].(map[string]any)
		if tg["api_key"] != "123:tok" || tg["channel"] != "-10042" || tg["enabled"] != true {
			t.Errorf("%s: telegram not set: %v", s.Dir, tg)
		}
		ch := cfg["chains"].(map[string]any)["c-1"].(map[string]any)["alerts"].(map[string]any)
		if ch["telegram"].(map[string]any)["enabled"] != true {
			t.Errorf("%s: chain alerts don't enable telegram: %v", s.Dir, ch)
		}
	}
	// two nodes on one host get different metrics ports; the chain
	// instance takes the next one on its host
	if ports["val1"] == ports["val1b"] || ports["chain"] != 28687 || ports["val2"] != 28686 {
		t.Errorf("ports %v", ports)
	}
	v1b := string(sets[2].Files["config.yml"])
	for _, want := range []string{"url: http://127.0.0.1:26757", "metrics_url: http://127.0.0.1:26760", "profile: val1b", "role: sidecar", "valoper_address:"} {
		if !strings.Contains(v1b, want) {
			t.Errorf("val1b config lacks %q:\n%s", want, v1b)
		}
	}
	if arch := string(sets[0].Files["config.yml"]); strings.Contains(arch, "valoper_address") {
		t.Errorf("archive watched as a validator:\n%s", arch)
	}
	if !strings.Contains(v1b, "prometheus_bind: 127.0.0.1") {
		t.Error("metrics not on loopback by default")
	}
	if c := string(sets[2].Files["compose.yml"]); !strings.Contains(c, "/home/u/.cometcli-nodes/val1b:/node-home:ro") || !strings.Contains(c, "network_mode: host") {
		t.Errorf("compose:\n%s", c)
	}
}

func TestCometdutyFilesWithoutExtras(t *testing.T) {
	sets, err := CometdutyFiles("c-1", monitorProfiles(), MonitorOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sets {
		c := string(s.Files["config.yml"])
		if strings.Contains(c, "api_token") || strings.Contains(c, "secret") || strings.Contains(c, "url: https://") ||
			strings.Contains(c, "api_key") {
			t.Errorf("%s: optional parts present without being asked:\n%s", s.Dir, c)
		}
	}
	if _, err := CometdutyFiles("other-1", monitorProfiles(), MonitorOpts{}); err == nil {
		t.Error("profiles of another chain accepted")
	}
}

// The files must load in cometduty itself (its YAML is strict). Runs when
// COMETDUTY_BIN points at a cometduty build.
func TestCometdutyAcceptsExport(t *testing.T) {
	bin := os.Getenv("COMETDUTY_BIN")
	if bin == "" {
		t.Skip("COMETDUTY_BIN not set")
	}
	sets, err := CometdutyFiles("c-1", monitorProfiles(), MonitorOpts{WebhookURL: "https://cc.example/alerts", Secret: "sec", APIToken: "0123456789abcdef0123",
		TelegramToken: "123:tok", TelegramChat: "-10042"})
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := StandaloneFiles("c-1", monitorProfiles(), MonitorOpts{WebhookURL: "https://cc.example/alerts", Secret: "sec",
		APIToken: "0123456789abcdef0123", ChainOn: "val1"}, []string{"https://rpc.example:443"})
	if err != nil {
		t.Fatal(err)
	}
	sets = append(sets, f)
	for _, s := range sets {
		p := filepath.Join(t.TempDir(), "config.yml")
		_ = os.WriteFile(p, s.Files["config.yml"], 0o600)
		out, err := exec.Command(bin, "validate", "-f", p).CombinedOutput()
		if err != nil || strings.Contains(string(out), "error") {
			t.Errorf("%s: cometduty validate: %v\n%s", s.Dir, err, out)
		}
	}
}

func TestStandaloneFiles(t *testing.T) {
	f, skipped, err := StandaloneFiles("c-1", monitorProfiles(), MonitorOpts{ChainOn: "val1", TelegramToken: "1:t", TelegramChat: "-1"},
		[]string{"https://rpc.example:443"})
	if err != nil {
		t.Fatal(err)
	}
	// val2 and arch listen on loopback on other hosts: not reachable from val1's
	if strings.Join(skipped, ",") != "arch,val2" || f.Dir != "standalone" || f.Port != StandalonePort {
		t.Fatalf("skipped %v dir %s port %d", skipped, f.Dir, f.Port)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(f.Files["config.yml"], &cfg); err != nil {
		t.Fatal(err)
	}
	ch := cfg["chains"].(map[string]any)["c-1"].(map[string]any)
	if _, ok := ch["role"]; ok {
		t.Errorf("standalone has a role: %v", ch["role"])
	}
	var names []string
	for _, n := range ch["nodes"].([]any) {
		names = append(names, n.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "val1,val1b,rpc-1" {
		t.Errorf("nodes %v", names)
	}
	if rpc := ch["nodes"].([]any)[2].(map[string]any); rpc["alert_if_down"] != false {
		t.Errorf("extra RPC pages when down: %v", rpc)
	}
	// every validator's signing, deduplicated by operator address
	if v := ch["validators"].([]any); len(v) != 1 {
		t.Errorf("validators %v", v)
	}
	a := ch["alerts"].(map[string]any)
	if a["stalled_enabled"] != true || a["set_watch_enabled"] != true || a["alert_if_no_servers"] != true ||
		a["telegram"].(map[string]any)["enabled"] != true || a["disk_free_bytes_alert"] != nil || a["disk_fill_hours_alert"] != nil {
		t.Errorf("alerts %v", a)
	}
	if c := string(f.Files["compose.yml"]); strings.Contains(c, "node-home") || strings.Contains(c, "docker-proxy") {
		t.Errorf("standalone mounts a node:\n%s", c)
	}
	// nothing reachable: a clear error
	if _, _, err := StandaloneFiles("c-1", monitorProfiles(), MonitorOpts{ChainOn: "arch"}, nil); err != nil {
		t.Errorf("arch reaches its own RPC: %v", err)
	}
	ps := monitorProfiles()[:1] // val2 only, run on val2: its own loopback RPC
	if _, _, err := StandaloneFiles("c-1", ps, MonitorOpts{}, nil); err != nil {
		t.Errorf("own host: %v", err)
	}
}
