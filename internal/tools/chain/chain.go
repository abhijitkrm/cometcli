// Package chain implements chain.* tools: network-wide state reads.
package chain

import (
	"fmt"
	"strings"

	query "cosmossdk.io/api/cosmos/base/query/v1beta1"
	govv1 "cosmossdk.io/api/cosmos/gov/v1"
	mintv1beta1 "cosmossdk.io/api/cosmos/mint/v1beta1"
	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	upgradev1beta1 "cosmossdk.io/api/cosmos/upgrade/v1beta1"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Register adds all chain.* tools.
func Register(r *toolkit.Registry) {
	r.Register(validatorsTool{})
	r.Register(govTool{})
	r.Register(upgradePlanTool{})
	r.Register(paramsTool{})
	r.Register(poolTool{})
}

type validatorsTool struct{}

func (validatorsTool) Name() string { return "chain.validators" }
func (validatorsTool) Desc() string {
	return "List validators with status, tokens, and commission"
}
func (validatorsTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"status": toolkit.Enum("filter by bond status", "all", "bonded", "unbonding", "unbonded"),
	})
}
func (validatorsTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (validatorsTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	status := map[string]string{
		"bonded": "BOND_STATUS_BONDED", "unbonding": "BOND_STATUS_UNBONDING",
		"unbonded": "BOND_STATUS_UNBONDED", "all": "",
	}[a.String("status", "bonded")]
	var b strings.Builder
	var all []map[string]any
	key := []byte{}
	for {
		res, err := g.Staking.Validators(c, &stakingv1beta1.QueryValidatorsRequest{
			Status: status,
			Pagination: &query.PageRequest{Key: key, Limit: 200},
		})
		if err != nil {
			return nil, err
		}
		for _, v := range res.Validators {
			fmt.Fprintf(&b, "%-24s %-10s %s  comm=%s\n",
				trunc(v.Description.Moniker, 24), strings.TrimPrefix(v.Status.String(), "BOND_STATUS_"),
				v.Tokens, v.Commission.CommissionRates.Rate)
			all = append(all, map[string]any{
				"moniker": v.Description.Moniker, "status": v.Status.String(),
				"tokens": v.Tokens, "valoper": v.OperatorAddress, "jailed": v.Jailed,
			})
		}
		if res.Pagination == nil || len(res.Pagination.NextKey) == 0 {
			break
		}
		key = res.Pagination.NextKey
	}
	fmt.Fprintf(&b, "total: %d\n", len(all))
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"validators": all, "count": len(all)}}, nil
}

type govTool struct{}

func (govTool) Name() string { return "chain.gov" }
func (govTool) Desc() string {
	return "List governance proposals (default: voting period)"
}
func (govTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"status": toolkit.Enum("proposal status filter", "voting", "deposit", "passed", "rejected", "all"),
	})
}
func (govTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (govTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	st := map[string]govv1.ProposalStatus{
		"voting": govv1.ProposalStatus_PROPOSAL_STATUS_VOTING_PERIOD,
		"deposit": govv1.ProposalStatus_PROPOSAL_STATUS_DEPOSIT_PERIOD,
		"passed": govv1.ProposalStatus_PROPOSAL_STATUS_PASSED,
		"rejected": govv1.ProposalStatus_PROPOSAL_STATUS_REJECTED,
		"all": govv1.ProposalStatus_PROPOSAL_STATUS_UNSPECIFIED,
	}[a.String("status", "voting")]
	res, err := g.GovV1.Proposals(c, &govv1.QueryProposalsRequest{ProposalStatus: st})
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	var props []map[string]any
	for _, p := range res.Proposals {
		fmt.Fprintf(&b, "#%-4d %-26s %-56s ends %s\n",
			p.Id, strings.TrimPrefix(p.Status.String(), "PROPOSAL_STATUS_"),
			trunc(p.Title, 56), p.VotingEndTime.AsTime().Format("2006-01-02 15:04"))
		props = append(props, map[string]any{"id": p.Id, "title": p.Title, "status": p.Status.String()})
	}
	if len(props) == 0 {
		fmt.Fprintln(&b, "no matching proposals")
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"proposals": props}}, nil
}

type upgradePlanTool struct{}

func (upgradePlanTool) Name() string { return "chain.upgrade-plan" }
func (upgradePlanTool) Desc() string {
	return "Show the on-chain software upgrade plan (height, name)"
}
func (upgradePlanTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (upgradePlanTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (upgradePlanTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	res, err := g.Upgrade.CurrentPlan(c, &upgradev1beta1.QueryCurrentPlanRequest{})
	if err != nil {
		return nil, err
	}
	if res.Plan == nil {
		return &toolkit.Result{Text: "no upgrade plan scheduled", Data: map[string]any{"plan": nil}}, nil
	}
	p := res.Plan
	var b strings.Builder
	fmt.Fprintf(&b, "upgrade plan: %s\nheight: %d\ninfo: %s\n", p.Name, p.Height, trunc(p.Info, 200))
	return &toolkit.Result{Text: b.String(), Data: map[string]any{
		"plan": map[string]any{"name": p.Name, "height": p.Height, "info": p.Info},
	}}, nil
}

type paramsTool struct{}

func (paramsTool) Name() string { return "chain.params" }
func (paramsTool) Desc() string {
	return "Key chain params: slashing window, unbonding, inflation"
}
func (paramsTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (paramsTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (paramsTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	data := map[string]any{}
	if p, err := g.Slashing.Params(c, &slashingv1beta1.QueryParamsRequest{}); err == nil {
		fmt.Fprintf(&b, "slashing: window=%d  min_signed=%s%%  downtime_jail=%s  slash_frac_downtime=%s  slash_frac_doublesign=%s\n",
			p.Params.SignedBlocksWindow, p.Params.MinSignedPerWindow,
			p.Params.DowntimeJailDuration, p.Params.SlashFractionDowntime, p.Params.SlashFractionDoubleSign)
		data["slashing"] = map[string]any{
			"window": p.Params.SignedBlocksWindow, "min_signed_pct": p.Params.MinSignedPerWindow,
		}
	}
	if p, err := g.Staking.Params(c, &stakingv1beta1.QueryParamsRequest{}); err == nil {
		fmt.Fprintf(&b, "staking:  unbonding=%s  max_validators=%d  bond_denom=%s\n",
			p.Params.UnbondingTime, p.Params.MaxValidators, p.Params.BondDenom)
		data["staking"] = map[string]any{"bond_denom": p.Params.BondDenom, "max_validators": p.Params.MaxValidators}
	}
	if p, err := g.Mint.Params(c, &mintv1beta1.QueryParamsRequest{}); err == nil {
		fmt.Fprintf(&b, "mint:     inflation min=%s max=%s\n", p.Params.InflationMin, p.Params.InflationMax)
	}
	return &toolkit.Result{Text: b.String(), Data: data}, nil
}

type poolTool struct{}

func (poolTool) Name() string { return "chain.pool" }
func (poolTool) Desc() string {
	return "Staking pool: bonded vs not-bonded tokens"
}
func (poolTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (poolTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (poolTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	p, err := g.Staking.Pool(c, &stakingv1beta1.QueryPoolRequest{})
	if err != nil {
		return nil, err
	}
	t := fmt.Sprintf("bonded:     %s\nnot bonded: %s\n", p.Pool.BondedTokens, p.Pool.NotBondedTokens)
	return &toolkit.Result{Text: t, Data: map[string]any{
		"bonded": p.Pool.BondedTokens, "not_bonded": p.Pool.NotBondedTokens,
	}}, nil
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
