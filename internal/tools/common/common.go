// Package common holds helpers shared by tool domains.

package common

import (
	"bytes"
	query "cosmossdk.io/api/cosmos/base/query/v1beta1"
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
	// the node knows which validator it is: its consensus key is on chain
	if v, err := ValoperFromNode(c); err == nil {
		return v, nil
	} else if c.Profile == nil || c.Profile.Signer.Key == "" {
		return "", fmt.Errorf("can't tell which validator this node is (%v) — pass --validator <valoper>, or save it: cometcli profile add %s --valoper <valoper>", err, profileName(c))
	}
	tb, err := c.Tx()
	if err != nil {
		return "", fmt.Errorf("can't tell which validator this node is — pass --validator <valoper>, or save it: cometcli profile add %s --valoper <valoper> (%v)", profileName(c), err)
	}
	return tb.ValAddress()
}

func profileName(c *toolkit.Context) string {
	if c.Profile != nil && c.Profile.Name != "" {
		return c.Profile.Name
	}
	return "<profile>"
}

// identify learns which validator the node is before a signer is picked,
// so the signer can be matched to the operator account.
func identify(c *toolkit.Context) {
	if c.Profile != nil && c.Profile.Metadata["valoper"] == "" {
		_, _ = ValoperFromNode(c)
	}
}

// ValoperFromNode finds the validator this node signs for: the node's
// consensus address (CometBFT status) matched against every validator's
// consensus key on chain. The result is remembered on the profile for
// the rest of the process (not saved to disk — init does that).
func ValoperFromNode(c *toolkit.Context) (string, error) {
	if c.Profile == nil {
		return "", fmt.Errorf("no profile")
	}
	cc, err := c.Comet()
	if err != nil {
		return "", err
	}
	st, err := cc.Status(c)
	if err != nil {
		return "", err
	}
	want := []byte(st.ValidatorInfo.Address)
	if len(want) == 0 {
		return "", fmt.Errorf("the node reports no consensus key")
	}
	g, err := c.GRPC()
	if err != nil {
		return "", err
	}
	var key []byte
	for {
		res, err := g.Staking.Validators(c, &stakingv1beta1.QueryValidatorsRequest{Pagination: &query.PageRequest{Key: key, Limit: 200}})
		if err != nil {
			return "", err
		}
		for _, v := range res.Validators {
			if v.ConsensusPubkey == nil {
				continue
			}
			pub := protowireFieldBytes(v.ConsensusPubkey.Value, 1)
			if pub == nil {
				continue
			}
			sum := sha256.Sum256(pub)
			if bytes.Equal(sum[:20], want) {
				if c.Profile.Metadata == nil {
					c.Profile.Metadata = map[string]string{}
				}
				c.Profile.Metadata["valoper"] = v.OperatorAddress
				return v.OperatorAddress, nil
			}
		}
		if res.Pagination == nil || len(res.Pagination.NextKey) == 0 {
			break
		}
		key = res.Pagination.NextKey
	}
	return "", fmt.Errorf("no validator on chain uses this node's consensus key — it isn't a validator (yet), or runs a different key than the one registered")
}

// Account resolves the bech32 address of the key that will sign — the
// same choice BroadcastMsgs uses, so messages and signature always agree.
func Account(c *toolkit.Context) (string, error) {
	identify(c)
	tb, err := c.TxSigner()
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
	identify(c)
	tb, err := c.TxSigner()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	// Sequence drift can surface at any stage (simulate, CheckTx, deliver).
	// One automatic rebuild+retry with a fresh seq heals it — the doc is
	// re-approved since its bytes changed.
	for attempt := 0; ; attempt++ {
		prep, err := tb.Prepare(c, msgs, opt)
		if err != nil {
			if retrySeq(err.Error(), attempt, &opt) {
				fmt.Fprintln(os.Stderr, "sequence drifted at simulation — rebuilding with fresh seq")
				continue
			}
			return nil, err
		}
		built := &tx.Built{Doc: prep.Doc}
		detail := map[string]any{"doc": built.Doc.String()}
		for k, v := range meta {
			detail[k] = v
		}
		if err := c.Approve("broadcast transaction\n"+built.Doc.String(), toolkit.TierOnChain, detail); err != nil {
			return nil, err
		}
		// sign only after approval: a container keyring asks for its
		// password here, never for a transaction that was declined
		if built.TxBytes, err = tb.Sign(c, prep); err != nil {
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
			return nil, fmt.Errorf("mempool rejected tx (code %d): %s%s", code, rawLog, TxHint(rawLog))
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
			return nil, fmt.Errorf("tx failed in block (code %d): %s%s", final.Code, final.RawLog, TxHint(final.RawLog))
		}
		return &toolkit.Result{Text: b.String(), Data: map[string]any{
			"hash": hash, "height": final.Height, "code": final.Code,
			"gas_used": final.GasUsed, "raw_log": final.RawLog, "doc": built.Doc,
		}}, nil
	}
}

// txHints maps SDK failure messages to their cause and the next step.
var txHints = []struct{ match, hint string }{
	{"validator still jailed", "the jail period isn't over yet — wait until jailed_until (wait.until condition=unjailable), then retry"},
	{"validator not jailed", "the validator is already unjailed — verify with val.consensus"},
	{"self delegation less than minimum", "self-delegation is below min_self_delegation — tx.delegate the difference, then retry"},
	{"self-delegation is too low", "self-delegation is below min_self_delegation — tx.delegate the difference, then retry"},
	{"cannot be unjailed", "check val.jail-check for what blocks the unjail"},
	{"tombstoned", "the validator is tombstoned (double-sign) and can never be unjailed"},
	{"validator does not exist", "the signing key isn't this validator's operator key — sign with the operator key"},
	{"no validator", "the signing key isn't this validator's operator key — sign with the operator key"},
	{"insufficient fee", "raise the gas price (gas-price) or fee — the node's minimum-gas-prices is higher"},
	{"insufficient funds", "the signer account can't cover the amount plus fees — fund it"},
	{"out of gas", "set a higher gas-limit (simulation underestimated)"},
	{"signature verification failed", "wrong chain-id, account number or key — check the profile's chain_id and the signer"},
	{"tx already in mempool", "an identical tx is pending — wait for it (tx.get) instead of resending"},
	{"mempool is full", "the node's mempool is full — retry shortly or via another node"},
}

// TxHint explains a known transaction failure ("" when unknown).
func TxHint(rawLog string) string {
	low := strings.ToLower(rawLog)
	for _, h := range txHints {
		if strings.Contains(low, h.match) {
			return "\n→ " + h.hint
		}
	}
	return ""
}

// retrySeq reports whether the failure is a sequence mismatch worth one
// automatic rebuild — impossible when the caller pinned the seq explicitly,
// and never retried more than once. On a healable mismatch it sets
// a forced sequence (the chain's expected value) so a mempool-pending tx
// doesn't leave the re-query returning the same stale seq.
var expectedSeqRe = regexp.MustCompile(`expected (\d+)`)

func retrySeq(log string, attempt int, opt *tx.Options) bool {
	if !seqMismatch(log) || attempt != 0 || opt.PinnedAccount() {
		return false
	}
	if m := expectedSeqRe.FindStringSubmatch(log); m != nil {
		if n, err := strconv.ParseUint(m[1], 10, 64); err == nil {
			*opt = opt.ForceSequence(n)
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

// DecScaled converts a human decimal ("0.10") to the fixed-18 integer string
// ("100000000000000000") SDK Dec fields expect on the wire. Values already in
// scaled form (integers > 2) pass through untouched.
func DecScaled(s string) (string, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", fmt.Errorf("invalid decimal %q", s)
	}
	if !strings.Contains(s, ".") && !strings.ContainsAny(s, "/eE") && r.IsInt() && r.Num().Cmp(big.NewInt(2)) > 0 {
		return s, nil // already scaled
	}
	scaled := new(big.Rat).Mul(r, big.NewRat(1000000000000000000, 1))
	if !scaled.IsInt() {
		return "", fmt.Errorf("%q exceeds 18 decimal places", s)
	}
	return scaled.Num().String(), nil
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
