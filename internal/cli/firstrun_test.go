package cli

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
)

func TestSetupProvider(t *testing.T) {
	cases := []struct {
		name, input, provider, keyEnv, key string
		ok                                 bool
	}{
		{"skip", "\n", "", "", "", false},
		{"by number", "1\nsk-test\n", "anthropic", "ANTHROPIC_API_KEY", "sk-test", true},
		{"by name", "groq\ngsk-test\n", "groq", "GROQ_API_KEY", "gsk-test", true},
		{"no key", "openai\n\n", "", "OPENAI_API_KEY", "", false},
		{"unknown", "nope\n", "", "", "", false},
		{"local", "6\n\n", "openai-compat", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("COMETCLI_HOME", filepath.Join(home, ".cometcli"))
			for _, p := range setupProviders {
				if p.keyEnv != "" {
					t.Setenv(p.keyEnv, "")
					os.Unsetenv(p.keyEnv)
				}
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			ok, err := setupProvider(cfg, bufio.NewReader(strings.NewReader(tc.input)), io.Discard)
			if err != nil || ok != tc.ok {
				t.Fatalf("ok=%v err=%v, want ok=%v", ok, err, tc.ok)
			}
			saved, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if saved.Agent.Provider != tc.provider {
				t.Errorf("provider = %q, want %q", saved.Agent.Provider, tc.provider)
			}
			if tc.provider == "openai-compat" && saved.Agent.BaseURL != "http://localhost:11434" {
				t.Errorf("base_url = %q", saved.Agent.BaseURL)
			}
			if tc.keyEnv != "" && config.Credential(tc.keyEnv) != tc.key {
				t.Errorf("%s = %q, want %q", tc.keyEnv, config.Credential(tc.keyEnv), tc.key)
			}
		})
	}
}

func TestProfileSelected(t *testing.T) {
	t.Setenv("COMETCLI_PROFILE", "")
	old := flagProfile
	defer func() { flagProfile = old }()
	flagProfile = ""
	if profileSelected(&config.Config{}) {
		t.Error("fresh config: want no profile selected")
	}
	if !profileSelected(&config.Config{Active: "val1"}) {
		t.Error("active profile: want selected")
	}
	t.Setenv("COMETCLI_PROFILE", "val2")
	if !profileSelected(&config.Config{}) {
		t.Error("COMETCLI_PROFILE: want selected")
	}
}
