package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// setupProviders are the choices offered when no model is configured,
// with the env var each provider reads its key from ("" = no key).
var setupProviders = []struct{ name, keyEnv, note string }{
	{"anthropic", "ANTHROPIC_API_KEY", "Claude — console.anthropic.com"},
	{"openai", "OPENAI_API_KEY", "platform.openai.com"},
	{"gemini", "GEMINI_API_KEY", "aistudio.google.com/apikey"},
	{"groq", "GROQ_API_KEY", "console.groq.com"},
	{"openrouter", "OPENROUTER_API_KEY", "free models — openrouter.ai"},
	{"openai-compat", "", "local ollama / vLLM, no key"},
}

// setupProvider asks for a provider and its key when the chat starts with
// no model, and saves both (agent.provider in config.yaml, the key in
// ~/.cometcli/credentials). It returns false when the user skips.
func setupProvider(cfg *config.Config, r *bufio.Reader, out io.Writer) (bool, error) {
	fmt.Fprintln(out, "no AI model is set up yet — the chat needs one (known questions still work without it)")
	for i, p := range setupProviders {
		fmt.Fprintf(out, "  %d  %-14s %s\n", i+1, p.name, p.note)
	}
	choice := ask(r, out, "provider (number or name, Enter to skip)", "")
	if choice == "" {
		fmt.Fprintln(out, "skipped — set one later: cometcli config set agent.provider <name>")
		return false, nil
	}
	idx := -1
	for i, p := range setupProviders {
		if choice == fmt.Sprint(i+1) || strings.EqualFold(choice, p.name) {
			idx = i
		}
	}
	if idx < 0 {
		fmt.Fprintf(out, "unknown provider %q — skipped; set one later: cometcli config set agent.provider <name>\n", choice)
		return false, nil
	}
	p := setupProviders[idx]
	if p.keyEnv == "" {
		cfg.Agent.BaseURL = ask(r, out, "server URL", "http://localhost:11434")
	} else if os.Getenv(p.keyEnv) == "" && config.Credential(p.keyEnv) == "" {
		key, err := readHidden(r, out, p.keyEnv+" (hidden): ")
		if err != nil {
			return false, err
		}
		if key = strings.TrimSpace(key); key == "" {
			fmt.Fprintf(out, "no key — skipped; later: cometcli config set-key %s\n", p.keyEnv)
			return false, nil
		}
		if err := config.SetCredential(p.keyEnv, key); err != nil {
			return false, err
		}
		path, _ := config.Path("credentials")
		fmt.Fprintf(out, "saved %s to %s (0600)\n", p.keyEnv, path)
	}
	cfg.Agent.Provider = p.name
	if err := cfg.Save(); err != nil {
		return false, err
	}
	fmt.Fprintf(out, "agent.provider = %s — change it any time with cometcli config set agent.provider\n\n", p.name)
	return true, nil
}
