package common

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

func TestMain(m *testing.M) {
	fakenode.MaybeRunFakeDocker()
	os.Exit(m.Run())
}

// containerSetup: a profile whose key lives only in the node container.
func containerSetup(t *testing.T, keyring string) (*fakenode.Node, *toolkit.Context, *[]string) {
	t.Helper()
	tx.ConfirmTimeout, tx.ConfirmPoll = 300*time.Millisecond, 20*time.Millisecond
	n := fakenode.Start(t, "primium-1")
	p := n.Profile(t, "eth_secp256k1")
	p.Signer = config.Signer{Backend: "file", ContainerKey: "val"}
	p.Service = config.Service{Type: "docker", Unit: "primium-validator0"}
	fakenode.InstallFakeDocker(t, keyring, "correct horse")
	var asked []string
	c := &toolkit.Context{Context: context.Background(), Profile: p,
		Approver: func(_ *toolkit.Context, prompt string, _ toolkit.Tier, _ map[string]any) (bool, error) {
			asked = append(asked, prompt)
			return true, nil
		}}
	c.SetHost(&host.Local{})
	t.Cleanup(c.Close)
	return n, c, &asked
}

func TestContainerSigningTestKeyring(t *testing.T) {
	n, c, asked := containerSetup(t, "test")
	res, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	bs, rej, sims := n.Snapshot()
	if len(bs) != 1 || len(rej) != 0 {
		t.Fatalf("broadcast=%d rejected=%v", len(bs), rej)
	}
	// no public key on chain: gas isn't simulated, and the approval says so
	if sims != 0 || bs[0].Fee.GasLimit != tx.DefaultGas || !strings.Contains((*asked)[0], "not simulated") {
		t.Fatalf("sims=%d gas=%d", sims, bs[0].Fee.GasLimit)
	}
	if !strings.Contains((*asked)[0], "container primium-validator0 (key val, test keyring)") {
		t.Fatalf("approval doesn't name the container signer: %s", (*asked)[0])
	}
	if !strings.Contains((*asked)[0], fakenode.TestAddress()) || res.Data["code"] != uint32(0) {
		t.Fatalf("result = %v", res.Data)
	}
}

func TestContainerSigningSimulatesWithOnChainPubKeyAndAsksPassword(t *testing.T) {
	n, c, asked := containerSetup(t, "file")
	// the account has signed before: its pubkey is on chain
	var v []byte
	v = protowire.AppendTag(v, 1, protowire.BytesType)
	v = protowire.AppendBytes(v, mustPub(t))
	n.Set(func(n *fakenode.Node) {
		n.PubKey = &anypb.Any{TypeUrl: "/cosmos.evm.crypto.v1.ethsecp256k1.PubKey", Value: v}
	})
	var prompts []string
	c.Secret = func(_ *toolkit.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "correct horse", nil
	}
	c.Profile.Metadata["account"] = fakenode.TestAddress()
	if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{}); err != nil {
		t.Fatal(err)
	}
	bs, _, sims := n.Snapshot()
	if sims != 1 || bs[0].Fee.GasLimit != 140000 || len(prompts) != 1 || len(*asked) != 1 {
		t.Fatalf("sims=%d gas=%d prompts=%v asked=%d", sims, bs[0].Fee.GasLimit, prompts, len(*asked))
	}
	// declined → no password prompt, nothing signed
	prompts = nil
	c.Approver = func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return false, nil }
	if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{}); err == nil || len(prompts) != 0 {
		t.Fatalf("a declined tx must not ask for the keyring password: %v %v", err, prompts)
	}
	// wrong password → clear error, nothing broadcast
	c.Approver = func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return true, nil }
	c.Secret = func(*toolkit.Context, string) (string, error) { return "nope", nil }
	before, _, _ := n.Snapshot()
	if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{}); err == nil || !strings.Contains(err.Error(), "wrong keyring password") {
		t.Fatalf("err = %v", err)
	}
	if after, _, _ := n.Snapshot(); len(after) != len(before) {
		t.Fatal("broadcast despite a failed signing")
	}
}

func TestSignerChoiceWhenBothAvailable(t *testing.T) {
	n, c, _ := containerSetup(t, "test")
	c.Profile.Signer.Key = "ops" // the local keyring has it too (same test key)
	var offered []string
	c.Chooser = func(_ *toolkit.Context, _ string, opts []string) (int, error) {
		offered = opts
		return 1, nil // container
	}
	if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{}); err != nil {
		t.Fatal(err)
	}
	if len(offered) != 2 || !strings.Contains(offered[0], "cometcli keyring") || !strings.Contains(offered[1], "node container primium-validator0") {
		t.Fatalf("options = %v", offered)
	}
	if _, _, sims := n.Snapshot(); sims != 0 {
		t.Fatal("container signer was not used")
	}
	// headless: no chooser → explain, don't guess
	c.Chooser = nil
	if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{}); err == nil || !strings.Contains(err.Error(), "signer.mode") {
		t.Fatalf("err = %v", err)
	}
	c.Profile.Signer.Mode = "local"
	if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{}); err != nil {
		t.Fatalf("signer.mode local: %v", err)
	}
	if _, _, sims := n.Snapshot(); sims != 1 {
		t.Fatal("local signer not used under signer.mode local")
	}
}

func mustPub(t *testing.T) []byte {
	t.Helper()
	return fakenode.TestPubKey()
}
