// Package toolkit defines the core abstraction of cometcli: the Tool.
//
// Every capability of cometcli — whether invoked as `cometcli val status`
// or called by the agent loop — is a registered Tool with a name, a JSON
// schema describing its arguments, a safety Tier, and a Run method.
// The CLI and the agent are both thin layers over one Registry.
package toolkit

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Tier is the safety classification of a tool. It drives the approval
// engine: higher tiers require explicit human confirmation.
type Tier int

const (
	// TierObserve is a pure read: RPC queries, status, metrics.
	TierObserve Tier = iota
	// TierDiagnose is local inspection: logs, file perms, config lint.
	TierDiagnose
	// TierLocalChange mutates the host: config edits, service restarts,
	// snapshot restores. Requires confirmation.
	TierLocalChange
	// TierOnChain broadcasts a transaction. Always requires explicit
	// approval after simulation.
	TierOnChain
)

func (t Tier) String() string {
	switch t {
	case TierObserve:
		return "observe"
	case TierDiagnose:
		return "diagnose"
	case TierLocalChange:
		return "local-change"
	case TierOnChain:
		return "on-chain"
	default:
		return "unknown"
	}
}

// Args are the decoded arguments to a tool call.
type Args map[string]any

// String reads a string arg.
func (a Args) String(key, def string) string {
	if v, ok := a[key].(string); ok && v != "" {
		return v
	}
	return def
}

// Bool reads a bool arg.
func (a Args) Bool(key string, def bool) bool {
	if v, ok := a[key].(bool); ok {
		return v
	}
	return def
}

// Int reads an int arg (JSON numbers arrive as float64).
// Has reports whether the caller supplied the argument at all.
func (a Args) Has(key string) bool { _, ok := a[key]; return ok }

func (a Args) Int(key string, def int64) int64 {
	switch v := a[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		var n int64
		if _, err := fmt.Sscan(v, &n); err == nil {
			return n
		}
	}
	return def
}

// Result is the output of a tool call. Text is the human rendering;
// Data is structured output for the agent and for --json mode.
type Result struct {
	Text string
	Data map[string]any
}

// JSON returns Data (or Text) as a JSON string.
func (r *Result) JSON() string {
	v := any(r.Data)
	if r.Data == nil {
		v = map[string]any{"text": r.Text}
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// LongRunner is implemented by tools that run indefinitely by design
// (watchers, dashboards) — they are exempt from the per-tool deadline.
type LongRunner interface {
	LongRunning() bool
}

// IsLongRunning reports whether a tool should not get a deadline.
func IsLongRunning(t Tool) bool {
	lr, ok := t.(LongRunner)
	return ok && lr.LongRunning()
}

// Tool is a single capability.
type Tool interface {
	// Name is the dotted identifier, e.g. "node.status".
	Name() string
	// Desc is a one-line human description.
	Desc() string
	// Schema returns a JSON-schema object describing the args.
	Schema() map[string]any
	// Tier is the safety classification.
	Tier() Tier
	// Run executes the tool.
	Run(c *Context, args Args) (*Result, error)
}

// Schema helpers for declaring arg schemas compactly.
func ObjSchema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func Str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func Bool(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
func Int(desc string) map[string]any  { return map[string]any{"type": "integer", "description": desc} }

func Enum(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}

// Registry holds all known tools.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds a tool, panicking on duplicate names (programmer error).
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[t.Name()]; dup {
		panic("duplicate tool: " + t.Name())
	}
	r.tools[t.Name()] = t
}

// Get fetches a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// All returns tools sorted by name.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// FuncSchemas returns OpenAI/Anthropic-style function schemas for the agent.
func (r *Registry) FuncSchemas() []map[string]any {
	var out []map[string]any
	for _, t := range r.All() {
		out = append(out, map[string]any{
			"name":        strings.ReplaceAll(t.Name(), ".", "__"),
			"description": fmt.Sprintf("[%s] %s", t.Tier(), t.Desc()),
			"parameters":  t.Schema(),
		})
	}
	return out
}

// ResolveName maps an agent-side function name back to a tool name.
func ResolveName(fnName string) string { return strings.ReplaceAll(fnName, "__", ".") }

// Approver asks the human for confirmation. It must return true to proceed.
type Approver func(c *Context, prompt string, tier Tier, detail map[string]any) (bool, error)

// RequireApproval invokes the approver and returns an error if denied.
func RequireApproval(c *Context, prompt string, tier Tier, detail map[string]any) error {
	if c.Approver == nil {
		return fmt.Errorf("approval required but no approver configured: %s", prompt)
	}
	ok, err := c.Approver(c, prompt, tier, detail)
	if err != nil {
		return err
	}
	if c.Audit != nil {
		name := ""
		if c.Profile != nil {
			name = c.Profile.Name
		}
		c.Audit.Approval(name, prompt, ok)
	}
	if !ok {
		return fmt.Errorf("denied: %s", prompt)
	}
	return nil
}
