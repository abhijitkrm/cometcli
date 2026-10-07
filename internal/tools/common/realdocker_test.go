//go:build realdocker

package common

// Validates container signing against a real evmd. Run with a throwaway
// container (no node process) holding test keys:
//
//	docker run -d --rm --name cometcli-signtest --entrypoint sleep primium-evm:v0.7.2 900
//	docker exec cometcli-signtest evmd keys add t --keyring-backend test --home /tmp/h
//	printf 'testpass123\ntestpass123\n' | docker exec -i cometcli-signtest evmd keys add tf --keyring-backend file --home /tmp/h
//	go test -tags realdocker -run RealEvmd ./internal/tools/common/

import (
	"context"

	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

func TestRealEvmdContainerSigning(t *testing.T) {
	for _, tc := range []struct{ key, keyring string }{{"t", "test"}, {"tf", "file"}} {
		t.Run(tc.keyring, func(t *testing.T) {
			tx.ConfirmTimeout, tx.ConfirmPoll = time.Second, 20*time.Millisecond
			n := fakenode.Start(t, "primium-1")
			p := n.Profile(t, "eth_secp256k1")
			p.Binary = "evmd"
			p.Signer = config.Signer{Container: "cometcli-signtest", ContainerKey: tc.key, ContainerKeyring: tc.keyring, ContainerHome: "/tmp/h"}
			c := &toolkit.Context{Context: context.Background(), Profile: p,
				Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return true, nil },
				Secret:   func(*toolkit.Context, string) (string, error) { return "testpass123", nil }}
			c.SetHost(&host.Local{})
			defer c.Close()
			tb, err := c.TxSigner()
			if err != nil {
				t.Fatal(err)
			}
			msg := &bankv1beta1.MsgSend{FromAddress: tb.Address(), ToAddress: "cosmos1p52q2cw6fuzs3cyuw6n3yzkfx3tffdhk66zdqu",
				Amount: []*basev1beta1.Coin{{Denom: "adex", Amount: "5"}}}
			res, err := BroadcastMsgs(c, tx.Msgs{msg}, "real evmd", nil, tx.Options{})
			if err != nil {
				t.Fatal(err)
			}
			bs, rej, _ := n.Snapshot()
			if len(bs) != 1 || len(rej) != 0 || bs[0].Memo != "real evmd" || !strings.Contains(res.Text, "tx hash") {
				t.Fatalf("broadcasts=%+v rejected=%v", bs, rej)
			}
			t.Logf("evmd (%s keyring) signed tx %s — signature verified", tc.keyring, bs[0].Hash)
		})
	}
}
