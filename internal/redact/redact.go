// Package redact scrubs sensitive material before any text reaches an LLM
// context or leaves the box. Applied to context snapshots, tool results,
// and user-visible echoes.
package redact

import (
	"regexp"
	"strings"

	"github.com/cosmos/go-bip39"
)

var (
	// candidate mnemonic span: 12+ whitespace-separated lowercase words.
	// Spans are confirmed word-by-word against the BIP-39 list so ordinary
	// prose ("why is my validator missing blocks …") is left alone.
	mnemonicRe = regexp.MustCompile(`\b([a-z]+\s+){11,}[a-z]+\b`)
	wordRe     = regexp.MustCompile(`[a-z]+`)
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
	s = mnemonicRe.ReplaceAllStringFunc(s, scrubMnemonic)
	s = scrubHex64(s)
	s = privValRe.ReplaceAllString(s, `"$1": "[REDACTED]"`)
	s = keyValueRe.ReplaceAllString(s, `"value": "[REDACTED]"`)
	s = kvSecretRe.ReplaceAllString(s, "[REDACTED_SECRET]")
	s = bearerRe.ReplaceAllString(s, "Bearer [REDACTED]")
	s = jwtRe.ReplaceAllString(s, "[REDACTED_JWT]")
	s = urlCredRe.ReplaceAllString(s, "$1://[REDACTED_CRED]@")
	return s
}

// hashLabelRe ends with a label that marks the following hex as a hash
// (tx, block, app hash): public data the operator needs, not a key.
var hashLabelRe = regexp.MustCompile(`(?i)(hash["']?\s*(:|=)?\s*["']?|\btxs?\s+(get\s+)?(--hash\s+)?|\b(query|q)\s+tx\s+(--type[= ]hash\s+)?)$`)

// scrubHex64 redacts 64-char hex (a private key's shape) unless it is
// labelled as a hash: "tx hash: AB…", "txhash=…", "\"hash\": \"0x…\"".
func scrubHex64(s string) string {
	locs := hex64Re.FindAllStringIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, l := range locs {
		start := max(0, l[0]-24)
		b.WriteString(s[last:l[0]])
		if hashLabelRe.MatchString(s[start:l[0]]) {
			b.WriteString(s[l[0]:l[1]])
		} else {
			b.WriteString("[REDACTED_HEX]")
		}
		last = l[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// mnemonicLen is the shortest BIP-39 mnemonic (128-bit entropy).
const mnemonicLen = 12

// scrubMnemonic replaces every run of ≥12 consecutive BIP-39 words inside a
// candidate span, keeping surrounding non-mnemonic words intact.
func scrubMnemonic(span string) string {
	locs := wordRe.FindAllStringIndex(span, -1)
	var b strings.Builder
	last, runStart, runLen := 0, 0, 0
	flush := func(end int) {
		if runLen >= mnemonicLen {
			b.WriteString(span[last:locs[runStart][0]])
			b.WriteString("[REDACTED_MNEMONIC]")
			last = locs[end-1][1]
		}
	}
	for i, l := range locs {
		if _, ok := bip39.ReverseWordMap[span[l[0]:l[1]]]; ok {
			if runLen == 0 {
				runStart = i
			}
			runLen++
			continue
		}
		flush(i)
		runLen = 0
	}
	flush(len(locs))
	b.WriteString(span[last:])
	return b.String()
}

// Redactor layers profile-specific patterns (hostnames, IPs the operator
// flagged) over the stateless Text scrubber. The zero value behaves like Text.
type Redactor struct {
	hosts []*regexp.Regexp
}

// NewRedactor builds a redactor that also masks each literal host/IP.
func NewRedactor(hosts ...string) *Redactor {
	r := &Redactor{}
	seen := map[string]bool{}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] || isLoopback(h) {
			continue
		}
		seen[h] = true
		r.hosts = append(r.hosts, regexp.MustCompile(`(^|[^A-Za-z0-9.-])`+regexp.QuoteMeta(h)+`($|[^A-Za-z0-9-])`))
	}
	return r
}

// isLoopback skips hosts that carry no identifying information.
func isLoopback(h string) bool {
	switch h {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1":
		return true
	}
	return false
}

// Text scrubs s with the base rules plus the redactor's hosts.
func (r *Redactor) Text(s string) string {
	s = Text(s)
	if r == nil {
		return s
	}
	for _, re := range r.hosts {
		s = re.ReplaceAllString(s, "${1}[REDACTED_HOST]${2}")
	}
	return s
}

// HostOf extracts the host part of an endpoint ("tcp://h:26657", "h:9090",
// "https://h"), or "" when there is none.
func HostOf(endpoint string) string {
	e := endpoint
	if i := strings.Index(e, "://"); i >= 0 {
		e = e[i+3:]
	}
	if i := strings.IndexAny(e, "/?"); i >= 0 {
		e = e[:i]
	}
	if i := strings.LastIndex(e, "@"); i >= 0 {
		e = e[i+1:]
	}
	if strings.HasPrefix(e, "[") { // [ipv6]:port
		if j := strings.Index(e, "]"); j > 0 {
			return e[1:j]
		}
	}
	if i := strings.LastIndex(e, ":"); i >= 0 && strings.Count(e, ":") == 1 {
		e = e[:i]
	}
	return e
}

// Args deep-scrubs a map (tool args before logging/LLM echo).
func Args(m map[string]any) map[string]any { return argsWith(m, Text) }

// List scrubs a heterogeneous slice element-wise.
func List(l []any) []any { return listWith(l, Text) }

// Args deep-scrubs a map with the redactor's rules.
func (r *Redactor) Args(m map[string]any) map[string]any { return argsWith(m, r.Text) }

func argsWith(m map[string]any, f func(string) string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = f(t)
		case map[string]any:
			out[k] = argsWith(t, f)
		case []any:
			out[k] = listWith(t, f)
		case []string:
			l := make([]string, len(t))
			for i, s := range t {
				l[i] = f(s)
			}
			out[k] = l
		default:
			out[k] = v
		}
	}
	return out
}

func listWith(l []any, f func(string) string) []any {
	out := make([]any, len(l))
	for i, v := range l {
		switch t := v.(type) {
		case string:
			out[i] = f(t)
		case map[string]any:
			out[i] = argsWith(t, f)
		case []any:
			out[i] = listWith(t, f)
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
