package silence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func TestForNodeSilencesAtEachMonitor(t *testing.T) {
	var got []map[string]any
	var auth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		got = append(got, m)
		_, _ = w.Write([]byte(`{"silenceID":"s1"}`))
	}))
	defer api.Close()
	t.Setenv(TokenCredential, "tok")
	p := &config.Profile{Name: "val01", ChainID: "c-1", Endpoints: config.Endpoints{Monitor: api.URL + "/metrics"}}
	cfg := &config.Config{Monitor: config.MonitorConf{Silence: []config.SilenceAPI{{URL: api.URL, Via: "val01"}}},
		Profiles: map[string]*config.Profile{"val01": p}}
	c := &toolkit.Context{Context: context.Background(), Profile: p, Cfg: cfg}
	line := ForNode(c, cfg, p, false, 15*time.Minute, "restart")
	// own monitor + the chain instance, which reads the chain through val01:
	// its no-nodes/stalled are silenced too
	if len(got) != 3 || auth != "Bearer tok" || !strings.Contains(line, "alerts for val01 silenced for 15m0s") {
		t.Fatalf("calls %d auth %q line %q", len(got), auth, line)
	}
	m := got[0]["matchers"].([]any)[0].(map[string]any)
	if m["name"] != "profile" || m["value"] != "val01" || m["isEqual"] != true {
		t.Fatalf("matcher %v", m)
	}
	if m := got[2]["matchers"].([]any); len(m) != 2 || m[1].(map[string]any)["value"] != "no-nodes|stalled" {
		t.Fatalf("source matchers %v", m)
	}
	got = nil
	ForNode(c, cfg, p, true, 30*time.Minute, "upgrade")
	if m := got[0]["matchers"].([]any)[0].(map[string]any); m["name"] != "chain_id" || m["value"] != "c-1" {
		t.Fatalf("chain-wide matcher %v", m)
	}
}

func TestForNodeQuietWithoutMonitorOrAPI(t *testing.T) {
	p := &config.Profile{Name: "val01"}
	c := &toolkit.Context{Context: context.Background(), Profile: p}
	if line := ForNode(c, &config.Config{}, p, false, time.Minute, "x"); line != "" {
		t.Fatalf("no monitor configured but said %q", line)
	}
	noAPI := httptest.NewServer(http.NotFoundHandler()) // cometduty without api_token
	defer noAPI.Close()
	p.Endpoints.Monitor = noAPI.URL
	if line := ForNode(c, &config.Config{}, p, false, time.Minute, "x"); line != "" {
		t.Fatalf("monitor without a silence API reported %q", line)
	}
	p.Endpoints.Monitor = "http://127.0.0.1:1" // down: say so, never fail the work
	if line := ForNode(c, &config.Config{}, p, false, time.Minute, "x"); !strings.Contains(line, "could not silence") {
		t.Fatalf("unreachable monitor: %q", line)
	}
}
