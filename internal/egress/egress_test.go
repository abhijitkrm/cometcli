package egress

import (
	"strings"
	"testing"

	"github.com/cosmos/go-bip39"
)

func TestScanFindsKeyMaterial(t *testing.T) {
	ent, _ := bip39.NewEntropy(256)
	mn, _ := bip39.NewMnemonic(ent)
	for name, in := range map[string]string{
		"mnemonic":           "recover with: " + mn,
		"mnemonic upper":     strings.ToUpper(mn),
		"mnemonic in json":   `{"mnemonic":"` + mn + `"}`,
		"priv_validator_key": `{"address":"AB","pub_key":{"type":"tendermint/PubKeyEd25519","value":"x"},"priv_key":{"type":"tendermint/PrivKeyEd25519","value":"q2Ej1vT0Yd8kbm0GhX9wJ0Yv6p7c9W3qkH1sXy8bVwQ0c2Vt9tO3y7mJx3qT8f3h4Lr2Vb6ZpN9w0Qn1aYk5sA=="}}`,
		"openssh":            "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----",
		"armored cosmos":     "-----BEGIN TENDERMINT PRIVATE KEY-----\nkdf: bcrypt\n",
		"eth private key":    "private key: 0x4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318",
		"keystore":           `{"crypto":{"cipher":"aes-128-ctr","ciphertext":"5318b4d5bcd28de64ee5559e671353e16f075ecae9f99c7a79a38af5f869aa46","kdf":"scrypt"}}`,
		"file keyring":       "eyJhbGciOiJQQkVTMi1IUzI1NitBMTI4S1ciLCJjcmVhdGVkIjoiMjAyNi0x",
		"anthropic key":      "ANTHROPIC_API_KEY=sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		"openrouter key":     "sk-or-v1-0123456789abcdef0123456789abcdef0123456789abcdef",
		"github token":       "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"telegram token":     "123456789:AAEhBP0av28pWx0nPnRk3wd0Cf6bS7m1aZQ",
	} {
		if len(Scan(in)) == 0 {
			t.Errorf("%s: not detected", name)
		}
	}
}

func TestScanLeavesOrdinaryOutputAlone(t *testing.T) {
	for _, in := range []string{
		"The validator is jailed because the node was down for longer than the signed blocks window allows.",
		"txhash: 9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08",
		`{"pub_key":{"type":"tendermint/PubKeyEd25519","value":"q2Ej1vT0Yd8kbm0GhX9wJ0Yv6p7c9W3qkH1sXy8bVwQ="}}`,
		"block hash 0x4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318 at height 1200",
		"cosmosvaloper1gk5aka9mc8rw2uxm3xmzrtne526vj9t2ldw5c3 jailed until 2026-10-09T10:00:00Z",
		"open sesame: abandon the plan, the river is wide",   // a few BIP-39 words, not a run
		"word salad: ability able about above absent absorb", // 6-word run
		"-----BEGIN PUBLIC KEY-----",
	} {
		if f := Scan(in); len(f) != 0 {
			t.Errorf("false positive %v in %q", f, in)
		}
	}
}

func TestModeFor(t *testing.T) {
	cases := []struct {
		setting, role string
		node          bool
		want          Mode
	}{
		{"", "validator", true, Strict},
		{"", "", true, Strict},
		{"", "rpc", true, Filtered},
		{"", "validator", false, Filtered},
		{"filtered", "validator", true, Filtered},
		{"strict", "rpc", true, Strict},
	}
	for _, c := range cases {
		if got := ModeFor(c.setting, c.role, c.node); got != c.want {
			t.Errorf("ModeFor(%q,%q,%v) = %s, want %s", c.setting, c.role, c.node, got, c.want)
		}
	}
}

func TestSummarizeMasksAndFolds(t *testing.T) {
	var in strings.Builder
	for h := 100; h < 140; h++ {
		in.WriteString("2026-10-09T10:00:00Z INF committed state height=" + itoa(h) + " module=state\n")
	}
	in.WriteString("ERR dial tcp 10.0.3.7:26656: connection refused peer=4b2e7f0c9a1d3e5f7a9b1c3d5e7f9a1b3c5d7e9f\n")
	in.WriteString("validator cosmosvaloper1gk5aka9mc8rw2uxm3xmzrtne526vj9t2ldw5c3 rpc http://val1.internal.example:26657 laddr tcp://0.0.0.0:26657\n")
	in.WriteString("[exit code 1]\n")
	out := Summarize(in.String())
	for _, leak := range []string{"10.0.3.7", "4b2e7f0c9a1d", "gk5aka9mc8rw", "val1.internal"} {
		if strings.Contains(out, leak) {
			t.Errorf("summary leaks %q:\n%s", leak, out)
		}
	}
	for _, keep := range []string{"(×40)", "height=139", "connection refused", "cosmosvaloper1‹addr›", "tcp://0.0.0.0:26657", "[exit code 1]", "strict egress"} {
		if !strings.Contains(out, keep) {
			t.Errorf("summary lost %q:\n%s", keep, out)
		}
	}
}

func TestSummarizeKeepsErrorsWhenLong(t *testing.T) {
	var in strings.Builder
	in.WriteString("panic: runtime error: invalid memory address\n")
	for i := 0; i < 500; i++ {
		in.WriteString("line kind " + strings.Repeat("x", i%7+1) + " " + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + "\n")
	}
	out := Summarize(in.String())
	if !strings.Contains(out, "panic: runtime error") || !strings.Contains(out, "other distinct lines not shown") {
		t.Errorf("long output should keep the panic and say what was dropped:\n%s", out[:400])
	}
	if n := strings.Count(out, "\n"); n > maxGroups+3 {
		t.Errorf("summary has %d lines, cap is %d", n, maxGroups)
	}
}

func TestForModel(t *testing.T) {
	raw := "dial 10.1.2.3 failed\n"
	if ForModel(Filtered, "bash", raw) != raw {
		t.Error("filtered mode passes output through")
	}
	if strings.Contains(ForModel(Strict, "bash", raw), "10.1.2.3") {
		t.Error("strict bash output must be summarized")
	}
	if ForModel(Strict, "val.status", raw) != raw {
		t.Error("structured tools aren't summarized")
	}
}

func itoa(i int) string {
	return strings.TrimLeft(strings.Repeat("0", 3)+string(rune('0'+i/100))+string(rune('0'+i/10%10))+string(rune('0'+i%10)), "0")
}
