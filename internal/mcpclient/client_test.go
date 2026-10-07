package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/settings"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// TestMain doubles as a tiny stdio MCP server when MCP_FAKE_SERVER=1.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_FAKE_SERVER") == "1" {
		fakeServer()
		return
	}
	os.Exit(m.Run())
}

func respond(id json.RawMessage, method string, params map[string]any) any {
	switch method {
	case "initialize":
		return map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake"}}
	case "tools/list":
		if params != nil && params["cursor"] == "p2" {
			return map[string]any{"tools": []any{map[string]any{"name": "delete_dash", "description": "deletes", "inputSchema": map[string]any{"type": "object"}}}}
		}
		return map[string]any{"nextCursor": "p2", "tools": []any{map[string]any{"name": "echo", "description": "echoes", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}}, "annotations": map[string]any{"readOnlyHint": true}}}}
	case "tools/call":
		args, _ := params["arguments"].(map[string]any)
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("echo: %v", args["msg"])}}}
	}
	return nil
}

func fakeServer() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue
		}
		// a server→client request first, to check the client answers it
		if m.Method == "tools/call" {
			fmt.Println(`{"jsonrpc":"2.0","id":"srv-1","method":"roots/list"}`)
			fmt.Println(`{"jsonrpc":"2.0","method":"notifications/message","params":{}}`)
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": respond(m.ID, m.Method, m.Params)})
		fmt.Println(string(b))
	}
}

func TestStdioServer(t *testing.T) {
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, "fake", settings.MCPServer{Command: exe, Args: []string{"-test.run=^$"}, Env: map[string]string{"MCP_FAKE_SERVER": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(c.Tools) != 2 {
		t.Fatalf("paginated tools = %+v", c.Tools)
	}
	res, err := c.CallTool(ctx, "echo", map[string]any{"msg": "hi"})
	if err != nil || res.Text() != "echo: hi" {
		t.Fatalf("call = %+v %v", res, err)
	}

	// adapter: names, tiers, schema, permission gate
	echo, del := NewAgentTool(c, c.Tools[0]), NewAgentTool(c, c.Tools[1])
	if echo.Name() != "mcp__fake__echo" || echo.Tier() != toolkit.TierDiagnose || del.Tier() != toolkit.TierLocalChange {
		t.Fatalf("adapter: %s %s %s", echo.Name(), echo.Tier(), del.Tier())
	}
	if p, ok := del.Schema()["properties"].(map[string]any); !ok || p == nil {
		t.Fatal("schema properties not normalized")
	}
	asked := 0
	tc := &toolkit.Context{Context: ctx, AutoApproveBelow: toolkit.TierLocalChange,
		Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { asked++; return false, nil }}
	if r, err := echo.Run(tc, toolkit.Args{"msg": "x"}); err != nil || r.Text != "echo: x" || asked != 0 {
		t.Fatalf("read-only tool: %v %v asked=%d", r, err, asked)
	}
	if _, err := del.Run(tc, toolkit.Args{}); err == nil || asked != 1 {
		t.Fatalf("mutating tool must ask: %v asked=%d", err, asked)
	}
	tc.Rules, _ = toolkit.NewRules([]string{"mcp__fake"}, nil, nil)
	if _, err := del.Run(tc, toolkit.Args{}); err != nil || asked != 1 {
		t.Fatalf("server-wide allow rule ignored: %v asked=%d", err, asked)
	}
}

func TestHTTPServerSSEAndSession(t *testing.T) {
	var sawSession bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&m)
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		if m.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "s-1")
		} else if r.Header.Get("Mcp-Session-Id") == "s-1" {
			sawSession = true
		}
		if len(m.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": respond(m.ID, m.Method, m.Params)})
		if m.Method == "tools/call" { // answer as an SSE stream
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\ndata: %s\n\n", b)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	defer srv.Close()
	t.Setenv("TOK", "tok")
	ctx := context.Background()
	c, err := Connect(ctx, "web", settings.MCPServer{Type: "http", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer ${TOK}"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.CallTool(ctx, "echo", map[string]any{"msg": "sse"})
	if err != nil || res.Text() != "echo: sse" || !sawSession {
		t.Fatalf("call = %v %v session=%v", res, err, sawSession)
	}
}

func TestStdioServerThatExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Connect(ctx, "bad", settings.MCPServer{Command: "sh", Args: []string{"-c", "echo 'boom: missing token' >&2; exit 1"}})
	if err == nil || !strings.Contains(err.Error(), "boom: missing token") {
		t.Fatalf("err = %v", err)
	}
}
