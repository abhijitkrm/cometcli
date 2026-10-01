package agent

import (
	"os"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
)

func TestProviderGroqDefaults(t *testing.T) {
	os.Unsetenv("COMETCLI_LLM_API_KEY")
	t.Setenv("GROQ_API_KEY", "gsk_test")
	p, err := NewProvider(config.AgentConf{Provider: "groq"})
	if err != nil {
		t.Fatal(err)
	}
	o := p.(*openai)
	if o.base != "https://api.groq.com/openai" {
		t.Fatalf("base = %q", o.base)
	}
	if o.model != "llama-3.3-70b-versatile" {
		t.Fatalf("model = %q", o.model)
	}
	if o.key != "gsk_test" || o.Name() != "groq" {
		t.Fatalf("key/name = %q/%q", o.key, o.Name())
	}
}

func TestProviderGroqOverrides(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "k")
	p, err := NewProvider(config.AgentConf{Provider: "groq",
		Model: "qwen/qwen3-32b", BaseURL: "http://localhost:8080/proxy"})
	if err != nil {
		t.Fatal(err)
	}
	o := p.(*openai)
	if o.model != "qwen/qwen3-32b" || o.base != "http://localhost:8080/proxy" {
		t.Fatalf("overrides not applied: %q %q", o.model, o.base)
	}
}

func TestProviderOff(t *testing.T) {
	if _, err := NewProvider(config.AgentConf{Provider: "off"}); err == nil {
		t.Fatal("off must error")
	}
}
