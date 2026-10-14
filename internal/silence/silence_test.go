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
	t.Setenv("COMETCLI_HOME", t.TempDir())
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
	t.Setenv("COMETCLI_HOME", t.TempDir())
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

func TestPlannedLocalAndAtMonitor(t *testing.T) {
	t.Setenv("COMETCLI_HOME", t.TempDir())
	t.Setenv(TokenCredential, "tok")
	silences := `[]`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(silences))
	}))
	defer api.Close()
	p := &config.Profile{Name: "val01", ChainID: "c-1", Endpoints: config.Endpoints{Monitor: api.URL}}
	cfg := &config.Config{Profiles: map[string]*config.Profile{"val01": p}}
	c := &toolkit.Context{Context: context.Background(), Profile: p, Cfg: cfg}
	if why := Planned(c, cfg, p); why != "" {
		t.Fatalf("planned with nothing recorded: %q", why)
	}
	// a silence of other alerts (the chain view's companion) is not work on the node
	silences = `[{"matchers":[{"name":"chain_id","value":"c-1"},{"name":"alertname","value":"no-nodes|stalled","isRegex":true}],
		"endsAt":"2099-01-01T00:00:00Z","status":{"state":"active"}}]`
	if why := Planned(c, cfg, p); why != "" {
		t.Fatalf("alert-kind silence counted as planned work: %q", why)
	}
	// isEqual absent = equal, as in Alertmanager; expired ones don't count
	silences = `[{"matchers":[{"name":"profile","value":"val01"}],"endsAt":"2099-01-01T00:00:00Z","createdBy":"ops","comment":"disk swap","status":{"state":"expired"}},
		{"matchers":[{"name":"chain_id","value":"c-.*","isRegex":true}],"endsAt":"2099-01-01T00:00:00Z","createdBy":"ops","comment":"upgrade","status":{"state":"active"}}]`
	if why := Planned(c, cfg, p); !strings.Contains(why, "by ops: upgrade") {
		t.Fatalf("chain silence at the monitor not seen: %q", why)
	}
	// recorded locally: seen with no monitor at all
	silences = `[]`
	q := &config.Profile{Name: "val02", ChainID: "c-1"}
	Record(q, false, time.Minute, "node restart")
	if why := Planned(c, cfg, q); !strings.HasPrefix(why, "node restart (until ") {
		t.Fatalf("local record not seen: %q", why)
	}
	Record(q, false, -time.Second, "over")
	if why := Planned(c, cfg, q); why != "" {
		t.Fatalf("ended work still planned: %q", why)
	}
}
