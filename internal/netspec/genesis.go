package netspec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// precompileAddrs are cosmos/evm's static precompiles (x/vm/types).
var precompileAddrs = map[string]string{
	"p256":         "0x0000000000000000000000000000000000000100",
	"bech32":       "0x0000000000000000000000000000000000000400",
	"staking":      "0x0000000000000000000000000000000000000800",
	"distribution": "0x0000000000000000000000000000000000000801",
	"ics20":        "0x0000000000000000000000000000000000000802",
	"vesting":      "0x0000000000000000000000000000000000000803",
	"bank":         "0x0000000000000000000000000000000000000804",
	"gov":          "0x0000000000000000000000000000000000000805",
	"slashing":     "0x0000000000000000000000000000000000000806",
	"ics02":        "0x0000000000000000000000000000000000000807",
}

// PatchGenesis applies the spec's genesis parameters to a genesis.json
// from `evmd init` — the same edits node-setup's init-genesis.sh makes.
// Only keys the spec sets are touched.
func (s *Spec) PatchGenesis(raw []byte) ([]byte, error) {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("genesis: %w", err)
	}
	app, _ := doc["app_state"].(map[string]any)
	if app == nil {
		return nil, fmt.Errorf("genesis has no app_state")
	}
	var errs []string
	set := func(v any, path ...string) {
		if err := setPath(app, path, v); err != nil {
			errs = append(errs, err.Error())
		}
	}
	str := func(v string, path ...string) {
		if v != "" {
			set(v, path...)
		}
	}
	num := func(n int64, path ...string) {
		if n != 0 {
			set(strconv.FormatInt(n, 10), path...)
		}
	}
	dur := func(sec int64, path ...string) {
		if sec != 0 {
			set(fmt.Sprintf("%ds", sec), path...)
		}
	}
	dec18 := func(f float64, path ...string) {
		if f != 0 {
			set(Dec18(f), path...)
		}
	}
	coin := func(amount string, path ...string) {
		if amount != "" {
			set([]any{map[string]any{"denom": s.Chain.Denom, "amount": amount}}, path...)
		}
	}
	doc["chain_id"] = s.Chain.ID
	d := s.Chain.Denom
	g := s.Genesis
	str(d, "staking", "params", "bond_denom")
	str(d, "mint", "params", "mint_denom")
	str(d, "evm", "params", "evm_denom")
	if ext, ok := lookupMap(app, "evm", "params", "extended_denom_options"); ok {
		ext["extended_denom"] = d
	}
	// gov
	coin(g.Gov.MinDeposit, "gov", "params", "min_deposit")
	coin(g.Gov.ExpeditedMinDeposit, "gov", "params", "expedited_min_deposit")
	dur(g.Gov.VotingPeriodS, "gov", "params", "voting_period")
	dur(g.Gov.ExpeditedVotingPeriod, "gov", "params", "expedited_voting_period")
	dur(g.Gov.MaxDepositPeriodS, "gov", "params", "max_deposit_period")
	dec18(g.Gov.Quorum, "gov", "params", "quorum")
	dec18(g.Gov.Threshold, "gov", "params", "threshold")
	dec18(g.Gov.VetoThreshold, "gov", "params", "veto_threshold")
	dec18(g.Gov.ExpeditedThreshold, "gov", "params", "expedited_threshold")
	// staking
	dur(g.Staking.UnbondingTimeS, "staking", "params", "unbonding_time")
	if g.Staking.MaxValidators != 0 {
		set(json.Number(strconv.FormatInt(g.Staking.MaxValidators, 10)), "staking", "params", "max_validators")
	}
	set(Dec18(g.Staking.MinCommissionRate), "staking", "params", "min_commission_rate")
	// slashing
	num(g.Slashing.SignedBlocksWindow, "slashing", "params", "signed_blocks_window")
	dec18(g.Slashing.MinSignedPerWindow, "slashing", "params", "min_signed_per_window")
	dur(g.Slashing.DowntimeJailDurationS, "slashing", "params", "downtime_jail_duration")
	dec18(g.Slashing.SlashFractionDouble, "slashing", "params", "slash_fraction_double_sign")
	dec18(g.Slashing.SlashFractionDowntime, "slashing", "params", "slash_fraction_downtime")
	// mint, distribution
	dec18(g.Mint.InflationRateChange, "mint", "params", "inflation_rate_change")
	dec18(g.Mint.InflationMax, "mint", "params", "inflation_max")
	dec18(g.Mint.InflationMin, "mint", "params", "inflation_min")
	dec18(g.Mint.GoalBonded, "mint", "params", "goal_bonded")
	num(g.Mint.BlocksPerYear, "mint", "params", "blocks_per_year")
	dec18(g.Distribution.CommunityTax, "distribution", "params", "community_tax")
	set(g.Distribution.WithdrawAddrEnabled, "distribution", "params", "withdraw_addr_enabled")
	// feemarket
	fm := g.Feemarket
	set(fm.NoBaseFee, "feemarket", "params", "no_base_fee")
	if fm.BaseFee != "" {
		set(fm.BaseFee+".000000000000000000", "feemarket", "params", "base_fee")
	}
	if fm.MinGasPrice != "" {
		set(fm.MinGasPrice+".000000000000000000", "feemarket", "params", "min_gas_price")
	}
	if fm.BaseFeeChangeDenominator != 0 {
		set(json.Number(strconv.FormatInt(fm.BaseFeeChangeDenominator, 10)), "feemarket", "params", "base_fee_change_denominator")
	}
	if fm.ElasticityMultiplier != 0 {
		set(json.Number(strconv.FormatInt(fm.ElasticityMultiplier, 10)), "feemarket", "params", "elasticity_multiplier")
	}
	// evm
	ev := g.EVM
	if len(ev.Precompiles) > 0 {
		var addrs []string
		for _, p := range ev.Precompiles {
			a, ok := precompileAddrs[p]
			if !ok {
				return nil, fmt.Errorf("unknown precompile %q", p)
			}
			addrs = append(addrs, a)
		}
		sort.Strings(addrs) // evmd rejects unsorted precompiles
		list := make([]any, len(addrs))
		for i, a := range addrs {
			list[i] = a
		}
		set(list, "evm", "params", "active_static_precompiles")
	}
	str(ev.AccessCreate, "evm", "params", "access_control", "create", "access_type")
	str(ev.AccessCall, "evm", "params", "access_control", "call", "access_type")
	num(ev.HistoryServeWindow, "evm", "params", "history_serve_window")
	if _, ok := app["erc20"]; ok {
		set(ev.ERC20, "erc20", "params", "enable_erc20")
	}
	// denom metadata, so wallets show the display unit (18 decimals)
	if s.Chain.DisplayDenom != "" {
		bank, _ := app["bank"].(map[string]any)
		if bank != nil {
			meta, _ := bank["denom_metadata"].([]any)
			found := false
			for _, m := range meta {
				if mm, ok := m.(map[string]any); ok && mm["base"] == d {
					found = true
				}
			}
			if !found {
				bank["denom_metadata"] = append(meta, map[string]any{
					"base": d, "display": s.Chain.DisplayDenom, "name": s.Chain.DisplayDenom, "symbol": s.Chain.DisplayDenom,
					"description": s.Chain.ID + " native token",
					"denom_units": []any{
						map[string]any{"denom": d, "exponent": json.Number("0")},
						map[string]any{"denom": s.Chain.DisplayDenom, "exponent": json.Number("18")},
					},
				})
			}
		}
	}
	// block gas: consensus.params (SDK 0.50+ genesis) or consensus_params
	if ev.MaxGasPerBlock != 0 {
		mg := strconv.FormatInt(ev.MaxGasPerBlock, 10)
		if cons, ok := doc["consensus"].(map[string]any); ok {
			_ = setPath(cons, []string{"params", "block", "max_gas"}, mg)
		} else if cp, ok := doc["consensus_params"].(map[string]any); ok {
			_ = setPath(cp, []string{"block", "max_gas"}, mg)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("genesis patch: %v", errs)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// setPath sets a value at a path of existing objects; a missing module
// or params block is an error (the image's genesis decides the shape).
func setPath(m map[string]any, path []string, v any) error {
	cur := m
	for i, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			if i >= 2 { // inside params: create sub-objects (access_control.create)
				next = map[string]any{}
				cur[k] = next
			} else {
				return fmt.Errorf("%v: no %s in this genesis", path, k)
			}
		}
		cur = next
	}
	cur[path[len(path)-1]] = v
	return nil
}

func lookupMap(m map[string]any, path ...string) (map[string]any, bool) {
	cur := m
	for _, k := range path {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// Dec18 renders a fraction as an 18-place SDK decimal without binary
// float noise: 0.334 → "0.334000000000000000".
func Dec18(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 18 {
		frac = frac[:18]
	}
	return whole + "." + frac + strings.Repeat("0", 18-len(frac))
}
