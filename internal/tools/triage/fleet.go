package triage

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// FleetTriage is fleet.triage: triage every profile at once and compare
// them — what a single node's view can't show.
type FleetTriage struct{}

func (FleetTriage) Name() string { return "fleet.triage" }
func (FleetTriage) Desc() string {
	return "Triage every node at once and compare them: one row per node (sync lag, peers, signing, jail, top case) plus " +
		"fleet findings — a node behind the others, a chain-wide halt, version mismatches, two nodes on one consensus key, " +
		"validators sharing a host, sentries down. Use for 'how is my network/fleet?' or before picking a node to work on."
}
func (FleetTriage) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"profiles": toolkit.Str("comma-separated profiles (default: all)"),
		"chain":    toolkit.Str("only profiles on this chain id"),
	})
}
func (FleetTriage) Tier() toolkit.Tier                 { return toolkit.TierDiagnose }
func (FleetTriage) FleetWide() bool                    { return true }
func (FleetTriage) Timeout(toolkit.Args) time.Duration { return 4 * time.Minute }

// NodeReport is one node's part of a fleet sweep.
type NodeReport struct {
	Profile *config.Profile
	Report  *Report
	Hits    []kb.Hit
}

// FleetConcurrency bounds parallel node sweeps.
var FleetConcurrency = 4

// CollectFleet triages profiles in parallel, recording each node's history.
func CollectFleet(c *toolkit.Context, profiles []*config.Profile, since time.Duration) []NodeReport {
	out := make([]NodeReport, len(profiles))
	sem := make(chan struct{}, FleetConcurrency)
	var wg sync.WaitGroup
	for i, p := range profiles {
		wg.Add(1)
		go func(i int, p *config.Profile) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pc := &toolkit.Context{Context: c.Context, Profile: p, Cfg: c.Cfg, Audit: c.Audit, WorkRoot: c.WorkRoot}
			defer pc.Close()
			r := Collect(pc, since)
			remember(pc, r)
			out[i] = NodeReport{Profile: p, Report: r, Hits: LoadKB(pc).Match(r.Signals, r.Chain)}
		}(i, p)
	}
	wg.Wait()
	return out
}

func (FleetTriage) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	if c.Cfg == nil || len(c.Cfg.Profiles) == 0 {
		return nil, fmt.Errorf("no profiles configured — cometcli init")
	}
	want := map[string]bool{}
	for _, n := range strings.Split(a.String("profiles", ""), ",") {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	chain := a.String("chain", "")
	var profiles []*config.Profile
	var names []string
	for n := range c.Cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := c.Cfg.Profiles[n]
		if (len(want) > 0 && !want[n]) || (chain != "" && p.ChainID != chain) {
			continue
		}
		p.Name = n
		profiles = append(profiles, p)
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no profiles match")
	}
	reports := CollectFleet(c, profiles, 15*time.Minute)
	findings := FleetFindings(reports)
	return &toolkit.Result{Text: RenderFleet(reports, findings), Data: map[string]any{"findings": findings, "nodes": len(reports)}}, nil
}

// FleetFindings compares nodes: per-chain lag and halts, versions, shared
// consensus keys, shared hosts, sentries.
func FleetFindings(nodes []NodeReport) []string {
	var out []string
	byChain := map[string][]NodeReport{}
	byName := map[string]NodeReport{}
	for _, n := range nodes {
		byChain[n.Profile.ChainID] = append(byChain[n.Profile.ChainID], n)
		byName[n.Profile.Name] = n
	}
	f := func(n NodeReport, k string) (float64, bool) { v, ok := n.Report.Signals[k].(float64); return v, ok }
	var chains []string
	for ch := range byChain {
		chains = append(chains, ch)
	}
	sort.Strings(chains)
	for _, ch := range chains {
		group := byChain[ch]
		var top float64
		reachable, stalled := 0, 0
		versions := map[string][]string{}
		for _, n := range group {
			if n.Report.Signals["node.reachable"] != true {
				continue
			}
			reachable++
			if h, ok := f(n, "node.height"); ok && h > top {
				top = h
			}
			if age, ok := f(n, "node.block_age_s"); ok && age > 60 {
				stalled++
			}
			if v, _ := n.Report.Signals["node.version"].(string); v != "" {
				versions[v] = append(versions[v], n.Profile.Name)
			}
		}
		if reachable >= 2 && stalled == reachable {
			out = append(out, fmt.Sprintf("CHAIN HALT on %s: all %d reachable nodes are stalled near height %d — a chain-wide problem (upgrade height? consensus failure?), not one node", orNone(ch), reachable, int64(top)))
		}
		for _, n := range group {
			if n.Report.Signals["node.reachable"] != true {
				out = append(out, fmt.Sprintf("%s is unreachable", n.Profile.Name))
				continue
			}
			if h, ok := f(n, "node.height"); ok && top-h > 20 && stalled != reachable {
				out = append(out, fmt.Sprintf("%s is %d blocks behind the other %s nodes", n.Profile.Name, int64(top-h), orNone(ch)))
			}
		}
		if len(versions) > 1 {
			var parts []string
			for v, ns := range versions {
				parts = append(parts, v+": "+strings.Join(ns, ","))
			}
			sort.Strings(parts)
			out = append(out, fmt.Sprintf("version mismatch on %s — %s", orNone(ch), strings.Join(parts, "; ")))
		}
	}
	// one consensus key on two running nodes: double-sign risk
	keys := map[string][]string{}
	for _, n := range nodes {
		vp, _ := n.Report.Signals["node.voting_power"].(float64)
		if k, _ := n.Report.Signals["node.cons_addr_hex"].(string); k != "" && vp > 0 {
			keys[k] = append(keys[k], n.Profile.Name)
		}
	}
	for k, ns := range keys {
		if len(ns) > 1 {
			sort.Strings(ns)
			out = append(out, fmt.Sprintf("DOUBLE-SIGN RISK: %s run the same consensus key (%s…) — stop all but one now", strings.Join(ns, " and "), k[:min(8, len(k))]))
		}
	}
	// validators sharing one remote host
	hosts := map[string][]string{}
	for _, n := range nodes {
		p := n.Profile
		if p.IsValidator() && p.Transport.Type == "ssh" && p.Transport.Host != "" {
			hosts[p.Transport.Host] = append(hosts[p.Transport.Host], p.Name)
		}
	}
	for h, ns := range hosts {
		if len(ns) > 1 {
			sort.Strings(ns)
			out = append(out, fmt.Sprintf("validators %s share host %s — one machine failure takes them all down", strings.Join(ns, ", "), h))
		}
	}
	// sentries: a validator whose sentries are all down is cut off
	for _, n := range nodes {
		if len(n.Profile.Sentries) == 0 {
			continue
		}
		var down []string
		known := 0
		for _, s := range n.Profile.Sentries {
			sr, ok := byName[s]
			if !ok {
				continue
			}
			known++
			if sr.Report.Signals["node.reachable"] != true || sr.Report.Signals["node.peers"] == 0.0 {
				down = append(down, s)
			}
		}
		switch {
		case known > 0 && len(down) == known:
			out = append(out, fmt.Sprintf("%s: all its sentries are down (%s) — it's cut off from the network", n.Profile.Name, strings.Join(down, ", ")))
		case len(down) > 0:
			out = append(out, fmt.Sprintf("%s: sentry %s is down — one failure from being cut off", n.Profile.Name, strings.Join(down, ", ")))
		}
	}
	return out
}

// RenderFleet is the compact fleet report.
func RenderFleet(nodes []NodeReport, findings []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "fleet triage — %d nodes\n", len(nodes))
	fmt.Fprintf(&b, "%-14s %-9s %-10s %-6s %-8s %-7s %s\n", "node", "role", "height", "peers", "signing", "jailed", "top case")
	for _, n := range nodes {
		s := n.Report.Signals
		val := func(k string) string {
			v, ok := s[k]
			if !ok {
				return "-"
			}
			return fmtVal(v)
		}
		height := val("node.height")
		if s["node.reachable"] != true {
			height = "DOWN"
		} else if s["node.catching_up"] == true {
			height += "↻"
		} else if s["node.stalled"] == true {
			height += "!"
		}
		signing := val("val.signed_recent_pct")
		if signing != "-" {
			signing += "%"
		}
		top := "ok"
		for _, h := range n.Hits {
			if h.Case.Kind != "advisory" && h.SymptomOf == "" {
				top = h.Case.ID + " [" + h.Case.Severity + "]"
				break
			}
		}
		fmt.Fprintf(&b, "%-14s %-9s %-10s %-6s %-8s %-7s %s\n", clip(n.Profile.Name, 14), clip(n.Profile.Role, 9), height,
			val("node.peers"), signing, val("val.jailed"), top)
	}
	if len(findings) == 0 {
		b.WriteString("fleet findings: none — nodes agree with each other")
	} else {
		b.WriteString("fleet findings:\n")
		for _, f := range findings {
			b.WriteString("  - " + f + "\n")
		}
	}
	b.WriteString("\n→ use_node <profile> then node.triage / kb.show for a node's details")
	return strings.TrimRight(b.String(), "\n")
}
