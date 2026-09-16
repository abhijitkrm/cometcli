// Package agent implements the LLM agent loop: a provider-agnostic
// message/tool abstraction over Anthropic, OpenAI, and OpenAI-compatible
// endpoints, driving the shared tool registry.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
	MaxTok   int
}

// Response is the model's reply.
type Response struct {
	Text  string
	Calls []Call
	Done  bool // true when the model finished (no tool calls pending)
}

// Provider talks to an LLM API.
type Provider interface {
	Chat(ctx context.Context, r *Request) (*Response, error)
	Name() string
}

// NewProvider builds the provider from profile agent config.
// Env fallback: COMETCLI_LLM_API_KEY, then provider-specific vars.
func NewProvider(ac config.AgentConf) (Provider, error) {
	key := func(env string) string {
		if ac.APIKeyEnv != "" {
			return os.Getenv(ac.APIKeyEnv)
		}
		if v := os.Getenv("COMETCLI_LLM_API_KEY"); v != "" {
			return v
		}
		return os.Getenv(env)
	}
	switch strings.ToLower(ac.Provider) {
	case "anthropic", "claude":
		return &anthropic{
			key:   key("ANTHROPIC_API_KEY"),
			model: def(ac.Model, "claude-sonnet-4-5"),
			base:  def(ac.BaseURL, "https://api.anthropic.com"),
		}, nil
	case "openai":
		return &openai{
			key:   key("OPENAI_API_KEY"),
			model: def(ac.Model, "gpt-4.1"),
			base:  def(ac.BaseURL, "https://api.openai.com"),
		}, nil
	case "openai-compat", "ollama", "local", "vllm":
		base := ac.BaseURL
		if base == "" {
			base = "http://localhost:11434" // ollama default
		}
		return &openai{
			key:   key("OLLAMA_API_KEY"),
			model: def(ac.Model, "qwen3:32b"),
			base:  base,
		}, nil
	case "off", "none", "":
		return nil, fmt.Errorf("agent disabled — set agent.provider in the profile")
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
