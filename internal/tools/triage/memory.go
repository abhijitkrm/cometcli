package triage

import (
	"fmt"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/history"
	"github.com/abhijitkrm/cometcli/internal/incidents"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// remember stores this sweep in the signal history and fills the report
// with what changed since the previous one and the node's past incidents.
func remember(c *toolkit.Context, r *Report) {
	if c.Profile == nil || c.Profile.Name == "" {
		return
	}
	name := c.Profile.Name
	now := time.Now()
	if prev := history.Last(name, now); prev != nil {
		r.Deltas = history.Deltas(prev.Signals, r.Signals)
		r.DeltaSince = prev.TS
	}
	_ = history.Append(name, now, r.Signals)
	r.Past, _ = incidents.List(name, now.Add(-30*24*time.Hour))
}

// pastLine summarizes earlier incidents on this node for the triage text.
func pastLine(past []incidents.Incident, hits map[string]bool) string {
	if len(past) == 0 {
		return ""
	}
	last := past[0]
	line := fmt.Sprintf("past incidents on this node (30d): %d — last %s: %s", len(past),
		last.Started.UTC().Format("2006-01-02"), clip(last.Title, 80))
	if last.RootCause != "" {
		line += " (cause: " + clip(last.RootCause, 80) + ")"
	}
	var repeats []string
	seen := map[string]int{}
	for _, p := range past {
		if p.Case != "" && hits[p.Case] {
			seen[p.Case]++
		}
	}
	for id, n := range seen {
		repeats = append(repeats, fmt.Sprintf("%s ×%d", id, n))
	}
	if len(repeats) > 0 {
		line += "\nrecurring: " + strings.Join(repeats, ", ") + " — check what the last fix missed (incident.show)"
	}
	return line
}

// --- node.history ---------------------------------------------------------------

// History is node.history: how signals moved over time.
type History struct{}

func (History) Name() string { return "node.history" }
func (History) Desc() string {
	return "How node signals changed over time, from recorded triage sweeps: first→last, min/max, when it changed, a trend line. " +
		"Answers 'since when?' and 'is it getting worse?' (e.g. signals=val.missed,node.peers since=24h). Records come from every " +
		"node.triage run and from mon.record."
}
func (History) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"signals": toolkit.Str("comma-separated signal names or prefixes (val., host.disk_used_pct) — default: the key health signals"),
		"since":   toolkit.Str("window, e.g. 6h, 24h, 7d (default 24h)"),
	})
}
func (History) Tier() toolkit.Tier { return toolkit.TierObserve }

var defaultHistory = []string{"node.peers", "node.catching_up", "node.reachable", "val.jailed", "val.missed",
	"val.signed_recent_pct", "host.disk_used_pct", "host.mem_used_pct", "proc.restarts", "proc.running"}

func (History) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	if c.Profile == nil {
		return nil, fmt.Errorf("no node profile — use_node first")
	}
	window, err := parseWindow(a.String("since", "24h"))
	if err != nil {
		return nil, err
	}
	recs, err := history.Load(c.Profile.Name, time.Now().Add(-window))
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return &toolkit.Result{Text: fmt.Sprintf("no history for %s in the last %s — run node.triage (each run is recorded) or `cometcli mon record` for regular samples", c.Profile.Name, window)}, nil
	}
	names := defaultHistory
	if s := strings.TrimSpace(a.String("signals", "")); s != "" {
		names = expand(strings.Split(s, ","), recs)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d samples, %s → %s\n", c.Profile.Name, len(recs),
		recs[0].TS.Local().Format("Jan 2 15:04"), recs[len(recs)-1].TS.Local().Format("Jan 2 15:04"))
	data := map[string]any{}
	for _, n := range names {
		s := history.Summarize(recs, n)
		if s.Samples == 0 {
			continue
		}
		data[n] = map[string]any{"first": s.First, "last": s.Last, "changes": len(s.Changes)}
		if s.Numeric {
			fmt.Fprintf(&b, "%s: %v → %v (min %v, max %v) %s", n, fmtVal(s.First), fmtVal(s.Last), fmtVal(s.Min), fmtVal(s.Max), s.Spark)
		} else {
			fmt.Fprintf(&b, "%s: %v → %v", n, fmtVal(s.First), fmtVal(s.Last))
		}
		if k := len(s.Changes); k > 0 {
			ch := s.Changes
			if k > 4 {
				ch = ch[k-4:]
			}
			var parts []string
			for _, x := range ch {
				parts = append(parts, fmt.Sprintf("%s %v→%v", x.TS.Local().Format("Jan 2 15:04"), fmtVal(x.From), fmtVal(x.To)))
			}
			fmt.Fprintf(&b, " · changed %d× (last: %s)", k, strings.Join(parts, "; "))
		}
		b.WriteString("\n")
	}
	return &toolkit.Result{Text: strings.TrimRight(b.String(), "\n"), Data: data}, nil
}

// expand turns prefixes ("val.") into the signal names present in recs.
func expand(want []string, recs []history.Record) []string {
	have := map[string]bool{}
	for _, r := range recs {
		for k := range r.Signals {
			have[k] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, w := range want {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if have[w] && !seen[w] {
			out, seen[w] = append(out, w), true
			continue
		}
		for _, k := range sortedKeys(toAny(have)) {
			if strings.HasPrefix(k, w) && !seen[k] {
				out, seen[k] = append(out, k), true
			}
		}
	}
	return out
}

func toAny(m map[string]bool) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func parseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("since %q: want a window like 6h, 24h or 7d", s)
	}
	return d, nil
}

// --- mon.record -----------------------------------------------------------------

// Record is mon.record: a sampler that keeps the signal history fresh.
type Record struct{}

func (Record) Name() string { return "mon.record" }
func (Record) Desc() string {
	return "Record a triage sample every interval into the signal history (for node.history trends); runs until stopped"
}
func (Record) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"interval": toolkit.Str("time between samples, e.g. 5m (default 5m, minimum 30s)"),
	})
}
func (Record) Tier() toolkit.Tier { return toolkit.TierObserve }
func (Record) LongRunning() bool  { return true }

func (Record) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	if c.Profile == nil {
		return nil, fmt.Errorf("no node profile")
	}
	iv, err := time.ParseDuration(a.String("interval", "5m"))
	if err != nil || iv < 30*time.Second {
		return nil, fmt.Errorf("interval: want a duration ≥ 30s, e.g. 5m")
	}
	n := 0
	for {
		r := Collect(c, iv)
		if err := history.Append(c.Profile.Name, time.Now(), r.Signals); err != nil {
			return nil, err
		}
		n++
		if c.Out != nil {
			fmt.Fprintf(c.Out, "%s recorded sample %d for %s (%d signals)\n", time.Now().Format("15:04:05"), n, c.Profile.Name, len(r.Signals))
		}
		select {
		case <-c.Done():
			return &toolkit.Result{Text: fmt.Sprintf("recorded %d samples", n)}, nil
		case <-time.After(iv):
		}
	}
}

// --- incidents ------------------------------------------------------------------

// RecordIncident is incident.record.
type RecordIncident struct{}

func (RecordIncident) Name() string { return "incident.record" }
func (RecordIncident) Desc() string {
	return "Save a finished incident to this node's record (a JSON entry and a Markdown postmortem): title, root cause, case id, " +
		"evidence, actions, outcome. The timeline and tx hashes are filled in from the audit log. Call it at the end of every incident."
}
func (RecordIncident) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"title":      toolkit.Str("one line: what went wrong"),
		"root_cause": toolkit.Str("the cause, with the decisive evidence"),
		"case":       toolkit.Str("knowledge-base case id, if one matched"),
		"evidence":   toolkit.Str("key evidence, one item per line"),
		"actions":    toolkit.Str("what was done, one item per line"),
		"outcome":    toolkit.Str("resolved | mitigated | unresolved — and the final state"),
		"started":    toolkit.Str("when the incident started (RFC3339) or how long ago (e.g. 45m); default: this session's start"),
	}, "title", "root_cause", "outcome")
}
func (RecordIncident) Tier() toolkit.Tier { return toolkit.TierDiagnose } // writes only cometcli's own records

func (RecordIncident) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	if c.Profile == nil {
		return nil, fmt.Errorf("no node profile — use_node first")
	}
	now := time.Now()
	session := ""
	if c.Audit != nil {
		session = c.Audit.Session()
	}
	started := now.Add(-time.Hour)
	if s := strings.TrimSpace(a.String("started", "")); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			started = t
		} else if d, err := parseWindow(s); err == nil {
			started = now.Add(-d)
		} else {
			return nil, fmt.Errorf("started %q: want RFC3339 or a duration like 45m", s)
		}
	} else if parts := strings.SplitN(session, "-", 3); len(parts) == 3 {
		// session ids start with their UTC start time; a long or resumed
		// session must not stretch the incident back for days
		if t, err := time.Parse("20060102-150405", parts[0]+"-"+parts[1]); err == nil {
			started = t
			if now.Sub(started) > 6*time.Hour {
				started = now.Add(-6 * time.Hour)
			}
		}
	}
	inc := &incidents.Incident{
		Profile: c.Profile.Name, Title: a.String("title", ""), Case: a.String("case", ""),
		Started: started, Resolved: now, RootCause: a.String("root_cause", ""),
		Evidence: lines(a.String("evidence", "")), Actions: lines(a.String("actions", "")),
		Outcome: a.String("outcome", ""), Session: session,
	}
	inc.Timeline, inc.TxHashes = incidents.FromAudit(c.Profile.Name, session, started.Add(-time.Minute), now)
	path, err := incidents.Save(inc)
	if err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: fmt.Sprintf("recorded incident %s → %s (%d timeline events, %d txs)", inc.ID, path, len(inc.Timeline), len(inc.TxHashes)),
		Data: map[string]any{"id": inc.ID, "path": path}}, nil
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-*• "))
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// ListIncidents is incident.list.
type ListIncidents struct{}

func (ListIncidents) Name() string { return "incident.list" }
func (ListIncidents) Desc() string {
	return "List past incidents recorded on this node (newest first): when, case, title, outcome"
}
func (ListIncidents) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"since": toolkit.Str("window, e.g. 7d, 90d (default 30d)"),
	})
}
func (ListIncidents) Tier() toolkit.Tier { return toolkit.TierObserve }

func (ListIncidents) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	if c.Profile == nil {
		return nil, fmt.Errorf("no node profile — use_node first")
	}
	window, err := parseWindow(a.String("since", "30d"))
	if err != nil {
		return nil, err
	}
	past, err := incidents.List(c.Profile.Name, time.Now().Add(-window))
	if err != nil {
		return nil, err
	}
	if len(past) == 0 {
		return &toolkit.Result{Text: "no incidents recorded for " + c.Profile.Name + " in the last " + a.String("since", "30d")}, nil
	}
	var b strings.Builder
	var ids []string
	for _, p := range past {
		fmt.Fprintf(&b, "%s  %s  %s — %s\n", p.ID, orNone(p.Case), clip(p.Title, 70), clip(p.Outcome, 40))
		ids = append(ids, p.ID)
	}
	return &toolkit.Result{Text: strings.TrimRight(b.String(), "\n"), Data: map[string]any{"incidents": ids}}, nil
}

// ShowIncident is incident.show.
type ShowIncident struct{}

func (ShowIncident) Name() string { return "incident.show" }
func (ShowIncident) Desc() string {
	return "Show a recorded incident's postmortem: root cause, evidence, actions, transactions, timeline"
}
func (ShowIncident) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{"id": toolkit.Str("incident id from incident.list or triage")}, "id")
}
func (ShowIncident) Tier() toolkit.Tier { return toolkit.TierObserve }

func (ShowIncident) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	if c.Profile == nil {
		return nil, fmt.Errorf("no node profile — use_node first")
	}
	inc, err := incidents.Get(c.Profile.Name, a.String("id", ""))
	if err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: incidents.Markdown(inc), Data: map[string]any{"id": inc.ID}}, nil
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
