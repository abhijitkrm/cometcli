// Package mcp serves the cometcli tool registry over the Model Context
// Protocol — newline-delimited JSON-RPC 2.0 on stdio. Any MCP client
// (Claude Desktop, Cursor, an orchestrator) can then drive the same tools
// the CLI and the built-in agent use.
//
// Safety: observe/diagnose tools run freely; anything that would prompt a
// human (local-change, on-chain) fails fast — there is no TTY behind a
// stdio transport to approve on. Use the CLI for mutating operations.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/abhijitkrm/cometcli/internal/redact"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

const protocolVersion = "2025-03-26"

type Server struct {
	Reg     *toolkit.Registry
	Ctx     *toolkit.Context
	Name    string
	Version string
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads JSON-RPC requests from in until EOF or ctx cancel, writing
// one response per request to out.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 1<<20)
	w := bufio.NewWriter(out)
	enc := json.NewEncoder(w)

	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 1 {
			if resp := s.handle(ctx, line); resp != nil {
				if err := enc.Encode(resp); err != nil {
					return err
				}
				if err := w.Flush(); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (s *Server) handle(ctx context.Context, raw []byte) *response {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return &response{JSONRPC: "2.0", Error: &rpcErr{Code: -32700, Message: "parse error"}}
	}
	// notifications carry no id → no response
	notify := len(req.ID) == 0 || string(req.ID) == "null"

	reply := func(result any) *response {
		if notify {
			return nil
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	fail := func(code int, msg string) *response {
		if notify {
			return nil
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: code, Message: msg}}
	}

	switch req.Method {
	case "initialize":
		return reply(map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		})
	case "ping":
		return reply(map[string]any{})
	case "tools/list":
		return reply(map[string]any{"tools": s.toolList()})
	case "tools/call":
		var p struct {
			Name string         `json:"name"`
			Args map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(-32602, "bad params: "+err.Error())
		}
		res, isErr := s.callTool(ctx, p.Name, p.Args)
		return reply(res.result(isErr))
	case "resources/list", "prompts/list":
		key := "resources"
		if req.Method == "prompts/list" {
			key = "prompts"
		}
		return reply(map[string]any{key: []any{}})
	default:
		return fail(-32601, "method not found: "+req.Method)
	}
}

func (s *Server) toolList() []map[string]any {
	var out []map[string]any
	for _, t := range s.Reg.All() {
		if toolkit.IsLongRunning(t) || toolkit.IsAgentOnly(t) || toolkit.IsOperatorOnly(t) {
			continue // watchers never return; bash/read/edit are the client's own job
		}
		d := map[string]any{
			"name":        t.Name(),
			"description": fmt.Sprintf("[%s] %s", t.Tier(), t.Desc()),
			"inputSchema": t.Schema(),
		}
		ann := map[string]any{}
		switch t.Tier() {
		case toolkit.TierObserve:
			ann["readOnlyHint"] = true
		case toolkit.TierDiagnose:
			ann["readOnlyHint"] = true
		case toolkit.TierLocalChange, toolkit.TierOnChain:
			ann["destructiveHint"] = true
		}
		if len(ann) > 0 {
			d["annotations"] = ann
		}
		out = append(out, d)
	}
	return out
}

type callResult struct {
	text string
	data map[string]any
}

func (r callResult) result(isErr bool) map[string]any {
	res := map[string]any{
		"content": []map[string]any{{"type": "text", "text": r.text}},
	}
	if r.data != nil {
		res["structuredContent"] = r.data
	}
	if isErr {
		res["isError"] = true
	}
	return res
}

func (s *Server) callTool(ctx context.Context, name string, args map[string]any) (callResult, bool) {
	t, ok := s.Reg.Get(name)
	if !ok {
		return callResult{text: "no such tool: " + name}, true
	}
	if toolkit.IsLongRunning(t) {
		return callResult{text: name + " is a long-running watcher — not callable over MCP; use the CLI"}, true
	}
	if toolkit.IsAgentOnly(t) || toolkit.IsOperatorOnly(t) {
		return callResult{text: name + " is not exported over MCP"}, true
	}
	runCtx, cancel := toolkit.WithDeadline(s.Ctx, 120*time.Second)
	defer func() { runCtx.Close(); cancel() }()
	res, err := t.Run(runCtx, toolkit.Args(args))
	if err != nil {
		return callResult{text: "error: " + err.Error()}, true
	}
	text := res.Text
	if text == "" && res.Data != nil {
		text = res.JSON()
	}
	return callResult{text: redact.Text(text), data: res.Data}, false
}
