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
	sets, err := CometdutyFiles("c-1", monitorProfiles(), MonitorOpts{WebhookURL: "https://cc.example/alerts", Secret: "sec", APIToken: "0123456789abcdef0123", ChainOn: "arch"})
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
		if strings.Contains(c, "api_token") || strings.Contains(c, "secret") || strings.Contains(c, "url: https://") {
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
	sets, err := CometdutyFiles("c-1", monitorProfiles(), MonitorOpts{WebhookURL: "https://cc.example/alerts", Secret: "sec", APIToken: "0123456789abcdef0123"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sets {
		p := filepath.Join(t.TempDir(), "config.yml")
		_ = os.WriteFile(p, s.Files["config.yml"], 0o600)
		out, err := exec.Command(bin, "validate", "-f", p).CombinedOutput()
		if err != nil || strings.Contains(string(out), "error") {
			t.Errorf("%s: cometduty validate: %v\n%s", s.Dir, err, out)
		}
	}
}
