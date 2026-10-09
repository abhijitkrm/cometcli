package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRouteStats(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMETCLI_HOME", home)
	_ = os.MkdirAll(filepath.Join(home, "audit"), 0o700)
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339Nano)
	lines := []string{
		`{"ts":"` + ts + `","kind":"prompt","detail":{"text":"is anyone jailed?","route":"local","intent":"jailed-validators"}}`,
		`{"ts":"` + ts + `","kind":"prompt","detail":{"text":"restart val3","route":"local","intent":"service"}}`,
		`{"ts":"` + ts + `","kind":"prompt","detail":{"text":"/incident x","route":"local","case":"node-process-down"}}`,
		`{"ts":"` + ts + `","kind":"prompt","detail":{"text":"Why is val2 slow?","route":"model"}}`,
		`{"ts":"` + ts + `","kind":"prompt","detail":{"text":"why is val2 slow","route":"model"}}`,
		`{"ts":"` + ts + `","kind":"tool","detail":{"name":"node.status"}}`,
	}
	_ = os.WriteFile(filepath.Join(home, "audit", now.Format("2006-01-02")+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	s, err := RouteStats(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.Local != 3 || s.Model != 2 || s.ByIntent["incident node-process-down"] != 1 || s.ModelPrompts["why is val2 slow"] != 2 {
		t.Fatalf("stats = %+v", s)
	}
}
