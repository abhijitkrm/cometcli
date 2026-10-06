package nettool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

const cfgTOML = `# Comma separated list of nodes to keep persistent connections to
# (persistent_peers below is what this edits)
persistent_peers = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@10.0.0.1:26656"

# Maximum pause when redialing a persistent peer
persistent_peers_max_dial_period = "0s"
`

const peerB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb@10.0.0.2:26656"

func setup(t *testing.T) (*toolkit.Context, string, *int) {
	t.Helper()
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "config"), 0o700)
	p := filepath.Join(home, "config", "config.toml")
	os.WriteFile(p, []byte(cfgTOML), 0o600)
	asked := 0
	c := &toolkit.Context{Context: context.Background(), Profile: &config.Profile{Name: "t", Home: home},
		AutoApproveBelow: toolkit.TierLocalChange,
		Approver:         func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { asked++; return true, nil }}
	c.SetHost(&host.Local{})
	return c, p, &asked
}

func TestAddRemovePeerPreservesFile(t *testing.T) {
	c, p, asked := setup(t)
	if _, err := (addPeerTool{}).Run(c, toolkit.Args{"peer": peerB}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `persistent_peers = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@10.0.0.1:26656,`+peerB+`"`) {
		t.Fatalf("add: %s", b)
	}
	if !strings.Contains(string(b), "# (persistent_peers below is what this edits)") || !strings.Contains(string(b), `persistent_peers_max_dial_period = "0s"`) {
		t.Fatalf("other lines changed: %s", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed to %v", fi.Mode().Perm())
	}
	if res, _ := (addPeerTool{}).Run(c, toolkit.Args{"peer": peerB}); !strings.Contains(res.Text, "already") || *asked != 1 {
		t.Fatalf("duplicate add: %v asked=%d", res, *asked)
	}
	if _, err := (rmPeerTool{}).Run(c, toolkit.Args{"peer": peerB}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != cfgTOML {
		t.Fatalf("round trip changed the file:\n%s", b)
	}
	res, err := (rmPeerTool{}).Run(c, toolkit.Args{"peer": peerB})
	if err != nil || !strings.Contains(res.Text, "not") || *asked != 2 {
		t.Fatalf("removing an absent peer: %v %v asked=%d", res, err, *asked)
	}
}

func TestPeerFormatValidated(t *testing.T) {
	c, _, asked := setup(t)
	for _, bad := range []string{"10.0.0.2:26656", "xyz@10.0.0.2:26656", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb@10.0.0.2", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb@:26656"} {
		if _, err := (addPeerTool{}).Run(c, toolkit.Args{"peer": bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if *asked != 0 {
		t.Fatal("asked to approve an invalid peer")
	}
}
