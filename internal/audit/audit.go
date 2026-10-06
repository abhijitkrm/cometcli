// Package audit provides an append-only JSONL audit log of everything
// cometcli does: prompts, tool calls, shell commands, transactions and
// approvals. The log is compliance-grade and lives in ~/.cometcli/audit/.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// Kind enumerates audit event types.
type Kind string

const (
	KindTool     Kind = "tool"     // a tool invocation (name, args, tier, result digest)
	KindShell    Kind = "shell"    // a host-plane shell command
	KindTx       Kind = "tx"       // a transaction build/simulate/broadcast
	KindApproval Kind = "approval" // a human approval decision
	KindPrompt   Kind = "prompt"   // an agent prompt (redacted)
	KindLLM      Kind = "llm"      // an LLM response digest
	KindAlert    Kind = "alert"    // a monitor alert firing
)

// Event is one audit record.
type Event struct {
	TS      time.Time      `json:"ts"`
	Kind    Kind           `json:"kind"`
	Profile string         `json:"profile,omitempty"`
	Session string         `json:"session,omitempty"`
	Detail  map[string]any `json:"detail"`
}

// Logger appends events to a daily JSONL file. Loggers derived with
// WithSession share the underlying file and stamp a session id.
type Logger struct {
	sink    *sink
	session string
}

type sink struct {
	mu   sync.Mutex
	file *os.File
	path string
	enc  *json.Encoder
}

// Open creates/append the audit file for today.
func Open(profile string) (*Logger, error) {
	dir, err := config.Path("audit")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, time.Now().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{sink: &sink{file: f, path: path, enc: json.NewEncoder(f)}}, nil
}

// WithSession returns a logger sharing l's file that stamps every event
// with session id — the key `cometcli audit replay` groups by.
func (l *Logger) WithSession(id string) *Logger {
	if l == nil {
		return nil
	}
	return &Logger{sink: l.sink, session: id}
}

// Session returns the session id stamped on events ("" if none).
func (l *Logger) Session() string {
	if l == nil {
		return ""
	}
	return l.session
}

// Log records one event. Failures are intentionally non-fatal but reported.
func (l *Logger) Log(kind Kind, profile string, detail map[string]any) error {
	if l == nil || l.sink == nil {
		return nil
	}
	l.sink.mu.Lock()
	defer l.sink.mu.Unlock()
	return l.sink.enc.Encode(Event{TS: time.Now().UTC(), Kind: kind, Profile: profile, Session: l.session, Detail: detail})
}

// Path returns the current audit file path.
func (l *Logger) Path() string {
	if l == nil || l.sink == nil {
		return ""
	}
	return l.sink.path
}

// Close flushes and closes the file (shared by every WithSession child).
func (l *Logger) Close() error {
	if l == nil || l.sink == nil || l.sink.file == nil {
		return nil
	}
	return l.sink.file.Close()
}

// Tool logs a tool invocation.
func (l *Logger) Tool(profile, name string, tier string, args, result map[string]any, err error) {
	l.ToolSeen(profile, name, tier, args, result, err, "")
}

// ToolSeen logs a tool invocation plus the exact (redacted, truncated) text
// the model was shown — what makes an agent session replayable.
func (l *Logger) ToolSeen(profile, name string, tier string, args, result map[string]any, err error, seen string) {
	d := map[string]any{"name": name, "tier": tier, "args": args}
	if err != nil {
		d["error"] = err.Error()
	} else {
		d["result"] = result
	}
	if seen != "" {
		sum := sha256.Sum256([]byte(seen))
		d["seen"] = seen
		d["digest"] = hex.EncodeToString(sum[:8])
	}
	_ = l.Log(KindTool, profile, d)
}

// Shell logs a host command.
func (l *Logger) Shell(profile, cmd string, exitCode int) {
	_ = l.Log(KindShell, profile, map[string]any{"cmd": cmd, "exit": exitCode})
}

// Tx logs a transaction lifecycle event.
func (l *Logger) Tx(profile, stage string, detail map[string]any) {
	d := map[string]any{"stage": stage}
	for k, v := range detail {
		d[k] = v
	}
	_ = l.Log(KindTx, profile, d)
}

// Approval logs an approval decision.
func (l *Logger) Approval(profile, prompt string, granted bool) {
	_ = l.Log(KindApproval, profile, map[string]any{"prompt": prompt, "granted": granted})
}

// List returns audit file paths, newest first.
func List() ([]string, error) {
	dir, err := config.Path("audit")
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := len(ents) - 1; i >= 0; i-- {
		out = append(out, filepath.Join(dir, ents[i].Name()))
	}
	return out, nil
}

// ReadSession returns every event stamped with session id across all audit
// files, oldest first.
func ReadSession(id string) ([]Event, error) {
	files, err := List()
	if err != nil {
		return nil, err
	}
	var out []Event
	for i := len(files) - 1; i >= 0; i-- { // List is newest-first
		f, err := os.Open(files[i])
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.Session == id {
				out = append(out, e)
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Sessions lists distinct session ids found in the given audit file, in
// first-seen order, with their first prompt (for `audit sessions`).
func Sessions(path string) ([][2]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][2]string
	idx := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Session == "" {
			continue
		}
		i, ok := idx[e.Session]
		if !ok {
			i = len(out)
			idx[e.Session] = i
			out = append(out, [2]string{e.Session, ""})
		}
		if e.Kind == KindPrompt && out[i][1] == "" {
			out[i][1], _ = e.Detail["text"].(string)
		}
	}
	return out, sc.Err()
}
