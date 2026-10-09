package common

import (
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

// Signing is key access: asked every time with what, where and why —
// whatever auto-approval the session has.
func TestKeyUseIsAlwaysAskedWithDetail(t *testing.T) {
	n, c, asked := setup(t, "eth_secp256k1")
	c.AutoApproveBelow = toolkit.TierOnChain + 1 // as permissive as a context can be
	c.Purpose = "unjail after the node was restarted and synced"
	var details []map[string]any
	approve := c.Approver
	c.Approver = func(cc *toolkit.Context, prompt string, tier toolkit.Tier, d map[string]any) (bool, error) {
		details = append(details, d)
		return approve(cc, prompt, tier, d)
	}
	for i := 0; i < 2; i++ {
		if _, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(*asked) != 2 {
		t.Fatalf("each signature must be approved: asked %d times for 2", len(*asked))
	}
	p := (*asked)[0]
	tb, _ := c.TxSigner()
	if tb.Address() == "" {
		t.Fatal("no signer address")
	}
	for _, want := range []string{"KEY ACCESS", "account: " + tb.Address(), "cometcli keyring", "why:     unjail after", "this one signature", "/cosmos.bank.v1beta1.MsgSend"} {
		if !strings.Contains(p, want) {
			t.Errorf("approval lacks %q:\n%s", want, p)
		}
	}
	if details[0][toolkit.KeyUseMark] != true {
		t.Error("key use isn't marked for front-ends")
	}
	if bs, _, _ := n.Snapshot(); len(bs) != 2 {
		t.Fatalf("broadcasts = %d", len(bs))
	}
}

func TestDeclinedKeyUseNeverSigns(t *testing.T) {
	n, c, _ := setup(t, "secp256k1")
	c.Approver = func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return false, nil }
	_, err := BroadcastMsgs(c, send("cosmos1dest"), "", nil, tx.Options{})
	if err == nil || !strings.Contains(err.Error(), "didn't approve using the key") {
		t.Fatalf("want a key-use denial, got %v", err)
	}
	// gas was simulated with an unsigned tx: the key wasn't touched
	bs, _, sims := n.Snapshot()
	if len(bs) != 0 || sims != 1 {
		t.Fatalf("broadcasts=%d sims=%d", len(bs), sims)
	}
	if s := n.LastSimSignatures(); s != 1 || n.LastSimSigned() {
		t.Fatalf("simulation tx should carry one empty signature (got %d, signed=%v)", s, n.LastSimSigned())
	}
}
