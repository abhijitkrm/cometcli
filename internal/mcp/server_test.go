package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

type fakeTool struct {
	name string
	tier toolkit.Tier
	run  func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error)
}

func (f fakeTool) Name() string           { return f.name }
func (f fakeTool) Desc() string           { return "fake " + f.name }
func (f fakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (f fakeTool) Tier() toolkit.Tier     { return f.tier }
func (f fakeTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	return f.run(c, a)
}

func testServer() *Server {
	reg := toolkit.NewRegistry()
	reg.Register(fakeTool{name: "node.status", tier: toolkit.TierObserve,
		run: func(_ *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
			return &toolkit.Result{Text: "height 100", Data: map[string]any{"height": 100}}, nil
		}})
	reg.Register(fakeTool{name: "node.restart", tier: toolkit.TierLocalChange,
		run: func(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
			if err := toolkit.RequireApproval(c, "restart?", toolkit.TierLocalChange, nil); err != nil {
				return nil, err
			}
			return &toolkit.Result{Text: "restarted"}, nil
		}})
	c := &toolkit.Context{
		Context:          context.Background(),
		AutoApproveBelow: toolkit.TierLocalChange,
		Approver: func(_ *toolkit.Context, _ string, tier toolkit.Tier, _ map[string]any) (bool, error) {
			return false, nil
		},
	}
	return &Server{Reg: reg, Ctx: c, Name: "cometcli", Version: "test"}
}

func roundTrip(t *testing.T, s *Server, in string) map[string]any {
	t.Helper()
	var out strings.Builder
	if err := s.Serve(context.Background(), strings.NewReader(in+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("response not json: %q", out.String())
	}
	return resp
}

func TestInitialize(t *testing.T) {
	resp := roundTrip(t, testServer(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	res := resp["result"].(map[string]any)
	if res["protocolVersion"] == "" {
		t.Fatal("no protocolVersion")
	}
	info := res["serverInfo"].(map[string]any)
	if info["name"] != "cometcli" {
		t.Fatalf("serverInfo: %v", info)
	}
}

func TestToolsList(t *testing.T) {
	resp := roundTrip(t, testServer(), `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(tools))
	}
	byName := map[string]map[string]any{}
	for _, tl := range tools {
		m := tl.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if byName["node.status"]["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Fatalf("observe tool should be readOnly: %v", byName["node.status"])
	}
	if byName["node.restart"]["annotations"].(map[string]any)["destructiveHint"] != true {
		t.Fatal("local-change tool should be destructiveHint")
	}
	if byName["node.status"]["inputSchema"] == nil {
		t.Fatal("tool should carry inputSchema")
	}
}

func TestToolsCallObserve(t *testing.T) {
	resp := roundTrip(t, testServer(),
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"node.status","arguments":{}}}`)
	res := resp["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("unexpected isError: %v", res)
	}
	content := res["content"].([]any)[0].(map[string]any)
	if content["text"] != "height 100" {
		t.Fatalf("content: %v", content)
	}
	if res["structuredContent"].(map[string]any)["height"].(float64) != 100 {
		t.Fatalf("structuredContent: %v", res["structuredContent"])
	}
}

func TestToolsCallDeniedTier(t *testing.T) {
	resp := roundTrip(t, testServer(),
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"node.restart","arguments":{}}}`)
	res := resp["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("mutating call should be isError: %v", res)
	}
	content := res["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), "denied") {
		t.Fatalf("expected denial, got %v", content)
	}
}

func TestToolsCallUnknown(t *testing.T) {
	resp := roundTrip(t, testServer(),
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope.nope","arguments":{}}}`)
	res := resp["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("unknown tool should be isError")
	}
}

func TestUnknownMethod(t *testing.T) {
	resp := roundTrip(t, testServer(), `{"jsonrpc":"2.0","id":6,"method":"bogus/method"}`)
	e := resp["error"].(map[string]any)
	if e["code"].(float64) != -32601 {
		t.Fatalf("code: %v", e)
	}
}

func TestParseError(t *testing.T) {
	resp := roundTrip(t, testServer(), `{not json`)
	e := resp["error"].(map[string]any)
	if e["code"].(float64) != -32700 {
		t.Fatalf("code: %v", e)
	}
}

func TestNotificationNoResponse(t *testing.T) {
	var out strings.Builder
	err := testServer().Serve(context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"), &out)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("notification should produce no response, got %q", out.String())
	}
}
