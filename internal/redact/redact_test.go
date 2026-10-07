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

func TestProseIsNotAMnemonic(t *testing.T) {
	q := "why is my validator missing blocks and what should i do about it right now please"
	if out := Text(q); out != q {
		t.Fatalf("plain question mangled: %q", out)
	}
}

func TestMnemonicInsideProse(t *testing.T) {
	m := "legal winner thank year wave sausage worth useful legal winner thank yellow"
	out := Text("my seed is " + m + " is that ok")
	if strings.Contains(out, "sausage") || !strings.Contains(out, "[REDACTED_MNEMONIC]") {
		t.Fatalf("mnemonic not redacted: %q", out)
	}
	if !strings.HasPrefix(out, "my seed is ") || !strings.HasSuffix(out, " is that ok") {
		t.Fatalf("surrounding prose lost: %q", out)
	}
	// multi-line / double-spaced mnemonics too
	if out := Text(strings.ReplaceAll(m, " ", "\n  ")); strings.Contains(out, "sausage") {
		t.Fatalf("multiline mnemonic leaked: %q", out)
	}
}

func TestRedactorHosts(t *testing.T) {
	r := NewRedactor("val.internal", "10.0.4.7", "localhost", "")
	in := "ssh ops@val.internal; peer abc@10.0.4.7:26656; rpc localhost:26657; val.internal.example stays? 110.0.4.71 stays"
	out := r.Text(in)
	for _, leak := range []string{"ops@val.internal;", "@10.0.4.7:"} {
		if strings.Contains(out, leak) {
			t.Fatalf("host leaked (%s): %q", leak, out)
		}
	}
	if !strings.Contains(out, "localhost:26657") || !strings.Contains(out, "110.0.4.71") {
		t.Fatalf("over-redacted: %q", out)
	}
	if got := r.Args(map[string]any{"h": "val.internal"})["h"]; got != "[REDACTED_HOST]" {
		t.Fatalf("args not host-redacted: %v", got)
	}
	var nilR *Redactor
	if nilR.Text("x") != "x" {
		t.Fatal("nil redactor must behave like Text")
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"tcp://10.1.2.3:26657":      "10.1.2.3",
		"val.internal:9090":         "val.internal",
		"https://rpc.example.com/x": "rpc.example.com",
		"http://u:p@node.lan:8545":  "node.lan",
		"[fd00::1]:9090":            "fd00::1",
		"":                          "",
	} {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHashesAreNotKeys(t *testing.T) {
	h := "B240273F16D1F385FB9C4FC5CEB0820ECE4CC7C2C03B159FD22359CA7DAA014C"
	for _, in := range []string{"tx hash: " + h, "txhash=" + h, `{"hash": "0x` + strings.ToLower(h) + `"}`, "app_hash=" + h, "TxHash " + h} {
		if got := Text(in); !strings.Contains(strings.ToUpper(got), h) {
			t.Errorf("hash redacted: %q → %q", in, got)
		}
	}
	key := strings.ToLower(h)
	for _, in := range []string{"key " + key, "priv=" + key, key, "export: 0x" + key} {
		if got := Text(in); strings.Contains(got, key) {
			t.Errorf("key not redacted: %q", got)
		}
	}
}
