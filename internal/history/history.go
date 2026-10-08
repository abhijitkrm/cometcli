// Package history keeps a per-profile record of triage signals over time
// (~/.cometcli/history/<profile>/YYYY-MM-DD.jsonl), so questions like
// "when did peers start dropping?" can be answered from data instead of
// a single snapshot. Records older than Retention are pruned.
package history

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// Retention is how long records are kept.
var Retention = 30 * 24 * time.Hour

// Record is one sample of signals.
type Record struct {
	TS      time.Time      `json:"ts"`
	Signals map[string]any `json:"s"`
}

func dir(profile string) (string, error) {
	if profile == "" || strings.ContainsAny(profile, `/\`) || strings.HasPrefix(profile, ".") {
		return "", fmt.Errorf("history: bad profile name %q", profile)
	}
	return config.Path("history", profile)
}

// keep reports whether a signal is worth storing: numbers, booleans and
// short strings (states, versions) — not addresses, log lines or paths.
func keep(k string, v any) bool {
	switch x := v.(type) {
	case float64, bool:
		return true
	case string:
		return len(x) <= 32 && !strings.Contains(k, "addr") && !strings.HasSuffix(k, ".file") &&
			k != "logs.last_error" && k != "val.valoper"
	}
	return false
}

// Append stores one sample for profile.
func Append(profile string, ts time.Time, signals map[string]any) error {
	d, err := dir(profile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	rec := Record{TS: ts.UTC(), Signals: map[string]any{}}
	for k, v := range signals {
		if keep(k, v) {
			rec.Signals[k] = v
		}
	}
	p := filepath.Join(d, ts.UTC().Format("2006-01-02")+".jsonl")
	_, statErr := os.Stat(p)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(rec); err != nil {
		return err
	}
	if os.IsNotExist(statErr) {
		prune(d, ts)
	}
	return nil
}

// prune drops day files older than Retention (on the first write of a day).
func prune(d string, now time.Time) {
	files, _ := filepath.Glob(filepath.Join(d, "*.jsonl"))
	cut := now.Add(-Retention).UTC().Format("2006-01-02")
	for _, f := range files {
		if strings.TrimSuffix(filepath.Base(f), ".jsonl") < cut {
			_ = os.Remove(f)
		}
	}
}

// Load returns profile's records since t, oldest first.
func Load(profile string, since time.Time) ([]Record, error) {
	d, err := dir(profile)
	if err != nil {
		return nil, err
	}
	files, _ := filepath.Glob(filepath.Join(d, "*.jsonl"))
	sort.Strings(files)
	first := since.UTC().Format("2006-01-02")
	var out []Record
	for _, f := range files {
		if strings.TrimSuffix(filepath.Base(f), ".jsonl") < first {
			continue
		}
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var r Record
			if json.Unmarshal(sc.Bytes(), &r) == nil && !r.TS.Before(since) {
				out = append(out, r)
			}
		}
		fh.Close()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, nil
}

// Last is the most recent record before t (nil if none within Retention).
func Last(profile string, before time.Time) *Record {
	recs, err := Load(profile, before.Add(-Retention))
	if err != nil {
		return nil
	}
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].TS.Before(before) {
			return &recs[i]
		}
	}
	return nil
}

// Change is a moment a signal changed value.
type Change struct {
	TS       time.Time
	From, To any
}

// Summary describes one signal across records.
type Summary struct {
	Name        string
	Samples     int
	First, Last any
	Min, Max    float64 // numeric signals only
	Numeric     bool
	Changes     []Change // value changes (bool/string), or big jumps (numeric)
	Spark       string   // numeric trend, oldest → newest
}

// Summarize describes name across recs.
func Summarize(recs []Record, name string) Summary {
	s := Summary{Name: name, Min: math.Inf(1), Max: math.Inf(-1)}
	var prev any
	var nums []float64
	for _, r := range recs {
		v, ok := r.Signals[name]
		if !ok {
			continue
		}
		s.Samples++
		if s.Samples == 1 {
			s.First = v
		}
		s.Last = v
		if f, ok := v.(float64); ok {
			s.Numeric = true
			nums = append(nums, f)
			s.Min, s.Max = math.Min(s.Min, f), math.Max(s.Max, f)
			if p, ok := prev.(float64); ok && bigJump(p, f) {
				s.Changes = append(s.Changes, Change{r.TS, p, f})
			}
		} else if prev != nil && fmt.Sprint(prev) != fmt.Sprint(v) {
			s.Changes = append(s.Changes, Change{r.TS, prev, v})
		}
		prev = v
	}
	if s.Numeric {
		s.Spark = spark(nums, 24)
	}
	return s
}

// bigJump is a change worth calling out: ≥ 20% and ≥ 1 in absolute terms.
func bigJump(a, b float64) bool {
	d := math.Abs(b - a)
	return d >= 1 && d >= 0.2*math.Max(math.Abs(a), math.Abs(b))
}

// spark draws values (averaged into at most n buckets) as a sparkline.
func spark(vals []float64, n int) string {
	if len(vals) == 0 {
		return ""
	}
	if len(vals) > n {
		b := make([]float64, n)
		for i := range b {
			lo, hi := i*len(vals)/n, (i+1)*len(vals)/n
			sum := 0.0
			for _, v := range vals[lo:hi] {
				sum += v
			}
			b[i] = sum / float64(hi-lo)
		}
		vals = b
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	bars := []rune("▁▂▃▄▅▆▇█")
	var out []rune
	for _, v := range vals {
		i := 0
		if hi > lo {
			i = int((v - lo) / (hi - lo) * float64(len(bars)-1))
		}
		out = append(out, bars[i])
	}
	return string(out)
}

// noisy signals change every sample by design; deltas skip them.
func noisy(k string) bool {
	for _, s := range []string{"height", "block_age", "uptime", "window_min", "logs.lines", "disk_free", "pvs.ahead_by", "load_per_cpu", "mem_used_pct_of_limit"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// Deltas lists what changed meaningfully between two samples, sorted:
// any boolean/string change, numeric jumps of ≥ 20%.
func Deltas(prev, cur map[string]any) []string {
	var out []string
	for k, v := range cur {
		if noisy(k) || !keep(k, v) {
			continue
		}
		p, ok := prev[k]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case float64:
			if pf, ok := p.(float64); ok && bigJump(pf, x) {
				out = append(out, fmt.Sprintf("%s %s→%s", k, num(pf), num(x)))
			}
		default:
			if fmt.Sprint(p) != fmt.Sprint(v) {
				out = append(out, fmt.Sprintf("%s %v→%v", k, p, v))
			}
		}
	}
	sort.Strings(out)
	return out
}

func num(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%.1f", f)
}
