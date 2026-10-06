package host

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLocalExecCombinedOutputAndCodes(t *testing.T) {
	h := &Local{}
	res, err := h.Exec(context.Background(), "echo out; echo err >&2; exit 3", 1000)
	if err != nil || res.Code != 3 || !strings.Contains(res.Output, "out") || !strings.Contains(res.Output, "err") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// stdin is closed: a prompt gets EOF instead of hanging
	res, _ = h.Exec(context.Background(), "read x; echo got=[$x] rc=$?", 1000)
	if !strings.Contains(res.Output, "got=[]") {
		t.Fatalf("read from stdin: %q", res.Output)
	}
	// non-interactive env
	res, _ = h.Exec(context.Background(), "echo $GIT_TERMINAL_PROMPT $PAGER", 1000)
	if strings.TrimSpace(res.Output) != "0 cat" {
		t.Fatalf("env: %q", res.Output)
	}
}

func TestLocalExecCapsOutputKeepingHeadAndTail(t *testing.T) {
	res, err := (&Local{}).Exec(context.Background(), "echo HEAD; head -c 100000 /dev/zero | tr '\\0' x; echo; echo TAIL", 2000)
	if err != nil || !res.Truncated || len(res.Output) > 2200 || !strings.HasPrefix(res.Output, "HEAD") || !strings.Contains(res.Output, "TAIL") {
		t.Fatalf("truncated=%v len=%d", res.Truncated, len(res.Output))
	}
}

func TestLocalExecTimeoutKillsChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := (&Local{}).Exec(ctx, "sleep 30 & sleep 30; wait", 1000)
	if err != nil || !res.TimedOut || time.Since(start) > 5*time.Second {
		t.Fatalf("res=%+v err=%v took %s", res, err, time.Since(start))
	}
}

func TestLocalFiles(t *testing.T) {
	dir := t.TempDir()
	h := &Local{}
	p := dir + "/f.toml"
	if err := h.WriteFile(context.Background(), p, []byte("a=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := h.ReadFile(context.Background(), p)
	fi, _ := h.Stat(context.Background(), p)
	if err != nil || string(b) != "a=1" || fi.Mode().Perm() != 0o600 {
		t.Fatalf("read=%q err=%v mode=%v", b, err, fi.Mode().Perm())
	}
}
