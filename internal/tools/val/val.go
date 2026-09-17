// Package val implements val.* tools: validator status, signing health,
// rewards, and the on-chain ops transactions (unjail, withdraw, edit, vote).
package val

import (
	"fmt"
	"strings"
	"time"

	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	distv1beta1 "cosmossdk.io/api/cosmos/distribution/v1beta1"
	govv1 "cosmossdk.io/api/cosmos/gov/v1"
	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

// Register adds all val.* common.
func Register(r *toolkit.Registry) {
	r.Register(statusTool{})
	r.Register(signingTool{})
	r.Register(rewardsTool{})
	r.Register(votesTool{})
	r.Register(unjailTool{})
	r.Register(withdrawTool{})
	r.Register(editTool{})
	r.Register(voteTool{})
	r.Register(createTool{})
}

type statusTool struct{}

func (statusTool) Name() string { return "val.status" }
func (statusTool) Desc() string {
	return "Validator status: bonded/jailed/tombstoned, tokens, commission, signing info"
}
func (statusTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"validator": toolkit.Str("valoper address (default: signer key)"),
	})
}
func (statusTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (statusTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	valoper, err := common.Valoper(c, a)
	if err != nil {
		return nil, err
	}
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	res, err := g.Staking.Validator(c, &stakingv1beta1.QueryValidatorRequest{ValidatorAddr: valoper})
	if err != nil {
		return nil, err
	}
	v := res.Validator
	var b strings.Builder
	fmt.Fprintf(&b, "validator: %s\n", valoper)
	fmt.Fprintf(&b, "moniker:   %s\n", v.Description.Moniker)
	fmt.Fprintf(&b, "status:    %s   jailed: %v\n", bondStatus(v.Status), v.Jailed)
	fmt.Fprintf(&b, "tokens:    %s\n", v.Tokens)
	fmt.Fprintf(&b, "commission: %s%% (max %s%%, max change %s%%)\n",
		common.DecPct(string(v.Commission.CommissionRates.Rate)),
		common.DecPct(string(v.Commission.CommissionRates.MaxRate)),
		common.DecPct(string(v.Commission.CommissionRates.MaxChangeRate)))
	if cons, err := common.ConsAddress(c, valoper); err == nil {
		fmt.Fprintf(&b, "valcons:   %s\n", cons)
		if si, err := g.Slashing.SigningInfo(c, &slashingv1beta1.QuerySigningInfoRequest{ConsAddress: cons}); err == nil {
			info := si.ValSigningInfo
			fmt.Fprintf(&b, "missed:    %d blocks   tombstoned: %v   jailed_until: %s\n",
				info.MissedBlocksCounter, info.Tombstoned, info.JailedUntil.AsTime().Format(time.RFC3339))
		}
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{
		"valoper": valoper, "moniker": v.Description.Moniker, "jailed": v.Jailed,
		"status": bondStatus(v.Status), "tokens": v.Tokens,
	}}, nil
}

type signingTool struct{}

func (signingTool) Name() string { return "val.signing" }
func (signingTool) Desc() string {
	return "Signing health: missed-block counter, uptime over the signed-blocks window"
}
func (signingTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"validator": toolkit.Str("valoper address (default: signer key)"),
	})
}
func (signingTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (signingTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	valoper, err := common.Valoper(c, a)
	if err != nil {
		return nil, err
	}
	cons, err := common.ConsAddress(c, valoper)
	if err != nil {
		return nil, err
	}
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	si, err := g.Slashing.SigningInfo(c, &slashingv1beta1.QuerySigningInfoRequest{ConsAddress: cons})
	if err != nil {
		return nil, err
	}
	params, err := g.Slashing.Params(c, &slashingv1beta1.QueryParamsRequest{})
	if err != nil {
		return nil, err
	}
	info := si.ValSigningInfo
	window := params.Params.SignedBlocksWindow
	missed := info.MissedBlocksCounter
	uptime := 100.0
	if window > 0 {
		uptime = 100.0 * float64(window-missed) / float64(window)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "valcons:    %s\n", cons)
	fmt.Fprintf(&b, "window:     %d blocks\n", window)
	fmt.Fprintf(&b, "missed:     %d  (uptime %.2f%%)\n", missed, uptime)
	fmt.Fprintf(&b, "tombstoned: %v\n", info.Tombstoned)
	fmt.Fprintf(&b, "jailed_until: %s\n", info.JailedUntil.AsTime().Format(time.RFC3339))
	fmt.Fprintf(&b, "start_height: %d\n", info.StartHeight)
	return &toolkit.Result{Text: b.String(), Data: map[string]any{
		"cons_address": cons, "window": window, "missed": missed,
		"uptime_pct": uptime, "tombstoned": info.Tombstoned,
		"jailed_until": info.JailedUntil.AsTime().Format(time.RFC3339),
	}}, nil
}

type rewardsTool struct{}

func (rewardsTool) Name() string { return "val.rewards" }
func (rewardsTool) Desc() string {
	return "Outstanding rewards + commission for the validator operator"
}
func (rewardsTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"validator": toolkit.Str("valoper address (default: signer key)"),
	})
}
func (rewardsTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (rewardsTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	valoper, err := common.Valoper(c, a)
	if err != nil {
		return nil, err
	}
	acct, err := common.Account(c)
	if err != nil {
		return nil, err
	}
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	data := map[string]any{"valoper": valoper}
	if r, err := g.Dist.ValidatorOutstandingRewards(c, &distv1beta1.QueryValidatorOutstandingRewardsRequest{ValidatorAddress: valoper}); err == nil {
		fmt.Fprintf(&b, "outstanding rewards: %s\n", coins(r.Rewards.Rewards))
		data["outstanding"] = coins(r.Rewards.Rewards)
	}
	if r, err := g.Dist.ValidatorCommission(c, &distv1beta1.QueryValidatorCommissionRequest{ValidatorAddress: valoper}); err == nil {
		fmt.Fprintf(&b, "commission accrued:  %s\n", coins(r.Commission.Commission))
		data["commission"] = coins(r.Commission.Commission)
	}
	if r, err := g.Dist.DelegationRewards(c, &distv1beta1.QueryDelegationRewardsRequest{DelegatorAddress: acct, ValidatorAddress: valoper}); err == nil {
		fmt.Fprintf(&b, "self-delegation rewards: %s\n", coins(r.Rewards))
		data["self_rewards"] = coins(r.Rewards)
	}
	return &toolkit.Result{Text: b.String(), Data: data}, nil
}

type votesTool struct{}

func (votesTool) Name() string { return "val.votes" }
func (votesTool) Desc() string {
	return "Active gov proposals vs this validator's recorded votes"
}
func (votesTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"validator": toolkit.Str("valoper address (default: signer key)"),
	})
}
func (votesTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (votesTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	acct, err := common.Account(c)
	if err != nil {
		return nil, err
	}
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	res, err := g.GovV1.Proposals(c, &govv1.QueryProposalsRequest{
		ProposalStatus: govv1.ProposalStatus_PROPOSAL_STATUS_VOTING_PERIOD,
	})
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	var pending []uint64
	for _, p := range res.Proposals {
		vote, err := g.GovV1.Vote(c, &govv1.QueryVoteRequest{ProposalId: p.Id, Voter: acct})
		voted := err == nil && vote.Vote != nil && len(vote.Vote.Options) > 0
		status := "✗ NOT VOTED"
		if voted {
			status = "✓ voted " + voteOptions(vote.Vote.Options)
		} else {
			pending = append(pending, p.Id)
		}
		fmt.Fprintf(&b, "prop #%d  %-50s %s\n", p.Id, p.Title, status)
	}
	if len(res.Proposals) == 0 {
		fmt.Fprintln(&b, "no proposals in voting period")
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{
		"pending_votes": pending, "active_proposals": len(res.Proposals),
	}}, nil
}

// ---- on-chain tier ----

type unjailTool struct{}

func (unjailTool) Name() string { return "val.unjail" }
func (unjailTool) Desc() string {
	return "Broadcast MsgUnjail to release the validator from jail"
}
func (unjailTool) Schema() map[string]any {
	return toolkit.ObjSchema(common.WithTx(map[string]any{
		"validator": toolkit.Str("valoper address (default: signer key)"),
		"memo":      toolkit.Str("tx memo"),
	}))
}
func (unjailTool) Tier() toolkit.Tier { return toolkit.TierOnChain }

func (unjailTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	valoper, err := common.Valoper(c, a)
	if err != nil {
		return nil, err
	}
	return common.BroadcastMsgs(c, tx.Msgs{
		&slashingv1beta1.MsgUnjail{ValidatorAddr: valoper},
	}, a.String("memo", ""), map[string]string{"action": "unjail", "validator": valoper}, common.TxOpts(a))
}

type withdrawTool struct{}

func (withdrawTool) Name() string { return "val.withdraw" }
func (withdrawTool) Desc() string {
	return "Withdraw delegation rewards and/or validator commission"
}
func (withdrawTool) Schema() map[string]any {
	return toolkit.ObjSchema(common.WithTx(map[string]any{
		"validator":  toolkit.Str("valoper address (default: signer key)"),
		"commission": toolkit.Bool("also withdraw accrued commission"),
		"memo":       toolkit.Str("tx memo"),
	}))
}
func (withdrawTool) Tier() toolkit.Tier { return toolkit.TierOnChain }

func (withdrawTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	valoper, err := common.Valoper(c, a)
	if err != nil {
		return nil, err
	}
	acct, err := common.Account(c)
	if err != nil {
		return nil, err
	}
	msgs := tx.Msgs{
		&distv1beta1.MsgWithdrawDelegatorReward{DelegatorAddress: acct, ValidatorAddress: valoper},
	}
	if a.Bool("commission", false) {
		msgs = append(msgs, &distv1beta1.MsgWithdrawValidatorCommission{ValidatorAddress: valoper})
	}
	return common.BroadcastMsgs(c, msgs, a.String("memo", ""), map[string]string{"action": "withdraw", "validator": valoper}, common.TxOpts(a))
}

type editTool struct{}

func (editTool) Name() string { return "val.edit" }
func (editTool) Desc() string {
	return "Edit validator: commission rate, moniker, min-self-delegation"
}
func (editTool) Schema() map[string]any {
	return toolkit.ObjSchema(common.WithTx(map[string]any{
		"validator":           toolkit.Str("valoper address (default: signer key)"),
		"commission-rate":     toolkit.Str("new commission rate, e.g. 0.05"),
		"min-self-delegation": toolkit.Str("new min self delegation"),
		"moniker":             toolkit.Str("new moniker"),
		"memo":                toolkit.Str("tx memo"),
	}))
}
func (editTool) Tier() toolkit.Tier { return toolkit.TierOnChain }

func (editTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	valoper, err := common.Valoper(c, a)
	if err != nil {
		return nil, err
	}
	msg := &stakingv1beta1.MsgEditValidator{ValidatorAddress: valoper}
	// SDK requires a non-empty Description on every edit — carry the current
	// one through so changing commission alone doesn't wipe it.
	if g, err := c.GRPC(); err == nil {
		if res, err := g.Staking.Validator(c, &stakingv1beta1.QueryValidatorRequest{ValidatorAddr: valoper}); err == nil && res.Validator != nil {
			msg.Description = res.Validator.Description
		}
	}
	if m := a.String("moniker", ""); m != "" {
		if msg.Description == nil {
			msg.Description = &stakingv1beta1.Description{}
		}
		msg.Description.Moniker = m
	}
	if msg.Description == nil {
		msg.Description = &stakingv1beta1.Description{}
	}
	if r := a.String("commission-rate", ""); r != "" {
		scaled, err := common.DecScaled(r)
		if err != nil {
			return nil, fmt.Errorf("commission-rate: %w", err)
		}
		msg.CommissionRate = scaled
	}
	if m := a.String("min-self-delegation", ""); m != "" {
		msg.MinSelfDelegation = m
	}
	return common.BroadcastMsgs(c, tx.Msgs{msg}, a.String("memo", ""), map[string]string{"action": "edit-validator", "validator": valoper}, common.TxOpts(a))
}

type voteTool struct{}

func (voteTool) Name() string { return "val.vote" }
func (voteTool) Desc() string {
	return "Vote on a governance proposal (yes|no|abstain|no_with_veto)"
}
func (voteTool) Schema() map[string]any {
	return toolkit.ObjSchema(common.WithTx(map[string]any{
		"proposal": toolkit.Int("proposal id"),
		"option":   toolkit.Enum("vote option", "yes", "no", "abstain", "no_with_veto"),
		"memo":     toolkit.Str("tx memo"),
	}), "proposal", "option")
}
func (voteTool) Tier() toolkit.Tier { return toolkit.TierOnChain }

func (voteTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	acct, err := common.Account(c)
	if err != nil {
		return nil, err
	}
	opt := map[string]govv1.VoteOption{
		"yes":          govv1.VoteOption_VOTE_OPTION_YES,
		"no":           govv1.VoteOption_VOTE_OPTION_NO,
		"abstain":      govv1.VoteOption_VOTE_OPTION_ABSTAIN,
		"no_with_veto": govv1.VoteOption_VOTE_OPTION_NO_WITH_VETO,
	}[a.String("option", "yes")]
	msg := &govv1.MsgVote{
		ProposalId: uint64(a.Int("proposal", 0)),
		Voter:      acct,
		Option:     opt,
	}
	return common.BroadcastMsgs(c, tx.Msgs{msg}, a.String("memo", ""),
		map[string]string{"action": "gov-vote", "proposal": fmt.Sprint(msg.ProposalId), "option": a.String("option", "yes")}, common.TxOpts(a))
}

// ---- helpers ----

func bondStatus(s stakingv1beta1.BondStatus) string {
	return strings.TrimPrefix(s.String(), "BOND_STATUS_")
}

func coins(cs []*basev1beta1.DecCoin) string {
	if len(cs) == 0 {
		return "0"
	}
	var parts []string
	for _, c := range cs {
		parts = append(parts, common.DecFrac(string(c.Amount))+c.Denom)
	}
	return strings.Join(parts, ", ")
}

func voteOptions(opts []*govv1.WeightedVoteOption) string {
	var parts []string
	for _, o := range opts {
		parts = append(parts, strings.TrimPrefix(o.Option.String(), "VOTE_OPTION_"))
	}
	return strings.Join(parts, "+")
}
