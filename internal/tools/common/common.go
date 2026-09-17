// Package common holds helpers shared by tool domains.

package common

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"regexp"
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
	// a profile may pin its valoper for read-only views (no signer needed)
	if c.Profile != nil && c.Profile.Metadata["valoper"] != "" {
		return c.Profile.Metadata["valoper"], nil
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
		"acc-num":   toolkit.Int("explicit account number (offline signing)"),
		"seq":       toolkit.Int("explicit account sequence (offline signing)"),
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
	opt := tx.Options{
		GasPrice: a.String("gas-price", ""),
		GasLimit: uint64(a.Int("gas-limit", 0)),
		FeeDenom: a.String("fee-denom", ""),
	}
	if a.Has("seq") {
		opt = opt.WithAccount(uint64(a.Int("acc-num", 0)), uint64(a.Int("seq", 0)))
	}
	return opt
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
	var b strings.Builder
	// Sequence drift can surface at any stage (simulate, CheckTx, deliver).
	// One automatic rebuild+retry with a fresh seq heals it — the doc is
	// re-approved since its bytes changed.
	for attempt := 0; ; attempt++ {
		built, err := tb.Build(c, msgs, opt)
		if err != nil {
			if retrySeq(err.Error(), attempt, &opt) {
				fmt.Fprintln(os.Stderr, "sequence drifted at simulation — rebuilding with fresh seq")
				continue
			}
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
			if retrySeq(err.Error(), attempt, &opt) {
				fmt.Fprintln(os.Stderr, "sequence drifted — rebuilding with fresh seq and retrying")
				continue
			}
			return nil, err
		}
		fmt.Fprintf(&b, "tx hash: %s\n", hash)
		if code != 0 {
			// CheckTx rejected — never reached a block.
			if retrySeq(rawLog, attempt, &opt) {
				fmt.Fprintln(os.Stderr, "sequence drifted — rebuilding with fresh seq and retrying")
				continue
			}
			return nil, fmt.Errorf("mempool rejected tx (code %d): %s", code, rawLog)
		}
		// SYNC only means mempool-accepted; confirm the committed result.
		final, err := tb.Confirm(c, hash)
		if err != nil {
			fmt.Fprintf(&b, "broadcast accepted; confirmation pending: %v\n", err)
			fmt.Fprintln(&b, "check later: cometcli tx get "+hash)
			return &toolkit.Result{Text: b.String(), Data: map[string]any{
				"hash": hash, "pending": true, "doc": built.Doc,
			}}, nil
		}
		if retrySeq(final.RawLog, attempt, &opt) {
			fmt.Fprintln(os.Stderr, "sequence drifted — rebuilding with fresh seq and retrying")
			continue
		}
		fmt.Fprintf(&b, "height:  %d\ncode:    %d\ngas:     %d used\n", final.Height, final.Code, final.GasUsed)
		if final.RawLog != "" && final.Code != 0 {
			fmt.Fprintf(&b, "log:     %s\n", final.RawLog)
		}
		if final.Code != 0 {
			return nil, fmt.Errorf("tx failed in block (code %d): %s", final.Code, final.RawLog)
		}
		return &toolkit.Result{Text: b.String(), Data: map[string]any{
			"hash": hash, "height": final.Height, "code": final.Code,
			"gas_used": final.GasUsed, "raw_log": final.RawLog, "doc": built.Doc,
		}}, nil
	}
}

// retrySeq reports whether the failure is a sequence mismatch worth one
// automatic rebuild — impossible when the caller pinned the seq explicitly,
// and never retried more than once. On a healable mismatch it sets
// opt.ForceSeq to the chain's expected value so a mempool-pending tx
// doesn't leave the re-query returning the same stale seq.
var expectedSeqRe = regexp.MustCompile(`expected (\d+)`)

func retrySeq(log string, attempt int, opt *tx.Options) bool {
	if !seqMismatch(log) || attempt != 0 || opt.PinnedAccount() {
		return false
	}
	if m := expectedSeqRe.FindStringSubmatch(log); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			opt.ForceSeq = n
		}
	}
	return true
}

// seqMismatch matches the SDK's wrong-sequence error text.
func seqMismatch(rawLog string) bool {
	s := strings.ToLower(rawLog)
	return strings.Contains(s, "account sequence") || strings.Contains(s, "sequence mismatch")
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
