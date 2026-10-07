package waittool

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func TestMain(m *testing.M) {
	MinInterval = 20 * time.Millisecond
	os.Exit(m.Run())
}

func setup(t *testing.T) (*fakenode.Node, *fakenode.Comet, *toolkit.Context, *[]string) {
	t.Helper()
	n := fakenode.Start(t, "primium-1")
	chain := fakenode.StartComet(t)
	p := n.Profile(t, "eth_secp256k1")
	fakenode.WithComet(p, chain)
	var mu sync.Mutex
	var lines []string
	c := &toolkit.Context{Context: context.Background(), Profile: p, Progress: func(s string) {
		mu.Lock()
		lines = append(lines, s)
		mu.Unlock()
	}}
	t.Cleanup(c.Close)
	tb, err := c.Tx()
	if err != nil {
		t.Fatal(err)
	}
	valoper, _ := tb.ValAddress()
	p.Metadata["valoper"] = valoper
	n.Set(func(n *fakenode.Node) {
		n.Validator = &stakingv1beta1.Validator{OperatorAddress: valoper, ConsensusPubkey: fakenode.ConsPubAny(),
			Status: stakingv1beta1.BondStatus_BOND_STATUS_BONDED}
		n.SigningInfo = &slashingv1beta1.ValidatorSigningInfo{}
	})
	return n, chain, c, &lines
}

func after(d time.Duration, fn func()) { go func() { time.Sleep(d); fn() }() }

func TestWaitSyncedWithProgress(t *testing.T) {
	_, chain, c, lines := setup(t)
	behind := time.Now().Add(-10 * time.Minute)
	chain.Set(func(c *fakenode.Comet) { c.CatchingUp = true; c.LatestTimeOverride = &behind })
	stop := make(chan struct{})
	go func() { // the node syncs: height climbs, then it's caught up
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(15 * time.Millisecond):
			}
			chain.Set(func(c *fakenode.Comet) {
				c.Height += 50
				lt := behind.Add(time.Duration(i) * 20 * time.Second)
				c.LatestTimeOverride = &lt
				if i == 12 {
					c.CatchingUp, c.LatestTimeOverride = false, nil
				}
			})
		}
	}()
	defer close(stop)
	res, err := (Until{}).Run(c, toolkit.Args{"condition": "synced", "timeout": float64(10), "interval": 0.05})
	if err != nil || res.Data["done"] != true {
		t.Fatalf("res=%v err=%v", res, err)
	}
	joined := strings.Join(*lines, "\n")
	if !strings.Contains(joined, "behind") || !strings.Contains(joined, "blocks/s") {
		t.Fatalf("progress lines:\n%s", joined)
	}
}

func TestWaitUnjailableAndSigning(t *testing.T) {
	n, chain, c, _ := setup(t)
	// jailed until 3 chain-seconds from now; the chain advances 1 block/s of chain time
	until := time.Now().Add(3 * time.Second)
	n.Set(func(n *fakenode.Node) {
		n.Validator.Jailed = true
		n.SigningInfo.JailedUntil = timestamppb.New(until)
	})
	after(50*time.Millisecond, func() { chain.Advance(5) })
	res, err := (Until{}).Run(c, toolkit.Args{"condition": "unjailable", "timeout": float64(5), "interval": 0.05})
	if err != nil || res.Data["done"] != true || !strings.Contains(res.Text, "jail period over") {
		t.Fatalf("unjailable: %v %v", res, err)
	}

	// after unjail: signing resumes only from some height on
	start := int64(0)
	chain.Set(func(c *fakenode.Comet) {
		start = c.Height
		c.Signs = func(h int64) bool { return h > start+5 }
	})
	n.Set(func(n *fakenode.Node) { n.Validator.Jailed = false })
	after(50*time.Millisecond, func() { chain.Advance(30) })
	res, err = (Until{}).Run(c, toolkit.Args{"condition": "in-consensus", "window": float64(20), "timeout": float64(5), "interval": 0.05})
	if err != nil || res.Data["done"] != true || res.Data["signed"].(int64) < 18 {
		t.Fatalf("in-consensus: %v %v", res, err)
	}
}

func TestWaitTimesOutWithLastState(t *testing.T) {
	_, chain, c, _ := setup(t)
	chain.Set(func(c *fakenode.Comet) { c.Signs = func(int64) bool { return false } })
	start := time.Now()
	res, err := (Until{}).Run(c, toolkit.Args{"condition": "signing", "window": float64(10), "timeout": float64(1), "interval": 0.05})
	if err != nil || res.Data["done"] != false || !strings.Contains(res.Text, "signed 0/10") || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout: %v %v (%s)", res, err, time.Since(start))
	}
}

func TestWaitHeightAndBadArgs(t *testing.T) {
	_, chain, c, _ := setup(t)
	after(30*time.Millisecond, func() { chain.Advance(10) })
	res, err := (Until{}).Run(c, toolkit.Args{"condition": "height", "value": "1005", "timeout": float64(5), "interval": 0.05})
	if err != nil || res.Data["done"] != true {
		t.Fatalf("height: %v %v", res, err)
	}
	for _, a := range []toolkit.Args{{"condition": "height"}, {"condition": "proposal-status", "value": "x"}, {"condition": "nope"}} {
		if _, err := (Until{}).Run(c, a); err == nil {
			t.Errorf("%v accepted", a)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cc := &toolkit.Context{Context: ctx, Profile: c.Profile}
	defer cc.Close()
	after(30*time.Millisecond, cancel)
	if _, err := (Until{}).Run(cc, toolkit.Args{"condition": "height", "value": "999999", "timeout": float64(10), "interval": 0.05}); err == nil {
		t.Fatal("cancel didn't stop the wait")
	}
}
