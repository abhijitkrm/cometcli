package common

import (
	"context"
	"strings"
	"testing"
	"time"

	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	"google.golang.org/protobuf/proto"

	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

func setup(t *testing.T, algo string) (*fakenode.Node, *toolkit.Context, *[]string) {
	t.Helper()
	tx.ConfirmTimeout, tx.ConfirmPoll = 300*time.Millisecond, 20*time.Millisecond
	n := fakenode.Start(t, "primium-1")
	p := n.Profile(t, algo)
	var asked []string
	c := &toolkit.Context{Context: context.Background(), Profile: p, AutoApproveBelow: toolkit.TierOnChain,
		Approver: func(_ *toolkit.Context, prompt string, tier toolkit.Tier, _ map[string]any) (bool, error) {
			if tier != toolkit.TierOnChain {
				t.Errorf("tx approval asked at tier %s", tier)
			}
			asked = append(asked, prompt)
			return true, nil
		}}
	t.Cleanup(c.Close)
	return n, c, &asked
}

func send(to string) tx.Msgs {
	return tx.Msgs{&bankv1beta1.MsgSend{FromAddress: "x", ToAddress: to, Amount: []*basev1beta1.Coin{{Denom: "adex", Amount: "5"}}}}
}

func TestBroadcastHappyPathBothKeyTypes(t *testing.T) {
	for _, algo := range []string{"eth_secp256k1", "secp256k1"} {
		t.Run(algo, func(t *testing.T) {
			n, c, asked := setup(t, algo)
			n.Set(func(n *fakenode.Node) { n.Seq = 3 })
			res, err := BroadcastMsgs(c, send("cosmos1dest"), "memo-1", map[string]string{"why": "test"}, tx.Options{})
			if err != nil {
				t.Fatal(err)
			}
			bs, rej, sims := n.Snapshot()
			if len(bs) != 1 || len(rej) != 0 || sims != 1 || len(*asked) != 1 {
				t.Fatalf("broadcasts=%d rejected=%v sims=%d asked=%d", len(bs), rej, sims, len(*asked))
			}
			b := bs[0]
			if b.Seq != 3 || b.Memo != "memo-1" || b.Msgs[0].TypeUrl != "/cosmos.bank.v1beta1.MsgSend" {
				t.Fatalf("tx = %+v", b)
			}
			var m bankv1beta1.MsgSend
			proto.Unmarshal(b.Msgs[0].Value, &m)
			if m.ToAddress != "cosmos1dest" || m.Amount[0].Amount != "5" {
				t.Fatalf("msg = %+v", &m)
			}
			// gas = simulated 100000 × 1.4; fee = gas × 1e9
			if b.Fee.GasLimit != 140000 || b.Fee.Amount[0].Amount != "140000000000000" || b.Fee.Amount[0].Denom != "adex" {
				t.Fatalf("fee = %+v", b.Fee)
			}
			if !strings.Contains((*asked)[0], `"sequence": 3`) || res.Data["height"] == nil || res.Data["code"] != uint32(0) {
				t.Fatalf("approval doc / result: %q %v", (*asked)[0], res.Data)
			}
		})
	}
}

func TestBroadcastDeniedNeverReachesTheNode(t *testing.T) {
	n, c, _ := setup(t, "eth_secp256k1")
	c.Approver = func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return false, nil }
	if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{}); err == nil {
		t.Fatal("denied tx returned no error")
	}
	if bs, _, _ := n.Snapshot(); len(bs) != 0 {
		t.Fatal("a denied transaction was broadcast")
	}
	// even with every lower tier autopiloted
	c.AutoApproveBelow = toolkit.TierOnChain
	if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{}); err == nil {
		t.Fatal("on-chain must never be auto-approved")
	}
}

func TestSequenceDriftHealsOnceWithReapproval(t *testing.T) {
	n, c, asked := setup(t, "eth_secp256k1")
	stale := uint64(4)
	// the account query lags (a tx is pending): simulation and broadcast
	// see the real next sequence 5
	n.Set(func(n *fakenode.Node) { n.Seq, n.StaleSeq = 5, &stale })
	if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{GasLimit: 200000}); err != nil {
		t.Fatal(err)
	}
	bs, rej, _ := n.Snapshot()
	if len(bs) != 1 || bs[0].Seq != 5 || len(rej) != 1 || !strings.Contains(rej[0], "expected 5, got 4") {
		t.Fatalf("broadcasts=%+v rejected=%v", bs, rej)
	}
	if len(*asked) != 2 {
		t.Fatalf("rebuilt tx must be re-approved: asked %d times", len(*asked))
	}
}

func TestPinnedSequenceIsNeverRebuilt(t *testing.T) {
	n, c, _ := setup(t, "eth_secp256k1")
	n.Set(func(n *fakenode.Node) { n.Seq = 9 })
	_, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{GasLimit: 200000}.WithAccount(7, 8))
	if err == nil || !strings.Contains(err.Error(), "expected 9, got 8") {
		t.Fatalf("err = %v", err)
	}
	if bs, rej, _ := n.Snapshot(); len(bs) != 0 || len(rej) != 1 {
		t.Fatal("pinned seq retried")
	}
}

func TestCheckTxRejectionAndBlockFailure(t *testing.T) {
	n, c, _ := setup(t, "eth_secp256k1")
	n.Set(func(n *fakenode.Node) { n.CheckCode, n.CheckLog = 13, "insufficient fee" })
	if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{}); err == nil || !strings.Contains(err.Error(), "mempool rejected tx (code 13): insufficient fee") {
		t.Fatalf("checktx: %v", err)
	}
	n.Set(func(n *fakenode.Node) { n.CheckCode, n.DeliverCode, n.DeliverLog = 0, 5, "insufficient funds" })
	if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{}); err == nil || !strings.Contains(err.Error(), "tx failed in block (code 5)") {
		t.Fatalf("deliver: %v", err)
	}
}

func TestUnconfirmedIsReportedPendingNotFailed(t *testing.T) {
	n, c, _ := setup(t, "eth_secp256k1")
	n.Set(func(n *fakenode.Node) { n.NeverCommit = true })
	res, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{})
	if err != nil || res.Data["pending"] != true || !strings.Contains(res.Text, "cometcli tx get") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestWrongChainIDFailsSignatureCheck(t *testing.T) {
	n, c, _ := setup(t, "eth_secp256k1")
	c.Profile.ChainID = "other-chain-1" // the key signs for the wrong chain
	_, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{})
	if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("err = %v", err)
	}
	if bs, _, _ := n.Snapshot(); len(bs) != 0 {
		t.Fatal("bad signature accepted")
	}
}

func TestDecScaled(t *testing.T) {
	cases := map[string]string{
		"0.10": "100000000000000000", "1": "1000000000000000000", "0.05": "50000000000000000",
		"100000000000000000": "100000000000000000", // already scaled
	}
	for in, want := range cases {
		if got, err := DecScaled(in); err != nil || got != want {
			t.Errorf("DecScaled(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := DecScaled("0.0000000000000000001"); err == nil {
		t.Error("19 decimals accepted")
	}
	if DecPct("100000000000000000") != "10" || DecFrac("50000000000000000") != "0.05" {
		t.Error("DecPct/DecFrac")
	}
}
