// Package txtool implements tx.* tools: bank send and raw broadcast.
package txtool

import (
	"fmt"

	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"

	"github.com/abhijitkrm/cometcli/internal/tools/common"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

// Register adds tx.* tools.
func Register(r *toolkit.Registry) {
	r.Register(sendTool{})
	r.Register(delegateTool{})
}

type sendTool struct{}

func (sendTool) Name() string { return "tx.send" }
func (sendTool) Desc() string {
	return "Send tokens: MsgSend from the ops key to a recipient"
}
func (sendTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"to":     toolkit.Str("recipient bech32 address"),
		"amount": toolkit.Str("amount, e.g. 1000000atest"),
		"memo":   toolkit.Str("tx memo"),
	}, "to", "amount")
}
func (sendTool) Tier() toolkit.Tier { return toolkit.TierOnChain }

func (sendTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	acct, err := common.Account(c)
	if err != nil {
		return nil, err
	}
	coin, err := parseCoin(a.String("amount", ""))
	if err != nil {
		return nil, err
	}
	msg := &bankv1beta1.MsgSend{
		FromAddress: acct,
		ToAddress:   a.String("to", ""),
		Amount:      []*basev1beta1.Coin{coin},
	}
	return common.BroadcastMsgs(c, tx.Msgs{msg}, a.String("memo", ""),
		map[string]string{"action": "bank-send", "to": a.String("to", "")})
}

type delegateTool struct{}

func (delegateTool) Name() string { return "tx.delegate" }
func (delegateTool) Desc() string {
	return "Delegate tokens to a validator (self-delegation top-up)"
}
func (delegateTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"validator": toolkit.Str("valoper address (default: signer key)"),
		"amount":    toolkit.Str("amount, e.g. 1000000atest"),
		"memo":      toolkit.Str("tx memo"),
	}, "amount")
}
func (delegateTool) Tier() toolkit.Tier { return toolkit.TierOnChain }

func (delegateTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	acct, err := common.Account(c)
	if err != nil {
		return nil, err
	}
	valoper, err := common.Valoper(c, a)
	if err != nil {
		return nil, err
	}
	coin, err := parseCoin(a.String("amount", ""))
	if err != nil {
		return nil, err
	}
	msg := &stakingv1beta1.MsgDelegate{
		DelegatorAddress: acct,
		ValidatorAddress: valoper,
		Amount:           coin,
	}
	return common.BroadcastMsgs(c, tx.Msgs{msg}, a.String("memo", ""),
		map[string]string{"action": "delegate", "validator": valoper})
}

// parseCoin parses "1000000atest" into a Coin.
func parseCoin(s string) (*basev1beta1.Coin, error) {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 || i == len(s) {
		return nil, fmt.Errorf("bad amount %q — want e.g. 1000000atest", s)
	}
	return &basev1beta1.Coin{Amount: s[:i], Denom: s[i:]}, nil
}
