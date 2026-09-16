package val

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/abhijitkrm/cometcli/internal/keys"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

// createTool broadcasts MsgCreateValidator — used when bootstrapping a new
// validator with an ops key. The consensus pubkey must be supplied as JSON
// (e.g. from `<binary> comet show-validator` or tendermint show-validator).
type createTool struct{}

func (createTool) Name() string { return "val.create" }
func (createTool) Desc() string {
	return "Create a validator: MsgCreateValidator with a consensus pubkey"
}
func (createTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"amount":                toolkit.Str("self-delegation, e.g. 1000000atest"),
		"pubkey":                toolkit.Str(`consensus pubkey JSON, e.g. {"@type":"/cosmos.crypto.ed25519.PubKey","key":"..."}`),
		"moniker":               toolkit.Str("validator name"),
		"commission-rate":       toolkit.Str("initial commission, e.g. 0.05"),
		"commission-max-rate":   toolkit.Str("max commission, e.g. 0.20"),
		"commission-max-change": toolkit.Str("max daily change, e.g. 0.01"),
		"min-self-delegation":   toolkit.Str("min self delegation (default 1)"),
		"memo":                  toolkit.Str("tx memo"),
	}, "amount", "pubkey", "moniker")
}
func (createTool) Tier() toolkit.Tier { return toolkit.TierOnChain }

func (t createTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	acct, err := common.Account(c)
	if err != nil {
		return nil, err
	}
	pkAny, err := parsePubkeyJSON(a.String("pubkey", ""))
	if err != nil {
		return nil, err
	}
	coin, err := parseAmount(a.String("amount", ""))
	if err != nil {
		return nil, err
	}
	msg := &stakingv1beta1.MsgCreateValidator{
		ValidatorAddress: mustValoper(acct),
		DelegatorAddress: acct,
		Pubkey:           pkAny,
		Value:            coin,
		Description:      &stakingv1beta1.Description{Moniker: a.String("moniker", "")},
		Commission: &stakingv1beta1.CommissionRates{
			Rate:          defStr(a.String("commission-rate", ""), "0.05"),
			MaxRate:       defStr(a.String("commission-max-rate", ""), "0.20"),
			MaxChangeRate: defStr(a.String("commission-max-change", ""), "0.01"),
		},
		MinSelfDelegation: defStr(a.String("min-self-delegation", ""), "1"),
	}
	return common.BroadcastMsgs(c, tx.Msgs{msg}, a.String("memo", ""),
		map[string]string{"action": "create-validator", "moniker": a.String("moniker", "")})
}

// parsePubkeyJSON accepts `{"@type":"/cosmos.crypto.ed25519.PubKey","key":"<b64>"}`.
func parsePubkeyJSON(s string) (*anypb.Any, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("bad pubkey JSON: %w", err)
	}
	typ := m["@type"]
	keyB64 := m["key"]
	if typ == "" || keyB64 == "" {
		return nil, fmt.Errorf("pubkey JSON needs @type and key")
	}
	raw, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("pubkey key not base64: %w", err)
	}
	// pubkey proto is {bytes key = 1}
	var v []byte
	v = protowire.AppendTag(v, 1, protowire.BytesType)
	v = protowire.AppendBytes(v, raw)
	return &anypb.Any{TypeUrl: typ, Value: v}, nil
}

func parseAmount(s string) (*basev1beta1.Coin, error) {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 || i == len(s) {
		return nil, fmt.Errorf("bad amount %q", s)
	}
	return &basev1beta1.Coin{Amount: s[:i], Denom: s[i:]}, nil
}

func mustValoper(acct string) string {
	v, err := keys.ValAddress(acct)
	if err != nil {
		return acct
	}
	return v
}

func defStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
