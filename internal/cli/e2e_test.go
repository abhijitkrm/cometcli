package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools"
)

// fakeLLM is a scripted OpenAI-compatible chat-completions server. It
// answers by looking at the last message: user prompts are matched to a
// script entry (which may call a tool first), tool results get a final
// answer quoting the tool output.
type fakeLLM struct {
	mu       sync.Mutex
	requests []map[string]any
	script   map[string]string // prompt substring → shell command to run first
}

func (f *fakeLLM) handler(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	msgs, _ := req["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	var text string
	var call map[string]any
	switch last["role"] {
	case "tool":
		text = "Result: " + strings.SplitN(fmt.Sprint(last["content"]), "\n", 2)[0]
	default:
		prompt := fmt.Sprint(last["content"])
		text = "answer to: " + strings.SplitN(prompt, "\n", 2)[0]
		for k, cmd := range f.script {
			if strings.Contains(prompt, k) {
				args, _ := json.Marshal(map[string]any{"command": cmd})
				call = map[string]any{"index": 0, "id": "call_1", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": string(args)}}
			}
		}
	}
	w.Header().Set("content-type", "text/event-stream")
	send := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b) }
	if call != nil {
		send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{call}}}}})
		send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}},
			"usage": map[string]any{"prompt_tokens": 500, "completion_tokens": 20}})
	} else {
		send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}})
		send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 600, "completion_tokens": 10}})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (f *fakeLLM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeLLM) last() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

type env struct {
	t    *testing.T
	llm  *fakeLLM
	home string
	work string
	reg  *toolkit.Registry
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := &fakeLLM{script: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COMETCLI_HOME", filepath.Join(home, ".cometcli"))
	t.Setenv("OLLAMA_API_KEY", "x")
	os.MkdirAll(filepath.Join(home, ".cometcli"), 0o700)
	cfg := fmt.Sprintf("agent:\n  provider: openai-compat\n  base_url: %s\n  model: fake-1\nprofiles: {}\n", srv.URL)
	os.WriteFile(filepath.Join(home, ".cometcli", "config.yaml"), []byte(cfg), 0o600)
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(work)
	reg := toolkit.NewRegistry()
	tools.RegisterAll(reg)
	return &env{t: t, llm: f, home: home, work: work, reg: reg}
}

// run executes the CLI in-process and returns stdout, stderr and error.
func (e *env) run(args ...string) (string, string, error) {
	e.t.Helper()
	root := NewRoot(e.reg, []*cobra.Command{DoctorCmd(e.reg), AskCmd(e.reg), AgentCmd(e.reg), UICmd(e.reg), MCPCmd(e.reg), ServeCmd(e.reg)})
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errb.String(), err
}

func TestE2EPrintTextAndJSON(t *testing.T) {
	e := newEnv(t)
	e.llm.script["disk"] = "echo disk-ok"
	out, _, err := e.run("-p", "how is the disk?")
	if err != nil || strings.TrimSpace(out) != "Result: disk-ok" {
		t.Fatalf("text: out=%q err=%v", out, err)
	}
	// general mode: only general tools, general prompt
	req := e.llm.last()
	names := toolNamesOf(req)
	if !strings.Contains(names, "bash") || strings.Contains(names, "node__status") {
		t.Fatalf("tools = %s", names)
	}
	sys := fmt.Sprint(req["messages"].([]any)[0].(map[string]any)["content"])
	if !strings.Contains(sys, "working in the operator's terminal") {
		t.Fatalf("system prompt: %.200s", sys)
	}

	out, _, err = e.run("-p", "--output-format", "json", "hello")
	var res map[string]any
	if err != nil || json.Unmarshal([]byte(out), &res) != nil {
		t.Fatalf("json: %q %v", out, err)
	}
	if res["type"] != "result" || res["subtype"] != "success" || res["result"] != "answer to: hello" || res["mode"] != "general" {
		t.Fatalf("json result = %v", res)
	}
	if u := res["usage"].(map[string]any); u["input_tokens"].(float64) != 600 {
		t.Fatalf("usage = %v", u)
	}
}

func TestE2EStreamJSONAndHeadlessDenial(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(e.work, "made.txt")
	e.llm.script["create"] = "touch " + target
	out, _, err := e.run("-p", "--output-format", "stream-json", "create the file")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var denied bool
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(l), &ev) != nil {
			t.Fatalf("not JSON: %q", l)
		}
		types = append(types, fmt.Sprint(ev["type"]))
		if ev["type"] == "tool_result" && ev["is_error"] == true && strings.Contains(fmt.Sprint(ev["output"]), "-p has no one to ask") {
			denied = true
		}
	}
	if got := strings.Join(types, ","); got != "system,tool_use,tool_result,assistant,result" {
		t.Fatalf("event types = %s", got)
	}
	if !denied {
		t.Fatalf("headless write wasn't denied:\n%s", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("headless run created a file without approval")
	}
	// allowed explicitly → runs
	if _, _, err := e.run("-p", "--allowedTools", "bash(touch:*)", "create the file"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("--allowedTools didn't allow the command")
	}
}

func TestE2ESessionsContinueAndResume(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.run("-p", "first question"); err != nil {
		t.Fatal(err)
	}
	out, _, err := e.run("sessions")
	if err != nil || !strings.Contains(out, "first question") {
		t.Fatalf("sessions: %q %v", out, err)
	}
	id := strings.Fields(strings.Split(out, "\n")[1])[0]
	if _, _, err := e.run("-p", "-c", "second question"); err != nil {
		t.Fatal(err)
	}
	msgs := e.llm.last()["messages"].([]any)
	if len(msgs) != 4 { // system, first user, first answer, second user
		t.Fatalf("continue didn't carry the conversation: %d messages", len(msgs))
	}
	if _, stderr, err := e.run("-p", "-r", id[:10], "third"); err != nil || !strings.Contains(stderr, "resumed session") {
		t.Fatalf("resume: %q %v", stderr, err)
	}
	if _, _, err := e.run("-p", "-r", "no-such-id", "x"); err == nil {
		t.Fatal("bad resume id accepted")
	}
}

func TestE2EPipedStdinSlashCommandsConfigAndTypos(t *testing.T) {
	e := newEnv(t)
	// piped input: a real file as stdin
	in := filepath.Join(e.work, "log.txt")
	os.WriteFile(in, []byte("panic: out of memory\n"), 0o644)
	f, _ := os.Open(in)
	defer f.Close()
	root := NewRoot(e.reg, nil)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(f)
	root.SetArgs([]string{"-p", "why did it crash?"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	user := fmt.Sprint(e.llm.last()["messages"].([]any)[1].(map[string]any)["content"])
	if !strings.Contains(user, "<stdin>\npanic: out of memory\n</stdin>") {
		t.Fatalf("stdin not attached: %q", user)
	}

	// a slash command headlessly, no model call
	before := e.llm.count()
	if out, _, err := e.run("-p", "/permissions"); err != nil || !strings.Contains(out, "transactions ask") || e.llm.count() != before {
		t.Fatalf("/permissions: %q %v", out, err)
	}
	// a custom command expands into a prompt
	os.MkdirAll(filepath.Join(e.work, ".cometcli", "commands"), 0o755)
	os.WriteFile(filepath.Join(e.work, ".cometcli", "commands", "greet.md"), []byte("Say hi to $1"), 0o644)
	if out, _, err := e.run("-p", "/greet operator"); err != nil || strings.TrimSpace(out) != "answer to: Say hi to operator" {
		t.Fatalf("/greet: %q %v", out, err)
	}

	if out, _, err := e.run("config", "set", "agent.effort", "low"); err != nil || !strings.Contains(out, "agent.effort = low") {
		t.Fatalf("config set: %q %v", out, err)
	}
	if _, _, err := e.run("-p", "hi"); err != nil {
		t.Fatal(err)
	}
	if e.llm.last()["reasoning_effort"] != nil {
		// openai-compat (local servers) never gets reasoning_effort
		t.Fatalf("reasoning_effort sent to an openai-compat server")
	}
	if _, _, err := e.run("doctr"); err == nil || !strings.Contains(err.Error(), "did you mean doctor") {
		t.Fatalf("typo: %v", err)
	}
	if _, _, err := e.run("one"); err == nil || !strings.Contains(err.Error(), "init") {
		t.Fatalf("one without a profile: %v", err)
	}
}

func toolNamesOf(req map[string]any) string {
	var n []string
	tl, _ := req["tools"].([]any)
	for _, t := range tl {
		fn := t.(map[string]any)["function"].(map[string]any)
		n = append(n, fmt.Sprint(fn["name"]))
	}
	return strings.Join(n, ",")
}
