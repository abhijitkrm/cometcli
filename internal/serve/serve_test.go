package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

type scripted struct {
	resps []*agent.Response
	n     int
}

func (s *scripted) Name() string { return "scripted" }
func (s *scripted) Chat(context.Context, *agent.Request) (*agent.Response, error) {
	if s.n >= len(s.resps) {
		return &agent.Response{Text: "done", Done: true}, nil
	}
	r := s.resps[s.n]
	s.n++
	return r, nil
}

type unjail struct{ ran *bool }

func (unjail) Name() string           { return "val.unjail" }
func (unjail) Desc() string           { return "unjail" }
func (unjail) Tier() toolkit.Tier     { return toolkit.TierOnChain }
func (unjail) Schema() map[string]any { return toolkit.ObjSchema(map[string]any{}) }
func (u unjail) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	if err := c.Approve("broadcast unjail", toolkit.TierOnChain, map[string]any{"fee": "200uatom"}); err != nil {
		return nil, err
	}
	*u.ran = true
	return &toolkit.Result{Text: "unjailed at height 100"}, nil
}

func newTestServer(t *testing.T, prov agent.Provider, tools ...toolkit.Tool) (*Server, *httptest.Server) {
	t.Helper()
	t.Setenv("COMETCLI_HOME", t.TempDir())
	reg := toolkit.NewRegistry()
	for _, tl := range tools {
		reg.Register(tl)
	}
	c := &toolkit.Context{Context: context.Background(), Profile: &config.Profile{Name: "p", ChainID: "test-1"}}
	a := &agent.Agent{Provider: prov, Model: "m", Reg: reg, Ctx: c, MaxIter: 4,
		SnapshotFn: func(*toolkit.Context) string { return "" }}
	s := New(c, reg, a, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func authedReq(t *testing.T, s *Server, method, url, body string) *http.Request {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.Token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestGuards(t *testing.T) {
	s, ts := newTestServer(t, &scripted{})
	cases := []struct {
		name string
		req  func() *http.Request
		want int
	}{
		{"no token", func() *http.Request { r, _ := http.NewRequest("GET", ts.URL+"/api/info", nil); return r }, 401},
		{"bad token", func() *http.Request {
			r, _ := http.NewRequest("GET", ts.URL+"/api/info", nil)
			r.Header.Set("Authorization", "Bearer nope")
			return r
		}, 401},
		{"rebinding host", func() *http.Request {
			r := authedReq(t, s, "GET", ts.URL+"/api/info", "")
			r.Host = "evil.example:8765"
			return r
		}, 403},
		{"form post (csrf)", func() *http.Request {
			r := authedReq(t, s, "POST", ts.URL+"/api/cancel", "")
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}, 415},
		{"ok", func() *http.Request { return authedReq(t, s, "GET", ts.URL+"/api/info", "") }, 200},
	}
	for _, c := range cases {
		resp, err := http.DefaultClient.Do(c.req())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

func TestTokenLinkSetsCookie(t *testing.T) {
	s, ts := newTestServer(t, &scripted{})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(ts.URL + "/?token=" + s.Token)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) == 0 || !resp.Cookies()[0].HttpOnly {
		t.Fatalf("status=%d cookies=%v", resp.StatusCode, resp.Cookies())
	}
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.AddCookie(resp.Cookies()[0])
	page, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	if page.StatusCode != 200 || !strings.Contains(page.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("page status=%d", page.StatusCode)
	}
}

func TestListenRefusesNonLoopback(t *testing.T) {
	if _, err := Listen("0.0.0.0:0"); err == nil {
		t.Fatal("0.0.0.0 must be refused")
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}

// readEvents consumes an SSE body, calling fn per event until it returns false.
func readEvents(t *testing.T, resp *http.Response, fn func(map[string]any) bool) {
	t.Helper()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		if !fn(e) {
			return
		}
	}
}

func TestChatStreamsAndApprovesOnChain(t *testing.T) {
	ran := false
	prov := &scripted{resps: []*agent.Response{
		{Text: "unjailing", Calls: []agent.Call{{ID: "c1", Name: "val__unjail", Args: json.RawMessage(`{}`)}}},
		{Text: "you are back in the active set", Done: true},
	}}
	s, ts := newTestServer(t, prov, unjail{&ran})
	resp, err := http.DefaultClient.Do(authedReq(t, s, "POST", ts.URL+"/api/chat", `{"text":"unjail me"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("chat status %d", resp.StatusCode)
	}

	// a second turn while one runs is refused
	busy, _ := http.DefaultClient.Do(authedReq(t, s, "POST", ts.URL+"/api/chat", `{"text":"again"}`))
	busy.Body.Close()
	if busy.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent turn status %d, want 409", busy.StatusCode)
	}

	var kinds []string
	readEvents(t, resp, func(e map[string]any) bool {
		kind := e["kind"].(string)
		kinds = append(kinds, kind)
		if kind == "approval" {
			ap := e["approval"].(map[string]any)
			if ap["tier"] != "on-chain" {
				t.Errorf("approval tier = %v", ap["tier"])
			}
			body := `{"id":"` + ap["id"].(string) + `","ok":true}`
			r, err := http.DefaultClient.Do(authedReq(t, s, "POST", ts.URL+"/api/approve", body))
			if err != nil || r.StatusCode != 200 {
				t.Fatalf("approve failed: %v %v", err, r.StatusCode)
			}
			r.Body.Close()
		}
		return kind != "done" && kind != "error"
	})
	if !ran {
		t.Fatalf("approved tool did not run; events %v", kinds)
	}
	want := "text tool_start approval tool_result text done"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("events = %v, want %s", kinds, want)
	}
}

func TestChatDenyAndDisconnectNeverHang(t *testing.T) {
	ran := false
	prov := &scripted{resps: []*agent.Response{
		{Calls: []agent.Call{{ID: "c1", Name: "val__unjail", Args: json.RawMessage(`{}`)}}},
	}}
	s, ts := newTestServer(t, prov, unjail{&ran})
	resp, err := http.DefaultClient.Do(authedReq(t, s, "POST", ts.URL+"/api/chat", `{"text":"unjail"}`))
	if err != nil {
		t.Fatal(err)
	}
	// drop the stream as soon as the approval shows up
	readEvents(t, resp, func(e map[string]any) bool { return e["kind"] != "approval" })
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for s.busy.Load() {
		if time.Now().After(deadline) {
			t.Fatal("turn still busy after client disconnect — approval wedged")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ran {
		t.Fatal("tool ran without approval")
	}
}

func TestCommandEndpoint(t *testing.T) {
	s, ts := newTestServer(t, &scripted{})
	r, _ := http.DefaultClient.Do(authedReq(t, s, "POST", ts.URL+"/api/command", `{"line":"/mode readonly"}`))
	var out map[string]string
	json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(out["text"], "readonly") || !s.Agent.Policy.ReadOnly() {
		t.Fatalf("status=%d out=%v", r.StatusCode, out)
	}
	r, _ = http.DefaultClient.Do(authedReq(t, s, "POST", ts.URL+"/api/command", `{"line":"/approve on-chain on"}`))
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Fatalf("on-chain autopilot status %d, want 400", r.StatusCode)
	}
}
