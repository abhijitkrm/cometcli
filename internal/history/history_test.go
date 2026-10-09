package history

import (
	"strings"
	"testing"
	"time"
)

func TestAppendLoadSummarizeDeltas(t *testing.T) {
	t.Setenv("COMETCLI_HOME", t.TempDir())
	base := time.Now().Add(-3 * time.Hour)
	peers := []float64{8, 8, 7, 1, 0, 0}
	for i, p := range peers {
		jailed := i >= 4
		if err := Append("val01", base.Add(time.Duration(i)*30*time.Minute), map[string]any{
			"node.peers": p, "val.jailed": jailed, "node.height": float64(1000 + i*100),
			"val.cons_addr_hex": "ABCDEF", "logs.last_error": "a long log line that must not be stored",
		}); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := Load("val01", base.Add(-time.Minute))
	if err != nil || len(recs) != len(peers) {
		t.Fatalf("loaded %d records: %v", len(recs), err)
	}
	if _, ok := recs[0].Signals["logs.last_error"]; ok {
		t.Fatal("log line stored")
	}
	s := Summarize(recs, "node.peers")
	if s.First != 8.0 || s.Last != 0.0 || s.Min != 0 || s.Max != 8 || len([]rune(s.Spark)) != len(peers) {
		t.Fatalf("summary %+v", s)
	}
	if len(s.Changes) == 0 || s.Changes[0].From != 7.0 || s.Changes[0].To != 1.0 {
		t.Fatalf("big drop not marked: %+v", s.Changes)
	}
	j := Summarize(recs, "val.jailed")
	if len(j.Changes) != 1 || j.Changes[0].To != true {
		t.Fatalf("jail change: %+v", j.Changes)
	}
	d := Deltas(recs[2].Signals, recs[4].Signals)
	got := strings.Join(d, "; ")
	if !strings.Contains(got, "node.peers 7→0") || !strings.Contains(got, "val.jailed false→true") || strings.Contains(got, "height") {
		t.Fatalf("deltas: %s", got)
	}
	if last := Last("val01", time.Now()); last == nil || last.Signals["node.peers"] != 0.0 {
		t.Fatalf("last: %+v", last)
	}
	if err := Append("../escape", time.Now(), nil); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestPrune(t *testing.T) {
	t.Setenv("COMETCLI_HOME", t.TempDir())
	old := time.Now().Add(-40 * 24 * time.Hour)
	_ = Append("v", old, map[string]any{"x": 1.0})
	_ = Append("v", time.Now(), map[string]any{"x": 2.0}) // first write of a new day prunes
	recs, _ := Load("v", old.Add(-time.Hour))
	if len(recs) != 1 {
		t.Fatalf("old day not pruned: %d records", len(recs))
	}
}
