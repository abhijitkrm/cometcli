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

func TestSensitiveName(t *testing.T) {
	if !IsSensitiveName("/home/x/.evmd/config/priv_validator_key.json") {
		t.Fatal("priv_validator_key.json must be sensitive")
	}
	if IsSensitiveName("/home/x/.evmd/config/config.toml") {
		t.Fatal("config.toml should not be sensitive")
	}
}
