// Package egress is the last check on everything cometcli sends to a model
// provider. Redaction (package redact) masks what it recognizes field by
// field; egress is the gate behind it: a message that still carries key
// material is withheld whole, never partially sent.
//
// It also makes the local summaries that stand in for raw command output
// on strict profiles (validators by default), so a node's shell and log
// output never leaves the machine verbatim.
package egress

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/cosmos/go-bip39"
)

// Finding is one kind of key material found in a text.
type Finding struct {
	Kind string // short name: "mnemonic", "pem-private-key", …
}

type detector struct {
	kind string
	re   *regexp.Regexp
}

// detectors recognize key material by shape. Each is specific enough that
// a match is worth withholding a whole message for.
var detectors = []detector{
	{"pem-private-key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----`)},
	{"consensus-private-key", regexp.MustCompile(`(?s)"priv_key"\s*:\s*\{\s*"type"\s*:\s*"[^"]*PrivKey[^"]*"\s*,\s*"value"\s*:\s*"[A-Za-z0-9+/=]{40,}"`)},
	{"consensus-private-key", regexp.MustCompile(`"type"\s*:\s*"tendermint/PrivKey[A-Za-z0-9]*"\s*,\s*"value"\s*:\s*"[A-Za-z0-9+/=]{40,}"`)},
	{"keystore", regexp.MustCompile(`(?s)"crypto"\s*:\s*\{.{0,400}"ciphertext"\s*:\s*"[0-9a-fA-F]{32,}"`)},
	{"private-key-hex", regexp.MustCompile(`(?i)\b(priv(ate)?[_ -]?key|secret[_ -]?key|privkey|sk)\b["'\s:=]{1,6}(0x)?[0-9a-fA-F]{64}\b`)},
	{"keyring-entry", regexp.MustCompile(`\beyJhbGciOiJQQkVTMi[A-Za-z0-9_-]{20,}`)}, // cosmos file keyring (PBES2 JWE)
	{"api-key", regexp.MustCompile(`\bsk-(ant|or-v1|proj)-[A-Za-z0-9_-]{20,}`)},
	{"api-key", regexp.MustCompile(`\b(ghp|gho|ghs|ghu)_[A-Za-z0-9]{30,}\b|\bgithub_pat_[A-Za-z0-9_]{40,}`)},
	{"api-key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"api-key", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{20,}`)},
	{"api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"bot-token", regexp.MustCompile(`\b[0-9]{8,10}:AA[A-Za-z0-9_-]{33}\b`)},
}

var wordRe = regexp.MustCompile(`[a-z]+`)

// mnemonicRun reports whether text holds 12 or more consecutive BIP-39
// words. Common English function words (the, a, is, of, to, and) aren't
// in the list, so prose essentially never makes such a run.
func mnemonicRun(s string) bool {
	run := 0
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		if _, ok := bip39.ReverseWordMap[w]; ok {
			run++
			if run >= 12 {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// Scan reports the kinds of key material in s (nil when clean).
func Scan(s string) []Finding {
	if s == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []Finding
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			out = append(out, Finding{Kind: k})
		}
	}
	for _, d := range detectors {
		if d.re.MatchString(s) {
			add(d.kind)
		}
	}
	if mnemonicRun(s) {
		add("mnemonic")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Kinds joins findings for messages.
func Kinds(f []Finding) string {
	var k []string
	for _, x := range f {
		k = append(k, x.Kind)
	}
	return strings.Join(k, ", ")
}

// Withheld is what replaces a message that carried key material.
func Withheld(f []Finding) string {
	return fmt.Sprintf("[withheld by cometcli: this content contained key material (%s) and never leaves this machine. "+
		"Don't ask for it again; keys are only used through cometcli's signing tools.]", Kinds(f))
}
