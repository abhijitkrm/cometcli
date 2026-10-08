package val

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	distv1beta1 "cosmossdk.io/api/cosmos/distribution/v1beta1"
	govv1 "cosmossdk.io/api/cosmos/gov/v1"
	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

func TestMain(m *testing.M) {
	fakenode.MaybeRunFakeDocker()
	os.Exit(m.Run())
}

func setup(t *testing.T) (*fakenode.Node, *toolkit.Context, string) {
	n, c, v, _ := setupChain(t)
	return n, c, v
}

func setupChain(t *testing.T) (*fakenode.Node, *toolkit.Context, string, *fakenode.Comet) {
	t.Helper()
	tx.ConfirmTimeout, tx.ConfirmPoll = 300*time.Millisecond, 20*time.Millisecond
	n := fakenode.Start(t, "mychain-1")
	chain := fakenode.StartComet(t)
	p := n.Profile(t, "eth_secp256k1")
	fakenode.WithComet(p, chain)
	c := &toolkit.Context{Context: context.Background(), Profile: p, AutoApproveBelow: toolkit.TierLocalChange,
		Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return true, nil }}
	t.Cleanup(c.Close)
	tb, err := c.Tx()
	if err != nil {
		t.Fatal(err)
	}
	valoper, _ := tb.ValAddress()
	n.Set(func(n *fakenode.Node) {
		n.Validator = &stakingv1beta1.Validator{OperatorAddress: valoper, ConsensusPubkey: fakenode.ConsPubAny(),
			Status: stakingv1beta1.BondStatus_BOND_STATUS_BONDED, MinSelfDelegation: "1",
			Description: &stakingv1beta1.Description{Moniker: "validator-01", Website: "https://validator.example", Details: "genesis validator"}}
		n.SigningInfo = &slashingv1beta1.ValidatorSigningInfo{}
	})
	return n, c, valoper, chain
}

// jail puts the validator in jail until `until`.
func jail(n *fakenode.Node, until time.Time) {
	n.Set(func(n *fakenode.Node) {
		n.Validator.Jailed = true
		n.Validator.Status = stakingv1beta1.BondStatus_BOND_STATUS_UNBONDING
		n.SigningInfo.JailedUntil = timestamppb.New(until)
	})
}

func lastMsgs(t *testing.T, n *fakenode.Node) []proto.Message {
	t.Helper()
	bs, rej, _ := n.Snapshot()
	if len(bs) == 0 {
		t.Fatalf("nothing broadcast (rejected: %v)", rej)
	}
	var out []proto.Message
	for _, a := range bs[len(bs)-1].Msgs {
		var m proto.Message
		switch a.TypeUrl {
		case "/cosmos.staking.v1beta1.MsgEditValidator":
			m = &stakingv1beta1.MsgEditValidator{}
		case "/cosmos.slashing.v1beta1.MsgUnjail":
			m = &slashingv1beta1.MsgUnjail{}
		case "/cosmos.gov.v1.MsgVote":
			m = &govv1.MsgVote{}
		case "/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward":
			m = &distv1beta1.MsgWithdrawDelegatorReward{}
		case "/cosmos.distribution.v1beta1.MsgWithdrawValidatorCommission":
			m = &distv1beta1.MsgWithdrawValidatorCommission{}
		default:
			t.Fatalf("unexpected msg %s", a.TypeUrl)
		}
		if err := proto.Unmarshal(a.Value, m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestEditCommissionKeepsDescription(t *testing.T) {
	n, c, valoper := setup(t)
	if _, err := (editTool{}).Run(c, toolkit.Args{"commission-rate": "0.10"}); err != nil {
		t.Fatal(err)
	}
	m := lastMsgs(t, n)[0].(*stakingv1beta1.MsgEditValidator)
	if m.ValidatorAddress != valoper || m.CommissionRate != "100000000000000000" {
		t.Fatalf("edit = %+v", m)
	}
	if m.Description.GetMoniker() != "validator-01" || m.Description.GetWebsite() != "https://validator.example" {
		t.Fatalf("description wiped: %+v", m.Description)
	}
	if _, err := (editTool{}).Run(c, toolkit.Args{"moniker": "validator-one"}); err != nil {
		t.Fatal(err)
	}
	m = lastMsgs(t, n)[0].(*stakingv1beta1.MsgEditValidator)
	if m.Description.GetMoniker() != "validator-one" || m.Description.GetDetails() != "genesis validator" || m.CommissionRate != "" {
		t.Fatalf("moniker edit = %+v", m)
	}
}

func TestEditRejectsBadInputBeforeBroadcast(t *testing.T) {
	n, c, _ := setup(t)
	for _, args := range []toolkit.Args{{"commission-rate": "abc"}, {"commission-rate": "0.0000000000000000001"}, {}} {
		if _, err := (editTool{}).Run(c, args); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	if bs, _, _ := n.Snapshot(); len(bs) != 0 {
		t.Fatal("invalid edit broadcast")
	}
}

func TestUnjailAndWithdraw(t *testing.T) {
	n, c, valoper := setup(t)
	jail(n, time.Now().Add(-time.Minute))
	if _, err := (unjailTool{}).Run(c, toolkit.Args{}); err != nil {
		t.Fatal(err)
	}
	if m := lastMsgs(t, n)[0].(*slashingv1beta1.MsgUnjail); m.ValidatorAddr != valoper {
		t.Fatalf("unjail = %+v", m)
	}
	if _, err := (withdrawTool{}).Run(c, toolkit.Args{"commission": true}); err != nil {
		t.Fatal(err)
	}
	ms := lastMsgs(t, n)
	if len(ms) != 2 || ms[0].(*distv1beta1.MsgWithdrawDelegatorReward).ValidatorAddress != valoper || ms[1].(*distv1beta1.MsgWithdrawValidatorCommission).ValidatorAddress != valoper {
		t.Fatalf("withdraw = %v", ms)
	}
}

func TestVoteOptions(t *testing.T) {
	n, c, _ := setup(t)
	for opt, want := range map[string]govv1.VoteOption{"yes": govv1.VoteOption_VOTE_OPTION_YES, "no_with_veto": govv1.VoteOption_VOTE_OPTION_NO_WITH_VETO, "ABSTAIN": govv1.VoteOption_VOTE_OPTION_ABSTAIN} {
		if _, err := (voteTool{}).Run(c, toolkit.Args{"proposal": float64(7), "option": opt}); err != nil {
			t.Fatal(err)
		}
		m := lastMsgs(t, n)[0].(*govv1.MsgVote)
		if m.ProposalId != 7 || m.Option != want || m.Voter == "" {
			t.Fatalf("%s: vote = %+v", opt, m)
		}
	}
	before, _, _ := n.Snapshot()
	for _, args := range []toolkit.Args{{"proposal": float64(7), "option": "yea"}, {"option": "yes"}} {
		if _, err := (voteTool{}).Run(c, args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if after, _, _ := n.Snapshot(); len(after) != len(before) {
		t.Fatal("invalid vote broadcast")
	}
}

func TestStatusAndSigning(t *testing.T) {
	n, c, valoper := setup(t)
	res, err := (statusTool{}).Run(c, toolkit.Args{"validator": valoper})
	if err != nil || !strings.Contains(res.Text, "validator-01") {
		t.Fatalf("status = %v %v", res, err)
	}
	n.Set(func(n *fakenode.Node) { n.Validator.Jailed = true })
	res, _ = (statusTool{}).Run(c, toolkit.Args{"validator": valoper})
	if !strings.Contains(strings.ToLower(res.Text), "jailed") {
		t.Fatalf("jailed not shown: %q", res.Text)
	}
}
