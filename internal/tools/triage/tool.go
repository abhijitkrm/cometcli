package triage

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Register adds node.triage and the kb.* tools.
func Register(r *toolkit.Registry) {
	r.Register(Triage{})
	r.Register(Search{})
	r.Register(Show{})
	r.Register(Add{})
}

// LoadKB loads the built-in cases plus the user's and the project's.
func LoadKB(c *toolkit.Context) *kb.Base {
	user := ""
	if d, err := config.Dir(); err == nil {
		user = filepath.Join(d, "kb")
	}
	return kb.Load(user, c.WorkRoot)
}

// Triage is node.triage.
type Triage struct{}

func (Triage) Name() string { return "node.triage" }
func (Triage) Desc() string {
	return "FIRST STEP for any node/validator problem or health question: one sweep collecting ~50 signals (sync, peers, " +
		"jail/signing, key match, upgrade plan, gov, disk/mem/clock, process restarts/OOM, log error categories, config " +
		"invariants, EVM) and matching them to known cases in the knowledge base. Then kb.show the top case for its procedure."
}
func (Triage) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"since":   map[string]any{"type": "string", "description": "log window, e.g. 30m, 2h (default 30m)"},
		"signals": map[string]any{"type": "string", "description": "only show signals with this prefix (e.g. val., logs.)"},
	}}
}
func (Triage) Tier() toolkit.Tier                 { return toolkit.TierDiagnose }
func (Triage) Timeout(toolkit.Args) time.Duration { return 2 * time.Minute }

func (Triage) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	since := 30 * time.Minute
	if s := a.String("since", ""); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("since %q: want a duration like 30m or 2h", s)
		}
		since = d
	}
	r := Collect(c, since)
	base := LoadKB(c)
	hits := base.Match(r.Signals, r.Chain)
	return &toolkit.Result{Text: Render(r, hits, a.String("signals", "")), Data: map[string]any{
		"chain": r.Chain, "signals": r.Signals, "sources": r.Sources, "cases": hitIDs(hits),
	}}, nil
}

func hitIDs(hits []kb.Hit) []string {
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.Case.ID)
	}
	return ids
}

// Render is the compact report the model reads (kept small: free-tier
// providers cap tokens per minute).
func Render(r *Report, hits []kb.Hit, prefix string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "triage (%s, logs last %s)\nsources:", r.Chain, r.Since)
	var names []string
	for n := range r.Sources {
		names = append(names, n)
	}
	sort.Strings(names)
	var down []string
	for _, n := range names {
		if r.Sources[n] == "ok" {
			fmt.Fprintf(&b, " %s ✓", n)
		} else {
			fmt.Fprintf(&b, " %s ✗", n)
			down = append(down, fmt.Sprintf("  %s: %s", n, clip(r.Sources[n], 140)))
		}
	}
	b.WriteString("\n")
	for _, d := range down {
		b.WriteString(d + "\n")
	}
	// group signals by prefix on one line each; zero log counts are noise
	groups := map[string][]string{}
	var order []string
	for _, k := range sortedKeys(r.Signals) {
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		v := r.Signals[k]
		if strings.HasPrefix(k, "logs.") && v == float64(0) && k != "logs.lines" {
			continue
		}
		if strings.HasSuffix(k, "cons_addr_hex") || k == "val.cons_addr" || v == "" { // matching-only / empty
			continue
		}
		g, name, _ := strings.Cut(k, ".")
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], name+"="+fmtVal(v))
	}
	b.WriteString("signals:\n")
	for _, g := range order {
		fmt.Fprintf(&b, "  %s: %s\n", g, strings.Join(groups[g], " "))
	}
	if len(r.Samples) > 0 && prefix == "" {
		b.WriteString("log samples:\n")
		var cats []string
		for k := range r.Samples {
			if k != "peer_net" && k != "p2p_auth" { // usually routine churn
				cats = append(cats, k)
			}
		}
		sort.Strings(cats)
		for _, k := range cats {
			fmt.Fprintf(&b, "  %s: %s\n", k, clip(r.Samples[k][0], 200))
		}
	}
	var incidents []kb.Hit
	var advisories, symptoms []string
	for _, h := range hits {
		switch {
		case h.Case.Kind == "advisory":
			advisories = append(advisories, h.Case.ID)
		case h.SymptomOf != "":
			symptoms = append(symptoms, h.Case.ID+" ← "+h.SymptomOf)
		default:
			incidents = append(incidents, h)
		}
	}
	if len(incidents) == 0 && len(symptoms) == 0 {
		b.WriteString("matched cases: none — no known failure pattern. If something is still wrong, investigate from the signals, then record what you learn with kb.add.")
		if len(advisories) > 0 {
			fmt.Fprintf(&b, "\nadvisories (hardening, not incidents): %s", strings.Join(advisories, ", "))
		}
		return b.String()
	}
	b.WriteString("matched cases (root causes, most severe first):\n")
	for i, h := range incidents {
		if i == 6 {
			fmt.Fprintf(&b, "  … %d more\n", len(incidents)-i)
			break
		}
		fmt.Fprintf(&b, "  %d. %s [%s] %s — %s\n", i+1, h.Case.ID, h.Case.Severity, h.Case.Title, strings.Join(h.Matched, ", "))
	}
	if len(symptoms) > 0 {
		fmt.Fprintf(&b, "symptoms of the above (clear once the root is fixed): %s\n", strings.Join(symptoms, ", "))
	}
	if len(advisories) > 0 {
		fmt.Fprintf(&b, "advisories (hardening, not incidents): %s\n", strings.Join(advisories, ", "))
	}
	b.WriteString("→ kb.show <id> for confirm steps, fix and verify; confirm before acting.")
	return b.String()
}

func fmtVal(v any) string {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%.1f", x)
	case string:
		if x == "" {
			return `""`
		}
		if strings.ContainsAny(x, " =") {
			return fmt.Sprintf("%q", x)
		}
		return x
	}
	return fmt.Sprint(v)
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// --- kb tools -------------------------------------------------------------------

// Search is kb.search.
type Search struct{}

func (Search) Name() string { return "kb.search" }
func (Search) Desc() string {
	return "Search the knowledge base of known validator/node failure cases by keywords (symptom, error text, module). Returns case ids and titles."
}
func (Search) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"query": map[string]any{"type": "string", "description": "keywords, e.g. 'app hash mismatch' or 'evm rpc not responding'"},
	}, "required": []string{"query"}}
}
func (Search) Tier() toolkit.Tier { return toolkit.TierObserve }
func (Search) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	base := LoadKB(c)
	cs := base.Search(a.String("query", ""), ChainType(c), 8)
	if len(cs) == 0 {
		return &toolkit.Result{Text: "no matching cases"}, nil
	}
	var b strings.Builder
	var ids []string
	for _, x := range cs {
		fmt.Fprintf(&b, "%s [%s] %s\n", x.ID, x.Severity, x.Title)
		ids = append(ids, x.ID)
	}
	return &toolkit.Result{Text: strings.TrimSpace(b.String()), Data: map[string]any{"cases": ids}}, nil
}

// Show is kb.show.
type Show struct{}

func (Show) Name() string { return "kb.show" }
func (Show) Desc() string {
	return "Show a knowledge-base case: how to confirm it, causes, fix steps (tagged [read]/[change]/[tx]), how to verify, and what never to do."
}
func (Show) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"id": map[string]any{"type": "string", "description": "case id from node.triage or kb.search"},
	}, "required": []string{"id"}}
}
func (Show) Tier() toolkit.Tier { return toolkit.TierObserve }
func (Show) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	id := a.String("id", "")
	x, ok := LoadKB(c).Cases[id]
	if !ok {
		return nil, fmt.Errorf("no case %q — use kb.search", id)
	}
	return &toolkit.Result{Text: x.Render(), Data: map[string]any{"id": x.ID, "source": x.Source}}, nil
}

// Add is kb.add: record a newly learned case.
type Add struct{}

func (Add) Name() string { return "kb.add" }
func (Add) Desc() string {
	return "Record a new failure case you diagnosed (or improve one, same id) so future triage recognizes it. yaml fields: " +
		"id, title, chains [cosmos-sdk|cosmos-evm], severity (critical|high|medium|low), symptoms, match {all:[…], any:[…]} " +
		"over node.triage signal names (e.g. 'logs.apphash > 0', 'val.jailed == true'), confirm, causes, fix (each step prefixed " +
		"[read] [change] or [tx]), verify, warnings, test {signals: {…}} that must match. Saved to the user KB (or project with scope=project)."
}
func (Add) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"yaml":  map[string]any{"type": "string", "description": "the case as YAML"},
		"scope": map[string]any{"type": "string", "enum": []string{"user", "project"}, "description": "where to save (default user)"},
	}, "required": []string{"yaml"}}
}
func (Add) Tier() toolkit.Tier { return toolkit.TierLocalChange }
func (Add) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	raw := []byte(a.String("yaml", ""))
	x, err := kb.Validate(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid case: %w", err)
	}
	if !kb.ValidID(x.ID) {
		return nil, fmt.Errorf("id %q: use lowercase letters, digits and dashes", x.ID)
	}
	var dir string
	switch a.String("scope", "user") {
	case "project":
		if c.WorkRoot == "" {
			return nil, fmt.Errorf("no project root")
		}
		dir = filepath.Join(c.WorkRoot, ".cometcli", "kb")
	default:
		d, err := config.Dir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(d, "kb")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, x.ID+".yaml")
	_, existed := os.Stat(p)
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		return nil, err
	}
	verb := "added"
	if existed == nil {
		verb = "updated"
	}
	return &toolkit.Result{Text: fmt.Sprintf("%s case %s → %s", verb, x.ID, p), Data: map[string]any{"id": x.ID, "path": p}}, nil
}
