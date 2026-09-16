// Package common holds helpers shared by tool domains.

package common

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
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

// BroadcastMsgs is the shared on-chain flow every tx tool uses:
// build (with gas simulation) → human approval gate → broadcast → report.
// The tx Doc (decoded messages, fee, gas) is always shown before approval.
func BroadcastMsgs(c *toolkit.Context, msgs tx.Msgs, memo string, meta map[string]string) (*toolkit.Result, error) {
	tb, err := c.Tx()
	if err != nil {
		return nil, err
	}
	built, err := tb.Build(c, msgs, tx.Options{Memo: memo})
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
