// Package agent implements the LLM agent loop: a provider-agnostic
// message/tool abstraction over Anthropic, OpenAI, and OpenAI-compatible
// endpoints, driving the shared tool registry.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// Msg is one conversation message in provider-neutral form.
type Msg struct {
	Role     string          // user | assistant | tool
	Text     string          // text content (user/assistant)
	Calls    []Call          // assistant tool-use requests
	CallID   string          // for role=tool: the call this answers
	ToolName string          // for role=tool
	IsError  bool            // tool result flagged error
	JSONArgs json.RawMessage // for role=assistant with calls
	// RawSteps carries a provider's verbatim turn output (Gemini steps with
	// thought signatures, Anthropic content blocks with thinking). It is
	// replayed unchanged, but only to the provider named in RawProvider.
	RawSteps    json.RawMessage
	RawProvider string `json:",omitempty"`
}

// Call is a tool invocation requested by the model.
type Call struct {
	ID   string
	Name string
	Args json.RawMessage
}

// ToolDef is the provider-neutral function schema.
type ToolDef struct {
	Name   string
	Desc   string
	Schema map[string]any
}

// Request is one chat round.
type Request struct {
	Model    string
	System   string
	Messages []Msg
	Tools    []ToolDef
	// MaxTok caps output tokens; 0 lets the provider pick its default.
	MaxTok int
	// Effort is the reasoning depth: low | medium | high | xhigh | max, or
	// "" for the model default. Each provider maps it to its own knob.
	Effort string
	// OnThinking, when set, receives streamed reasoning text (providers
	// that expose none never call it).
	OnThinking func(string) `json:"-"`
}

// StopReason is why a model round ended, normalized across providers.
type StopReason string

const (
	StopEnd       StopReason = "end"        // finished normally
	StopToolUse   StopReason = "tool_use"   // wants tool results
	StopMaxTokens StopReason = "max_tokens" // output cut off at MaxTok
	StopRefusal   StopReason = "refusal"    // declined by a safety filter
)

// Usage is the token accounting of one model round.
type Usage struct {
	Input     int `json:"input"`      // prompt tokens, including cached
	Output    int `json:"output"`     // generated tokens, including thinking
	CacheRead int `json:"cache_read"` // prompt tokens served from cache
}

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.Input += u2.Input
	u.Output += u2.Output
	u.CacheRead += u2.CacheRead
}

// Response is the model's reply.
type Response struct {
	Text  string
	Calls []Call
	Done  bool // true when the model finished (no tool calls pending)
	// Stop is the normalized stop reason ("" when the provider didn't say).
	Stop StopReason
	// StopDetail explains a refusal when the provider gives a category.
	StopDetail string
	// Usage is this round's token accounting (zero when not reported).
	Usage Usage
	// Thinking is visible reasoning text, when the provider returns any.
	Thinking string
	// RawSteps is the provider's raw output for this turn, replayed
	// verbatim in later requests (see Msg.RawSteps).
	RawSteps json.RawMessage
}

// Provider talks to an LLM API.
type Provider interface {
	Chat(ctx context.Context, r *Request) (*Response, error)
	Name() string
}

// Streamer is implemented by providers that can stream assistant text as
// it is generated. onText receives each text chunk; the returned Response
// is the same as Chat's (full text + tool calls).
type Streamer interface {
	Stream(ctx context.Context, r *Request, onText func(string)) (*Response, error)
}

// ErrOffline is returned when COMETCLI_OFFLINE disables the agent.
var ErrOffline = errors.New("agent disabled by COMETCLI_OFFLINE — the CLI, `cometcli mcp`, and all tool subcommands still work")

// Offline reports whether COMETCLI_OFFLINE is set to a truthy value.
func Offline() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("COMETCLI_OFFLINE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// NewProvider builds the provider from profile agent config.
// Env fallback: COMETCLI_LLM_API_KEY, then provider-specific vars.
func NewProvider(ac config.AgentConf) (Provider, error) {
	if Offline() {
		return nil, ErrOffline
	}
	key := func(env string) string {
		if ac.APIKeyEnv != "" {
			if v := os.Getenv(ac.APIKeyEnv); v != "" {
				return v
			}
			return config.Credential(ac.APIKeyEnv)
		}
		if v := os.Getenv("COMETCLI_LLM_API_KEY"); v != "" {
			return v
		}
		if v := os.Getenv(env); v != "" {
			return v
		}
		return config.Credential(env) // saved by `cometcli config set-key`
	}
	switch strings.ToLower(ac.Provider) {
	case "anthropic", "claude":
		return &anthropic{
			key:   key("ANTHROPIC_API_KEY"),
			model: def(ac.Model, "claude-opus-5-5"),
			base:  def(ac.BaseURL, "https://api.anthropic.com"),
		}, nil
	case "openai":
		return &openai{
			key:   key("OPENAI_API_KEY"),
			model: def(ac.Model, "gpt-4.1"),
			base:  def(ac.BaseURL, "https://api.openai.com"),
		}, nil
	case "groq":
		// Groq's OpenAI-compatible endpoint — https://console.groq.com
		return &openai{
			name:  "groq",
			key:   key("GROQ_API_KEY"),
			model: def(ac.Model, "llama-3.3-70b-versatile"),
			base:  def(ac.BaseURL, "https://api.groq.com/openai"),
		}, nil
	case "openrouter":
		// OpenRouter — https://openrouter.ai; openrouter/free routes each
		// request to a free model that supports what it needs (tools…)
		return &openai{
			name:  "openrouter",
			key:   key("OPENROUTER_API_KEY"),
			model: def(ac.Model, "openrouter/free"),
			base:  def(ac.BaseURL, "https://openrouter.ai/api"),
		}, nil
	case "gemini", "google":
		// Gemini Interactions API — https://aistudio.google.com/apikey
		return &gemini{
			key:   key("GEMINI_API_KEY"),
			model: def(ac.Model, "gemini-3.8-flash"),
			base:  def(ac.BaseURL, "https://generativelanguage.googleapis.com"),
		}, nil
	case "openai-compat", "ollama", "local", "vllm":
		base := ac.BaseURL
		if base == "" {
			base = "http://localhost:11434" // ollama default
		}
		return &openai{
			name:  "openai-compat",
			key:   key("OLLAMA_API_KEY"),
			model: def(ac.Model, "qwen3:32b"),
			base:  base,
		}, nil
	case "off", "none", "":
		return nil, fmt.Errorf("no agent provider — run `cometcli config set agent.provider openrouter` (or groq, gemini, anthropic, openai, openai-compat), or set one in a profile")
	default:
		return nil, fmt.Errorf("unknown agent.provider %q", ac.Provider)
	}
}

func def(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// readSSE parses a text/event-stream body, invoking fn with each event's
// joined data payload. Comment and event-name lines are ignored.
func readSSE(r io.Reader, fn func(data string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var data []string
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		d := strings.Join(data, "\n")
		data = data[:0]
		return fn(d)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return dispatch()
}

// apiError extracts {"error":{"message":…}} from a non-2xx response body.
func apiError(prefix string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != nil && e.Error.Message != "" {
		return fmt.Errorf("%s: %s (HTTP %d)", prefix, e.Error.Message, resp.StatusCode)
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return fmt.Errorf("%s: HTTP %d %s", prefix, resp.StatusCode, msg)
}
