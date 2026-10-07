package txtool

import (
	"context"
	"testing"
	"time"

	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	"google.golang.org/protobuf/proto"

	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

func TestParseCoin(t *testing.T) {
	good := map[string][2]string{
		"1000000atest":              {"1000000", "atest"},
		"1500000000000000000adex":   {"1500000000000000000", "adex"},
		"5ibc/27394FB092D2ECCD5612": {"5", "ibc/27394FB092D2ECCD5612"},
	}
	for in, want := range good {
		c, err := parseCoin(in)
		if err != nil || c.Amount != want[0] || c.Denom != want[1] {
			t.Errorf("parseCoin(%q) = %+v, %v", in, c, err)
		}
	}
	for _, in := range []string{"", "adex", "100", "1.5adex", "1e18adex", "-5adex", "10 adex", "0adex", "5a"} {
		if c, err := parseCoin(in); err == nil {
			t.Errorf("parseCoin(%q) accepted: %+v", in, c)
		}
	}
}

func TestSendBuildsMsgSend(t *testing.T) {
	tx.ConfirmTimeout, tx.ConfirmPoll = 300*time.Millisecond, 20*time.Millisecond
	n := fakenode.Start(t, "primium-1")
	c := &toolkit.Context{Context: context.Background(), Profile: n.Profile(t, "eth_secp256k1"),
		Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return true, nil }}
	defer c.Close()
	if _, err := (sendTool{}).Run(c, toolkit.Args{"to": "cosmos1recipient", "amount": "42adex", "memo": "refill"}); err != nil {
		t.Fatal(err)
	}
	bs, _, _ := n.Snapshot()
	var m bankv1beta1.MsgSend
	proto.Unmarshal(bs[0].Msgs[0].Value, &m)
	tb, _ := c.Tx()
	if m.FromAddress != tb.Address() || m.ToAddress != "cosmos1recipient" || m.Amount[0].Amount != "42" || bs[0].Memo != "refill" {
		t.Fatalf("send = %+v memo=%q", &m, bs[0].Memo)
	}
	if _, err := (sendTool{}).Run(c, toolkit.Args{"to": "cosmos1recipient", "amount": "1.5adex"}); err == nil {
		t.Fatal("decimal amount accepted")
	}
}
