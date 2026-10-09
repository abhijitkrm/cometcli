package toolkit_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/testutil/sshd"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// A node whose RPC and gRPC listen only on its own localhost is reached
// through the profile's ssh connection.
func TestNodeEndpointsGoThroughSSH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	_ = os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	key, pub := sshd.ClientKey(t, home, "")
	s := sshd.Start(t, pub)
	_ = os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"),
		[]byte(knownhosts.Line([]string{knownhosts.Normalize(s.Addr)}, s.HostKeys[0].PublicKey())+"\n"), 0o600)

	cm := fakenode.StartComet(t)
	node := fakenode.Start(t, "test-1")
	p := &config.Profile{Name: "remote", ChainID: "test-1",
		Transport: config.Transport{Type: "ssh", Host: s.Host, Port: s.Port, User: "tester", KeyFile: key},
		Endpoints: config.Endpoints{Comet: cm.URL, GRPC: node.Addr}}
	c := &toolkit.Context{Context: context.Background(), Profile: p}
	defer c.Close()

	cc, err := c.Comet()
	if err != nil {
		t.Fatal(err)
	}
	st, err := cc.Status(c)
	if err != nil || st.SyncInfo.LatestBlockHeight == 0 {
		t.Fatalf("comet status through ssh: %v", err)
	}
	g, err := c.GRPC()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Staking.Validators(c, &stakingv1beta1.QueryValidatorsRequest{}); err != nil {
		t.Fatalf("grpc through ssh: %v", err)
	}
	if n := s.Forwards.Load(); n < 2 {
		t.Fatalf("RPC and gRPC should both be tunneled, forwards=%d", n)
	}
}
