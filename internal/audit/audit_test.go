package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentSessionsKeepJSONLIntact(t *testing.T) {
	t.Setenv("COMETCLI_HOME", t.TempDir())
	lg, err := Open("val01")
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	var wg sync.WaitGroup
	for s := 0; s < 8; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			l := lg.WithSession(fmt.Sprintf("sess-%d", s))
			_ = l.Log(KindPrompt, "val01", map[string]any{"text": fmt.Sprintf("prompt %d", s)})
			for i := 0; i < 50; i++ {
				l.ToolSeen("val01", "node.status", "observe", map[string]any{"i": i}, map[string]any{"ok": true}, nil, "height 42")
			}
			l.Approval("val01", "broadcast tx", s%2 == 0)
			l.Tool("val01", "val.unjail", "on-chain", nil, nil, errors.New("denied"))
		}(s)
	}
	wg.Wait()

	f, _ := os.Open(lg.Path())
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %d is not valid JSON (interleaved write?): %q", n+1, sc.Text())
		}
		n++
	}
	if n != 8*(1+50+2) {
		t.Fatalf("got %d events, want %d", n, 8*53)
	}
	fi, _ := os.Stat(lg.Path())
	di, _ := os.Stat(filepath.Dir(lg.Path()))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("permissions: file %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
	}

	evs, err := ReadSession("sess-3")
	if err != nil || len(evs) != 53 || evs[0].Kind != KindPrompt || evs[52].Detail["error"] != "denied" {
		t.Fatalf("ReadSession: %d events err=%v", len(evs), err)
	}
	if evs[1].Detail["digest"] == nil || evs[1].Detail["seen"] != "height 42" {
		t.Fatalf("tool event = %v", evs[1].Detail)
	}
	ss, err := Sessions(lg.Path())
	if err != nil || len(ss) != 8 {
		t.Fatalf("Sessions = %v %v", ss, err)
	}
	for _, s := range ss {
		if s[1] == "" {
			t.Fatalf("session %s has no first prompt", s[0])
		}
	}
}

func TestReadSessionAcrossDays(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMETCLI_HOME", home)
	dir := filepath.Join(home, "audit")
	os.MkdirAll(dir, 0o700)
	line := func(kind, text string) string {
		b, _ := json.Marshal(Event{Kind: Kind(kind), Session: "s1", Detail: map[string]any{"text": text}})
		return string(b) + "\n"
	}
	os.WriteFile(filepath.Join(dir, "2026-10-05.jsonl"), []byte(line("prompt", "before midnight")+"not json\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "2026-10-06.jsonl"), []byte(line("llm", "after midnight")), 0o600)
	evs, err := ReadSession("s1")
	if err != nil || len(evs) != 2 || evs[0].Detail["text"] != "before midnight" || evs[1].Detail["text"] != "after midnight" {
		t.Fatalf("events = %+v err=%v", evs, err)
	}
	files, _ := List()
	if len(files) != 2 || filepath.Base(files[0]) != "2026-10-06.jsonl" {
		t.Fatalf("List = %v", files)
	}
}

func TestNilLoggerIsNoop(t *testing.T) {
	var l *Logger
	if l.Log(KindTool, "", nil) != nil || l.Path() != "" || l.Session() != "" || l.Close() != nil || l.WithSession("x") != nil {
		t.Fatal("nil logger must be a no-op")
	}
	l.Shell("p", "ls", 0)
	l.Tx("p", "simulate", nil)
}
