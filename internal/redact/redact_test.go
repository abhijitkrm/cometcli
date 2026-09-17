package redact

import (
	"strings"
	"testing"
)

func TestMnemonic(t *testing.T) {
	s := "the mnemonic is abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about ok"
	out := Text(s)
	if strings.Contains(out, "abandon abandon abandon") {
		t.Fatalf("mnemonic not redacted: %s", out)
	}
	if !strings.Contains(out, "[REDACTED_MNEMONIC]") {
		t.Fatalf("missing marker: %s", out)
	}
}

func TestHex64(t *testing.T) {
	s := "key: " + strings.Repeat("ab", 32)
	if got := Text(s); !strings.Contains(got, "[REDACTED_HEX]") {
		t.Fatalf("hex not redacted: %s", got)
	}
}

func TestPrivValidator(t *testing.T) {
	s := `{"height":"123","priv_key":{"type":"t","value":"SECRETKEYMATERIAL"}}`
	out := Text(s)
	if strings.Contains(out, "SECRETKEYMATERIAL") {
		t.Fatalf("priv_validator not redacted: %s", out)
	}
}

func TestSecrets(t *testing.T) {
	for _, s := range []string{
		`api_key: sk-abc123def`,
		`"token" = "xoxb-12345"`,
		`Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dbsigsample`,
	} {
		if out := Text(s); strings.Contains(out, "xoxb-12345") || strings.Contains(out, "sk-abc123def") {
			t.Fatalf("secret not redacted: %s", out)
		}
	}
}

func TestHex64_0xPrefixed(t *testing.T) {
	s := "imported 0x" + strings.Repeat("ab", 32) + " into keyring"
	if got := Text(s); strings.Contains(got, strings.Repeat("ab", 32)) {
		t.Fatalf("0x-prefixed key not redacted: %s", got)
	}
}

func TestJWT_Bare(t *testing.T) {
	s := "auth header eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c end"
	out := Text(s)
	if strings.Contains(out, "SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c") {
		t.Fatalf("jwt not redacted: %s", out)
	}
	if !strings.Contains(out, "[REDACTED_JWT]") {
		t.Fatalf("missing marker: %s", out)
	}
}

func TestBearerHeader(t *testing.T) {
	s := "Authorization: Bearer tok_aaaabbbbccccdddd1234"
	if out := Text(s); strings.Contains(out, "aaaabbbbccccdddd") {
		t.Fatalf("bearer token not redacted: %s", out)
	}
}

func TestURLCredentials(t *testing.T) {
	for _, s := range []string{
		"dial https://admin:s3cr3tpw@node.example.com:26657",
		"grpc://operator:hunter2@10.0.0.5:9090 failed",
		"tcp://user:p%40ss@host:26657",
	} {
		out := Text(s)
		if strings.Contains(out, "s3cr3tpw") || strings.Contains(out, "hunter2") || strings.Contains(out, "p%40ss") {
			t.Fatalf("url creds not redacted: %s", out)
		}
		if !strings.Contains(out, "[REDACTED_CRED]@") {
			t.Fatalf("missing cred marker: %s", out)
		}
	}
}

func TestNestedPrivKeyValue(t *testing.T) {
	s := `{"pub_key":{"type":"x","value":"AAAA"},"priv_key":{"type":"y","value":"QUJDREVGR0g="}}`
	out := Text(s)
	if strings.Contains(out, "QUJDREVGR0g=") {
		t.Fatalf("nested priv_key.value not redacted: %s", out)
	}
}

func TestArgsRecursion(t *testing.T) {
	in := map[string]any{
		"outer": map[string]any{
			"key": "0x" + strings.Repeat("cd", 32),
		},
		"list":  []any{"ok", "Bearer abcdefghijklmnop1234"},
		"plain": "nothing sensitive",
	}
	out := Args(in)
	nested := out["outer"].(map[string]any)
	if strings.Contains(nested["key"].(string), strings.Repeat("cd", 32)) {
		t.Fatalf("nested hex not redacted: %v", nested)
	}
	lst := out["list"].([]any)
	if strings.Contains(lst[1].(string), "abcdefghijklmnop") {
		t.Fatalf("list element not redacted: %v", lst)
	}
	if out["plain"] != "nothing sensitive" {
		t.Fatalf("clean text was mangled: %v", out["plain"])
	}
}

func TestBenignNotRedacted(t *testing.T) {
	// 40-hex eth addresses and normal text must survive untouched.
	s := "validator 0x9858EfFD232B4033E47d90003D41EC34EcaEda94 voted on height 12345"
	if got := Text(s); got != s {
		t.Fatalf("benign text modified: %s", got)
	}
}

func TestSensitiveName(t *testing.T) {
	if !IsSensitiveName("/home/x/.evmd/config/priv_validator_key.json") {
		t.Fatal("priv_validator_key.json must be sensitive")
	}
	if IsSensitiveName("/home/x/.evmd/config/config.toml") {
		t.Fatal("config.toml should not be sensitive")
	}
}
