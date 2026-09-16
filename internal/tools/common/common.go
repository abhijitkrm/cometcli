// Package common holds helpers shared by tool domains.

package common

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"

	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/abhijitkrm/cometcli/internal/keys"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

// Valoper resolves a validator operator (valoper) address from the
// "validator" arg, falling back to the profile signer key.
func Valoper(c *toolkit.Context, args toolkit.Args) (string, error) {
	if v := args.String("validator", ""); v != "" {
		return v, nil
	}
	tb, err := c.Tx()
	if err != nil {
		return "", fmt.Errorf("pass --validator <valoper> or configure signer.key: %w", err)
	}
	return tb.ValAddress()
}

// Account resolves the signer's bech32 account address.
func Account(c *toolkit.Context) (string, error) {
	tb, err := c.Tx()
	if err != nil {
		return "", err
	}
	return tb.Address(), nil
}

// ConsAddress derives the valcons address for a valoper: fetch the
// validator's consensus pubkey, hash to a cometbft address, bech32 it.
func ConsAddress(c *toolkit.Context, valoper string) (string, error) {
	g, err := c.GRPC()
	if err != nil {
		return "", err
	}
	res, err := g.Staking.Validator(c, &stakingv1beta1.QueryValidatorRequest{ValidatorAddr: valoper})
	if err != nil {
		return "", err
	}
	pk := res.Validator.ConsensusPubkey
	if pk == nil {
		return "", fmt.Errorf("validator %s has no consensus pubkey", valoper)
	}
	pubBytes := protowireFieldBytes(pk.Value, 1)
	if pubBytes == nil {
		return "", fmt.Errorf("bad consensus pubkey encoding (%s)", pk.TypeUrl)
	}
	sum := sha256.Sum256(pubBytes)
	return keys.ConsAddress(sum[:20], bech32Prefix(c))
}

func bech32Prefix(c *toolkit.Context) string {
	if c.Profile.Bech32Prefix != "" {
		return c.Profile.Bech32Prefix
	}
	return "cosmos"
}

// Prefix exposes the profile bech32 prefix.
func Prefix(c *toolkit.Context) string { return bech32Prefix(c) }

// oneLine collapses multi-line output for compact display.
func OneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// shellQ single-quotes a string for safe shell embedding.
func ShellQ(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TxSchema returns the optional gas/fee flags every tx tool accepts.
func TxSchema() map[string]any {
	return map[string]any{
		"gas-price": toolkit.Str("gas price in fee denom (e.g. 1125000000)"),
		"gas-limit": toolkit.Int("explicit gas limit (default: simulate)"),
		"fee-denom": toolkit.Str("override profile fee denom"),
	}
}

// WithTx merges the shared gas/fee flags into a schema property map.
func WithTx(props map[string]any) map[string]any {
	for k, v := range TxSchema() {
		props[k] = v
	}
	return props
}

// TxOpts extracts the optional gas/fee args into tx.Options.
func TxOpts(a toolkit.Args) tx.Options {
	return tx.Options{
		GasPrice: a.String("gas-price", ""),
		GasLimit: uint64(a.Int("gas-limit", 0)),
		FeeDenom: a.String("fee-denom", ""),
	}
}

// BroadcastMsgs is the shared on-chain flow every tx tool uses:
// build (with gas simulation) → human approval gate → broadcast → report.
// The tx Doc (decoded messages, fee, gas) is always shown before approval.
func BroadcastMsgs(c *toolkit.Context, msgs tx.Msgs, memo string, meta map[string]string, opt tx.Options) (*toolkit.Result, error) {
	opt.Memo = memo
	tb, err := c.Tx()
	if err != nil {
		return nil, err
	}
	built, err := tb.Build(c, msgs, opt)
	if err != nil {
		return nil, err
	}
	detail := map[string]any{"doc": built.Doc.String()}
	for k, v := range meta {
		detail[k] = v
	}
	if err := c.Approve("broadcast transaction\n"+built.Doc.String(), toolkit.TierOnChain, detail); err != nil {
		return nil, err
	}
	hash, code, rawLog, err := tb.Broadcast(c, built.TxBytes)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tx hash: %s\ncode:    %d\n", hash, code)
	if rawLog != "" {
		fmt.Fprintf(&b, "log:     %s\n", rawLog)
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{
		"hash": hash, "code": code, "raw_log": rawLog, "doc": built.Doc,
	}}, nil
}

// latestRelease fetches the newest release tag of a github repo.
func LatestRelease(c *toolkit.Context, repo string) (string, error) {
	req, err := http.NewRequestWithContext(c, "GET",
		"https://api.github.com/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.TagName == "" {
		return "", fmt.Errorf("no releases for %s", repo)
	}
	return r.TagName, nil
}

// protowireFieldBytes extracts a bytes field from a protobuf message.
func protowireFieldBytes(b []byte, num protowire.Number) []byte {
	for len(b) > 0 {
		n, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return nil
		}
		b = b[l:]
		if typ == protowire.BytesType {
			v, l := protowire.ConsumeBytes(b)
			if l < 0 {
				return nil
			}
			if n == num {
				return v
			}
			b = b[l:]
		} else {
			l := protowire.ConsumeFieldValue(n, typ, b)
			if l < 0 {
				return nil
			}
			b = b[l:]
		}
	}
	return nil
}

// DecRat parses a legacy-dec value (string or []byte, fixed 18 decimals).
func DecRat(s string) (*big.Rat, bool) {
	i, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, false
	}
	return new(big.Rat).SetFrac(i, big.NewInt(1000000000000000000)), true
}

// DecFrac renders a legacy dec as a plain fraction, e.g. "0.01".
func DecFrac(s string) string {
	r, ok := DecRat(s)
	if !ok {
		return s
	}
	f, _ := r.Float64()
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// DecPct renders a legacy dec as a percentage number, e.g. 50 for 0.5.
func DecPct(s string) string {
	r, ok := DecRat(s)
	if !ok {
		return s
	}
	f, _ := new(big.Rat).Mul(r, big.NewRat(100, 1)).Float64()
	return strconv.FormatFloat(f, 'f', -1, 64)
}
