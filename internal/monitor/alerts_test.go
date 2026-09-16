package monitor

import (
	"testing"
	"time"
)

func evalAll(rules []Rule, s, prev *Snapshot) map[string]string {
	out := map[string]string{}
	for _, r := range rules {
		if m := r.Eval(s, prev); m != "" {
			out[r.Name] = m
		}
	}
	return out
}

func TestRules(t *testing.T) {
	rules := DefaultRules(50, 85, 120)

	// healthy
	ok := &Snapshot{Reachable: true, Height: 100, Peers: 10, Window: 1000, Missed: 5, DiskUsedPct: 40}
	if got := evalAll(rules, ok, nil); len(got) != 0 {
		t.Fatalf("healthy snapshot triggered: %v", got)
	}

	// jailed
	j := &Snapshot{Reachable: true, Height: 100, Peers: 10, Jailed: true, Window: 1000}
	if got := evalAll(rules, j, nil); got["jailed"] == "" {
		t.Fatal("jailed not detected")
	}

	// missed threshold
	m := &Snapshot{Reachable: true, Height: 100, Peers: 10, Window: 1000, Missed: 60}
	if got := evalAll(rules, m, nil); got["missed-blocks"] == "" {
		t.Fatal("missed threshold not detected")
	}

	// height stall
	now := time.Now()
	p := &Snapshot{TS: now.Add(-5 * time.Minute), Reachable: true, Height: 100}
	s := &Snapshot{TS: now, Reachable: true, Height: 100}
	if got := evalAll(rules, s, p); got["height-stall"] == "" {
		t.Fatal("stall not detected")
	}

	// unreachable
	u := &Snapshot{Reachable: false}
	if got := evalAll(rules, u, nil); got["unreachable"] == "" {
		t.Fatal("unreachable not detected")
	}
}
