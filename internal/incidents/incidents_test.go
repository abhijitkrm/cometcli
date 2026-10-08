package incidents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/audit"
)

func TestSaveListGetAndAuditTimeline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMETCLI_HOME", home)
	now := time.Now().UTC()
	// an audit trail: one session for val01, noise from another session and profile
	ad := filepath.Join(home, "audit")
	_ = os.MkdirAll(ad, 0o700)
	var lines []string
	add := func(e audit.Event) { b, _ := json.Marshal(e); lines = append(lines, string(b)) }
	add(audit.Event{TS: now.Add(-20 * time.Minute), Kind: audit.KindPrompt, Profile: "val01", Session: "s1", Detail: map[string]any{"text": "is anyone jailed?"}})
	add(audit.Event{TS: now.Add(-19 * time.Minute), Kind: audit.KindTool, Profile: "val01", Session: "s1", Detail: map[string]any{"name": "node.triage"}})
	add(audit.Event{TS: now.Add(-10 * time.Minute), Kind: audit.KindApproval, Profile: "val01", Session: "s1", Detail: map[string]any{"prompt": "broadcast transaction\n{…}", "granted": true}})
	add(audit.Event{TS: now.Add(-9 * time.Minute), Kind: audit.KindTx, Profile: "val01", Session: "s1", Detail: map[string]any{"stage": "broadcast", "hash": "ABC123", "code": 0}})
	add(audit.Event{TS: now.Add(-9 * time.Minute), Kind: audit.KindTool, Profile: "val02", Session: "s1", Detail: map[string]any{"name": "other profile"}})
	add(audit.Event{TS: now.Add(-8 * time.Minute), Kind: audit.KindTool, Profile: "val01", Session: "s2", Detail: map[string]any{"name": "other session"}})
	_ = os.WriteFile(filepath.Join(ad, now.Format("2006-01-02")+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)

	tl, hashes := FromAudit("val01", "s1", now.Add(-time.Hour), now)
	joined := strings.Join(tl, "\n")
	if len(tl) != 4 || !strings.Contains(joined, "operator: is anyone jailed?") || !strings.Contains(joined, "approved: broadcast transaction") ||
		strings.Contains(joined, "other") {
		t.Fatalf("timeline:\n%s", joined)
	}
	if len(hashes) != 1 || !strings.HasPrefix(hashes[0], "ABC123") {
		t.Fatalf("hashes %v", hashes)
	}

	inc := &Incident{Profile: "val01", Title: "Jailed for downtime", Case: "val-jailed-downtime",
		Started: now.Add(-20 * time.Minute), Resolved: now, RootCause: "container stopped", Outcome: "resolved",
		Actions: []string{"restarted", "unjailed"}, Timeline: tl, TxHashes: hashes}
	md, err := Save(inc)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(md)
	for _, want := range []string{"# Jailed for downtime", "`val-jailed-downtime`", "## Root cause", "container stopped", "`ABC123 (code 0)`", "## Timeline"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("postmortem lacks %q:\n%s", want, raw)
		}
	}
	list, _ := List("val01", now.Add(-24*time.Hour))
	if len(list) != 1 || list[0].ID != inc.ID {
		t.Fatalf("list %+v", list)
	}
	got, err := Get("val01", inc.ID)
	if err != nil || got.RootCause != "container stopped" {
		t.Fatalf("get %+v %v", got, err)
	}
	if _, err := Get("val01", "../x"); err == nil {
		t.Fatal("path traversal accepted")
	}
}
