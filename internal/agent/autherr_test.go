package agent

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
)

func resp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func TestAPIErrorAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		auth   bool
	}{
		{"anthropic 401", 401, `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`, true},
		{"openai 401 plain", 401, `Unauthorized`, true},
		{"403", 403, `{"error":{"message":"forbidden"}}`, true},
		{"gemini expired", 400, `{"error":{"message":"API key expired. Please renew the API key."}}`, true},
		{"gemini invalid", 400, `{"error":{"message":"API key not valid. Please pass a valid API key."}}`, true},
		{"other 400", 400, `{"error":{"message":"max_tokens too large"}}`, false},
		{"429", 429, `{"error":{"message":"rate limited"}}`, false},
		{"500", 500, `boom`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := apiError("anthropic", resp(tc.status, tc.body))
			var ae *AuthError
			if got := errors.As(err, &ae); got != tc.auth {
				t.Fatalf("AuthError = %v, want %v (%v)", got, tc.auth, err)
			}
			if tc.auth && !strings.Contains(err.Error(), "config set-key ANTHROPIC_API_KEY") {
				t.Errorf("no fix in message: %v", err)
			}
		})
	}
}

func TestSetKey(t *testing.T) {
	t.Setenv("COMETCLI_HOME", filepath.Join(t.TempDir(), ".cometcli"))
	t.Setenv("GROQ_API_KEY", "old-key")
	t.Setenv("COMETCLI_LLM_API_KEY", "")
	a := &Agent{Provider: noProvider{}, conf: config.AgentConf{Provider: "groq"}}
	p, err := NewProvider(a.conf)
	if err != nil {
		t.Fatal(err)
	}
	a.Provider = p
	if a.KeyEnv() != "GROQ_API_KEY" {
		t.Fatalf("KeyEnv = %q", a.KeyEnv())
	}
	if err := a.SetKey("  "); err == nil {
		t.Error("empty key accepted")
	}
	if err := a.SetKey("new-key\n"); err != nil {
		t.Fatal(err)
	}
	if got := config.Credential("GROQ_API_KEY"); got != "new-key" {
		t.Errorf("saved = %q", got)
	}
	if got := envOr("GROQ_API_KEY"); got != "new-key" {
		t.Errorf("env still holds %q — a stale exported key would win", got)
	}
}

func envOr(k string) string { return strings.TrimSpace(os.Getenv(k)) }
