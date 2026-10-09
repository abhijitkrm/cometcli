package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/testutil/sshd"
)

func TestSSHTestReportsAccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	_ = os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	key, pub := sshd.ClientKey(t, home, "")
	s := sshd.Start(t, pub)
	cm := fakenode.StartComet(t)
	nodeHome := filepath.Join(home, "node")
	_ = os.MkdirAll(filepath.Join(nodeHome, "config"), 0o755)
	_ = os.WriteFile(filepath.Join(nodeHome, "config", "config.toml"), []byte("moniker = \"x\"\n"), 0o644)
	p := &config.Profile{Name: "val1", Home: nodeHome,
		Transport: config.Transport{Type: "ssh", Host: s.Host, Port: s.Port, User: "tester", KeyFile: key},
		Endpoints: config.Endpoints{Comet: cm.URL, GRPC: "127.0.0.1:1"}}

	var out strings.Builder
	// a new host: declining to trust it stops before any command runs
	if err := sshTest(context.Background(), p, strings.NewReader("n\n"), &out); err == nil || !strings.Contains(err.Error(), "known host") {
		t.Fatalf("declined host key should fail: %v", err)
	}
	out.Reset()
	if err := sshTest(context.Background(), p, strings.NewReader("y\n"), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"✓ connected, host key verified",
		"✓ node config " + nodeHome,
		"✓ CometBFT RPC " + strings.TrimPrefix(cm.URL, "http://") + " (through ssh)",
		"! gRPC 127.0.0.1:1 (through ssh)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}
