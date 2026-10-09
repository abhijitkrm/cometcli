package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/testutil/sshd"
)

func TestMinGasPriceDenom(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, "config"), 0o755)
	for app, want := range map[string][2]string{
		"minimum-gas-prices = \"1000000000atest\"\n":       {"1000000000", "atest"},
		"minimum-gas-prices = \"0.025uatom,0.001stake\"\n": {"0.025", "uatom"},
		"pruning = \"default\"\n":                          {"", ""},
	} {
		_ = os.WriteFile(filepath.Join(home, "config", "app.toml"), []byte(app), 0o644)
		if a, d := minGasPriceDenom(context.Background(), &host.Local{}, home); a != want[0] || d != want[1] {
			t.Errorf("%q → %q %q, want %v", app, a, d, want)
		}
	}
	if a, d := minGasPriceDenom(context.Background(), &host.Local{}, t.TempDir()); a != "" || d != "" {
		t.Error("missing app.toml should yield nothing")
	}
}

// init over ssh: the wizard trusts the new host key, reaches the node's
// localhost RPC/gRPC through the connection and reads app.toml remotely.
func TestInitOverSSH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COMETCLI_HOME", filepath.Join(home, ".cometcli"))
	t.Setenv("SSH_AUTH_SOCK", "")
	_ = os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	key, pub := sshd.ClientKey(t, home, "")
	s := sshd.Start(t, pub)
	cm := fakenode.StartComet(t)
	node := fakenode.Start(t, "mychain-1")
	nodeHome := filepath.Join(home, "node")
	_ = os.MkdirAll(filepath.Join(nodeHome, "config"), 0o755)
	_ = os.WriteFile(filepath.Join(nodeHome, "config", "app.toml"), []byte("minimum-gas-prices = \"7atest\"\n"), 0o644)

	answers := []string{
		"val1", "ssh", s.Host, "tester", fmt.Sprint(s.Port), key, "", // name, transport, host, user, port, key, jump
		"y",    // trust the host key
		"none", // service manager
		cm.URL, "cosmos", node.Addr, "", "none", "evmd", nodeHome,
		"", // signer question (decline)
	}
	var out strings.Builder
	err := runInit(context.Background(), strings.NewReader(strings.Join(answers, "\n")+"\n"), &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{"connected over ssh", "chain-id mychain-1", "fees from app.toml: 7atest"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if s.Forwards.Load() < 2 {
		t.Errorf("RPC and gRPC probes should go through ssh (forwards=%d)", s.Forwards.Load())
	}
	if kh, _ := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts")); !strings.Contains(string(kh), s.Host) {
		t.Error("trusted host key wasn't saved to known_hosts")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.ActiveProfile("")
	if err != nil {
		t.Fatal(err)
	}
	if p.Transport.Type != "ssh" || p.Transport.Host != s.Host || p.Metadata["fee_denom"] != "atest" || p.Endpoints.EVM != "" {
		t.Errorf("saved profile: %+v %v", p.Transport, p.Metadata)
	}
}
