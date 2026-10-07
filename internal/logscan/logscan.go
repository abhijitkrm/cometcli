// Package logscan classifies CometBFT / Cosmos SDK / Cosmos-EVM node log
// lines into failure categories. Triage turns the counts into signals
// (logs.<slug>); incident forensics shows sample lines per category.
package logscan

import (
	"regexp"
	"strings"
)

// Category is one class of log line.
type Category struct {
	Slug string // signal suffix: logs.<slug>
	Name string // human label
	Re   *regexp.Regexp
}

// Categories are checked in order; a line can fall in several.
var Categories = []Category{
	{"panic", "crash/panic", regexp.MustCompile(`(?i)\bpanic\b|fatal error|CONSENSUS FAILURE|segmentation`)},
	{"oom", "out of memory", regexp.MustCompile(`(?i)out of memory|oom.?kill|cannot allocate`)},
	{"disk_io", "disk full / IO", regexp.MustCompile(`(?i)no space left|disk quota|input/output error|read-only file system`)},
	{"peer_net", "peer loss / network", regexp.MustCompile(`(?i)connection refused|i/o timeout|dial tcp|stopping peer|no peers|failed to (dial|connect)|network is unreachable`)},
	{"privval", "signer / privval", regexp.MustCompile(`(?i)privval|remote signer|failed to sign|error signing|sign (vote|proposal)`)},
	{"apphash", "app hash / state", regexp.MustCompile(`(?i)wrong app ?hash|apphash|wrong block\.header|state mismatch`)},
	{"clock", "clock / timing", regexp.MustCompile(`(?i)clock|time.*(skew|drift)|timed out waiting`)},
	{"too_many_files", "too many files", regexp.MustCompile(`(?i)too many open files`)},
	{"upgrade_needed", "upgrade halt", regexp.MustCompile(`UPGRADE "[^"]+" NEEDED`)},
	{"binary_early", "binary updated before upgrade", regexp.MustCompile(`(?i)BINARY UPDATED BEFORE TRIGGER|wrong app version|unknown upgrade|upgrade.*not found in plan`)},
	{"db_lock", "database locked", regexp.MustCompile(`(?i)resource temporarily unavailable|lock .*held|database is locked`)},
	{"db_corrupt", "database corruption", regexp.MustCompile(`(?i)corrupt|checksum mismatch|missing .*\.(sst|ldb)|manifest`)},
	{"height_regression", "privval height regression", regexp.MustCompile(`(?i)height regression|conflicting data|round regression|step regression`)},
	{"double_sign", "double sign", regexp.MustCompile(`(?i)double.?sign|duplicate ?vote ?evidence`)},
	{"mempool_full", "mempool full", regexp.MustCompile(`(?i)mempool is full`)},
	{"statesync_fail", "state sync failure", regexp.MustCompile(`(?i)state ?sync.*(fail|error|abort)|failed to (apply|restore|verify) snapshot|no available snapshots|no suitable snapshots`)},
	{"port_in_use", "port in use", regexp.MustCompile(`(?i)address already in use`)},
	{"config_parse", "config error", regexp.MustCompile(`(?i)error reading config|failed to (load|parse|unmarshal|decode).*(config|toml)|unknown (field|flag)|invalid (config|minimum gas)`)},
	{"chain_mismatch", "chain / genesis mismatch", regexp.MustCompile(`(?i)different network|genesis.*(mismatch|doesn't match|hash)|wrong genesis|incompatible chain.?id|invalid chain.?id`)},
	{"jail", "jail / slashing", regexp.MustCompile(`(?i)\bjail|liveness fault|tombston|slashing`)},
	{"evm_rpc", "EVM JSON-RPC errors", regexp.MustCompile(`(?i)json-?rpc.*(error|fail)|failed to start.*(json-?rpc|evm)|evm.*indexer.*(error|fail)`)},
	{"p2p_auth", "p2p handshake / auth", regexp.MustCompile(`(?i)auth failure|handshake.*fail|filtered|incompatible|peer.*rejected`)},
}

// Result holds per-category counts and sample lines.
type Result struct {
	Lines   int
	Counts  map[string]int      // slug → matching lines
	Samples map[string][]string // slug → up to MaxSamples lines
	// UpgradeName is the plan name from an "UPGRADE "x" NEEDED" line.
	UpgradeName string
}

// MaxSamples caps sample lines kept per category.
var MaxSamples = 3

var upgradeRe = regexp.MustCompile(`UPGRADE "([^"]+)" NEEDED`)

// Scan classifies every line of text.
func Scan(text string) *Result {
	r := &Result{Counts: map[string]int{}, Samples: map[string][]string{}}
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		r.Lines++
		for _, c := range Categories {
			if c.Re.MatchString(line) {
				r.Counts[c.Slug]++
				if len(r.Samples[c.Slug]) < MaxSamples {
					r.Samples[c.Slug] = append(r.Samples[c.Slug], Clip(line))
				}
			}
		}
		if m := upgradeRe.FindStringSubmatch(line); m != nil {
			r.UpgradeName = m[1]
		}
	}
	return r
}

// Name returns a category's label by slug.
func Name(slug string) string {
	for _, c := range Categories {
		if c.Slug == slug {
			return c.Name
		}
	}
	return slug
}

// Clip shortens a log line for display.
func Clip(s string) string {
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}
