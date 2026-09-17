// Package redact scrubs sensitive material before any text reaches an LLM
// context or leaves the box. Applied to context snapshots, tool results,
// and user-visible echoes.
package redact

import (
	"regexp"
	"strings"
)

var (
	// BIP-39 style: 12+ consecutive lowercase words (loose match).
	mnemonicRe = regexp.MustCompile(`\b([a-z]+ ){11,}[a-z]+\b`)
	// 64-char hex (private keys), with or without 0x prefix
	hex64Re = regexp.MustCompile(`\b(?:0[xX])?[0-9a-fA-F]{64}\b`)
	// priv_validator_key.json / priv_validator_state.json payloads
	privValRe = regexp.MustCompile(`"(priv_key|signature|signbytes)"\s*:\s*[^,}]+`)
	// nested "value":"<base64>" inside key objects
	keyValueRe = regexp.MustCompile(`"value"\s*:\s*"[A-Za-z0-9+/=]{8,}"`)
	// generic key=value secrets
	kvSecretRe = regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|passphrase|mnemonic)["']?\s*[:=]\s*["']?\S+`)
	// bearer tokens / JWTs
	bearerRe = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{16,}`)
	jwtRe    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
	// credentials embedded in URLs: scheme://user:pass@host
	urlCredRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*)://[^/\s:@]+:[^/\s@]+@`)
)

// Text scrubs sensitive material from s.
func Text(s string) string {
	s = mnemonicRe.ReplaceAllString(s, "[REDACTED_MNEMONIC]")
	s = hex64Re.ReplaceAllString(s, "[REDACTED_HEX]")
	s = privValRe.ReplaceAllString(s, `"$1": "[REDACTED]"`)
	s = keyValueRe.ReplaceAllString(s, `"value": "[REDACTED]"`)
	s = kvSecretRe.ReplaceAllString(s, "[REDACTED_SECRET]")
	s = bearerRe.ReplaceAllString(s, "Bearer [REDACTED]")
	s = jwtRe.ReplaceAllString(s, "[REDACTED_JWT]")
	s = urlCredRe.ReplaceAllString(s, "$1://[REDACTED_CRED]@")
	return s
}

// Args deep-scrubs a map (tool args before logging/LLM echo).
func Args(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = Text(t)
		case map[string]any:
			out[k] = Args(t)
		case []any:
			out[k] = List(t)
		case []string:
			l := make([]string, len(t))
			for i, s := range t {
				l[i] = Text(s)
			}
			out[k] = l
		default:
			out[k] = v
		}
	}
	return out
}

// List scrubs a heterogeneous slice element-wise.
func List(l []any) []any {
	out := make([]any, len(l))
	for i, v := range l {
		switch t := v.(type) {
		case string:
			out[i] = Text(t)
		case map[string]any:
			out[i] = Args(t)
		case []any:
			out[i] = List(t)
		default:
			out[i] = v
		}
	}
	return out
}

// IsSensitiveName reports whether a file path holds key material that must
// never be read into agent context.
func IsSensitiveName(path string) bool {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "priv_validator_key.json", "node_key.json":
		return true
	}
	return strings.Contains(base, "mnemonic") || strings.Contains(base, "private")
}
