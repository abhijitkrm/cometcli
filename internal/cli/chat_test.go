package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/agent"
)

func TestReadPipedStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		w.WriteString("  panic: boom\n")
		w.Close()
	}()
	got, err := readPipedStdin(r)
	if err != nil || got != "panic: boom" {
		t.Fatalf("pipe = %q %v", got, err)
	}
	null, _ := os.Open(os.DevNull)
	defer null.Close()
	if got, _ := readPipedStdin(null); got != "" {
		t.Fatalf("/dev/null read %q", got)
	}
	big, _ := os.CreateTemp(t.TempDir(), "in")
	big.WriteString(strings.Repeat("x", maxStdin) + "TAIL")
	big.Seek(0, 0)
	got, _ = readPipedStdin(big)
	if len(got) > maxStdin+100 || !strings.HasSuffix(got, "TAIL") || !strings.Contains(got, "omitted") {
		t.Fatalf("large input: len=%d", len(got))
	}
}

func TestStreamEvent(t *testing.T) {
	if streamEvent(agent.Event{Kind: agent.EvDelta, Text: "x"}, false) != nil {
		t.Fatal("delta emitted without --include-partial-messages")
	}
	m := streamEvent(agent.Event{Kind: agent.EvToolResult, Tool: "bash", Err: "denied"}, false)
	if m["type"] != "tool_result" || m["is_error"] != true || m["output"] != "denied" {
		t.Fatalf("tool_result = %v", m)
	}
	if m := streamEvent(agent.Event{Kind: agent.EvText, Text: "hi"}, false); m["type"] != "assistant" {
		t.Fatalf("assistant = %v", m)
	}
}
