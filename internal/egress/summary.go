package egress

import (
	"fmt"
	"regexp"
	"strings"
)

// Mode is how much raw output may reach the model.
type Mode string

const (
	// Strict: shell, file and log output reach the model only as local
	// summaries (masked, deduplicated lines). Default on validators.
	Strict Mode = "strict"
	// Filtered: raw output after redaction and the key-material gate.
	Filtered Mode = "filtered"
)

// ModeFor picks the egress mode: an explicit setting wins; otherwise
// validators are strict and everything else filtered.
func ModeFor(setting, role string, nodeMode bool) Mode {
	switch Mode(strings.ToLower(setting)) {
	case Strict:
		return Strict
	case Filtered:
		return Filtered
	}
	if nodeMode && (role == "" || role == "validator") {
		return Strict
	}
	return Filtered
}

// rawTools return raw shell, file or log output; on strict profiles the
// model sees their local summary instead.
var rawTools = map[string]bool{
	"bash": true, "read": true, "grep": true,
	"node.logs": true, "fleet.exec": true, "fleet.shell": true,
}

// maskedTools return structured results with embedded log lines; those
// lines are masked but the structure is kept.
var maskedTools = map[string]bool{
	"node.triage": true, "fleet.triage": true, "val.jail-check": true, "node.config": true,
	// peer topology: node ids and IPs of sentries and peers
	"node.peers": true, "sec.exposure": true, "fleet.status": true,
}

// ForModel turns a tool's output into what the model may see in mode.
func ForModel(mode Mode, tool, text string) string {
	if mode != Strict {
		return text
	}
	switch {
	case rawTools[tool]:
		return Summarize(text)
	case maskedTools[tool]:
		return Mask(text)
	}
	return text
}

var (
	blobRe   = regexp.MustCompile(`[A-Za-z0-9+/_=-]{24,}`)
	hexRe    = regexp.MustCompile(`\b(0x)?[0-9a-fA-F]{20,}\b`)
	ipv4Re   = regexp.MustCompile(`\b(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\b`)
	ipv6Re   = regexp.MustCompile(`\b([0-9a-fA-F]{1,4}:){3,7}[0-9a-fA-F]{1,4}\b`)
	bech32Re = regexp.MustCompile(`\b([a-z]{2,20}1)[02-9ac-hj-np-z]{38,90}\b`)
	emailRe  = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	urlRe    = regexp.MustCompile(`\b([a-z][a-z0-9+.-]*://)([^/\s:@]+)`)
	digitsRe = regexp.MustCompile(`\d+`)
	alertRe  = regexp.MustCompile(`(?i)\b(err(or)?|eror|panic|fatal|fail(ed|ure)?|warn(ing)?|wrn|denied|refused|killed|oom|timeout|timed out|cannot|can't|unable|invalid|corrupt|no space)\b`)
)

// Mask hides identifiers and blobs in a line while keeping its shape:
// addresses, keys, hashes, IPs and hostnames become placeholders; words,
// numbers, paths and log levels stay.
func Mask(s string) string {
	s = emailRe.ReplaceAllString(s, "‹email›")
	s = urlRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := urlRe.FindStringSubmatch(m)
		if sub[2] == "localhost" || sub[2] == "127.0.0.1" || sub[2] == "0.0.0.0" {
			return m
		}
		return sub[1] + "‹host›"
	})
	s = bech32Re.ReplaceAllString(s, "${1}‹addr›")
	s = hexRe.ReplaceAllString(s, "‹hex›")
	s = ipv4Re.ReplaceAllStringFunc(s, func(ip string) string {
		if strings.HasPrefix(ip, "127.") || ip == "0.0.0.0" {
			return ip
		}
		return "‹ip›"
	})
	s = ipv6Re.ReplaceAllString(s, "‹ip›")
	s = blobRe.ReplaceAllStringFunc(s, func(b string) string {
		// keep long plain words and paths (snake_case, kebab, dotted names)
		if !strings.ContainsAny(b, "0123456789") || strings.Count(b, "/") >= 2 {
			return b
		}
		return "‹blob›"
	})
	return s
}

// maxGroups bounds a summary's distinct lines.
const maxGroups = 60

// Summarize makes the local stand-in for raw output: masked lines, with
// repeats (same line up to numbers) folded into one with a count, and
// error-like lines kept first when there are too many to show.
func Summarize(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	type group struct {
		line  string // latest masked instance
		count int
		alert bool
		first int
	}
	idx := map[string]*group{}
	var order []*group
	for i, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		if l == "" {
			continue
		}
		m := Mask(l)
		key := digitsRe.ReplaceAllString(m, "#")
		g := idx[key]
		if g == nil {
			g = &group{first: i, alert: alertRe.MatchString(m)}
			idx[key] = g
			order = append(order, g)
		}
		g.line = m
		g.count++
	}
	shown := order
	dropped := 0
	if len(order) > maxGroups {
		var alerts, rest []*group
		for _, g := range order {
			if g.alert {
				alerts = append(alerts, g)
			} else {
				rest = append(rest, g)
			}
		}
		if len(alerts) > maxGroups {
			alerts = alerts[len(alerts)-maxGroups:] // the most recent
		}
		room := maxGroups - len(alerts)
		if room > len(rest) {
			room = len(rest)
		}
		keep := map[*group]bool{}
		for _, g := range alerts {
			keep[g] = true
		}
		for _, g := range rest[len(rest)-room:] { // the latest context lines
			keep[g] = true
		}
		shown = nil
		for _, g := range order {
			if keep[g] {
				shown = append(shown, g)
			}
		}
		dropped = len(order) - len(shown)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[local summary: %d lines, %d distinct; identifiers masked, the raw output stays on this machine (strict egress)]\n", len(lines), len(order))
	for _, g := range shown {
		b.WriteString(g.line)
		if g.count > 1 {
			fmt.Fprintf(&b, "  (×%d)", g.count)
		}
		b.WriteByte('\n')
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "[%d other distinct lines not shown — narrow the command (grep, tail) to see them]\n", dropped)
	}
	return b.String()
}
