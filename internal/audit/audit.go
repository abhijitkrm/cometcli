// Package audit provides an append-only JSONL audit log of everything
// cometcli does: prompts, tool calls, shell commands, transactions and
// approvals. The log is compliance-grade and lives in ~/.cometcli/audit/.
package audit

import (
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
)

// Event is one audit record.
type Event struct {
	TS      time.Time      `json:"ts"`
	Kind    Kind           `json:"kind"`
	Profile string         `json:"profile,omitempty"`
	Detail  map[string]any `json:"detail"`
}

// Logger appends events to a daily JSONL file.
type Logger struct {
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
	return &Logger{file: f, path: path, enc: json.NewEncoder(f)}, nil
}

// Log records one event. Failures are intentionally non-fatal but reported.
func (l *Logger) Log(kind Kind, profile string, detail map[string]any) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enc.Encode(Event{TS: time.Now().UTC(), Kind: kind, Profile: profile, Detail: detail})
}

// Path returns the current audit file path.
func (l *Logger) Path() string { return l.path }

// Close flushes and closes the file.
func (l *Logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}

// Tool logs a tool invocation.
func (l *Logger) Tool(profile, name string, tier string, args, result map[string]any, err error) {
	d := map[string]any{"name": name, "tier": tier, "args": args}
	if err != nil {
		d["error"] = err.Error()
	} else {
		d["result"] = result
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
