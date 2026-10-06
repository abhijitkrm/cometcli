// Package mcpclient connects to Model Context Protocol servers (stdio or
// streamable HTTP) so their tools can be offered to the agent.
package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abhijitkrm/cometcli/internal/settings"
)

const protocolVersion = "2025-06-18"

// Tool is a tool a server offers.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations struct {
		ReadOnlyHint    bool `json:"readOnlyHint"`
		DestructiveHint bool `json:"destructiveHint"`
	} `json:"annotations"`
}

// Content is one item of a tool result.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	MimeType string `json:"mimeType"`
	Resource *struct {
		URI  string `json:"uri"`
		Text string `json:"text"`
	} `json:"resource"`
}

// CallResult is a tools/call result.
type CallResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError"`
}

// Text flattens the result for the model.
func (r *CallResult) Text() string {
	var parts []string
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "resource":
			if c.Resource != nil {
				parts = append(parts, fmt.Sprintf("[resource %s]\n%s", c.Resource.URI, c.Resource.Text))
			}
		default:
			parts = append(parts, fmt.Sprintf("[%s content (%s) omitted]", c.Type, c.MimeType))
		}
	}
	return strings.Join(parts, "\n")
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcMsg struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  any              `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type transport interface {
	roundTrip(ctx context.Context, id int64, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	close() error
}

// Client is one connected server.
type Client struct {
	Name  string
	Tools []Tool
	tr    transport
	seq   atomic.Int64
}

// Connect starts or dials a server, initializes it and lists its tools.
func Connect(ctx context.Context, name string, cfg settings.MCPServer) (*Client, error) {
	var tr transport
	var err error
	switch strings.ToLower(cfg.Type) {
	case "", "stdio":
		tr, err = newStdio(cfg)
	case "http", "streamable-http", "streamablehttp":
		tr = &httpTransport{url: expand(cfg.URL), headers: expandMap(cfg.Headers), hc: &http.Client{Timeout: 5 * time.Minute}}
	case "sse":
		return nil, fmt.Errorf("mcp %s: legacy SSE transport isn't supported — use the server's streamable HTTP endpoint", name)
	default:
		return nil, fmt.Errorf("mcp %s: unknown transport %q", name, cfg.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	c := &Client{Name: name, tr: tr}
	if err := c.init(ctx); err != nil {
		_ = tr.close()
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	return c, nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	raw, err := c.tr.roundTrip(ctx, c.seq.Add(1), method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) init(ctx context.Context) error {
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "cometcli", "version": "1"},
	}, nil)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := c.tr.notify(ctx, "notifications/initialized", nil); err != nil {
		return err
	}
	cursor := ""
	for page := 0; page < 20; page++ {
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return fmt.Errorf("tools/list: %w", err)
		}
		c.Tools = append(c.Tools, res.Tools...)
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	return nil
}

// CallTool invokes a tool.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var res CallResult
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Close stops the server or ends the HTTP session.
func (c *Client) Close() error { return c.tr.close() }

// --- stdio -------------------------------------------------------------------

type stdioTransport struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	mu       sync.Mutex // serializes writes
	pmu      sync.Mutex
	wait     map[string]chan rpcMsg
	done     chan struct{}
	stderr   *limitedBuffer
	err      error
	waitOnce sync.Once
}

// reap waits for the process exactly once (Wait isn't concurrency-safe).
func (t *stdioTransport) reap() { t.waitOnce.Do(func() { _ = t.cmd.Wait() }) }

func newStdio(cfg settings.MCPServer) (*stdioTransport, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("no command")
	}
	args := make([]string, len(cfg.Args))
	for i, a := range cfg.Args {
		args[i] = expand(a)
	}
	cmd := exec.Command(expand(cfg.Command), args...)
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+expand(v))
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	t := &stdioTransport{cmd: cmd, stdin: stdin, wait: map[string]chan rpcMsg{}, done: make(chan struct{}), stderr: &limitedBuffer{b: &bytes.Buffer{}, max: 8 << 10}}
	cmd.Stderr = t.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go t.read(stdout)
	return t, nil
}

func (t *stdioTransport) read(r io.Reader) {
	defer close(t.done)
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var m rpcMsg
			if json.Unmarshal(line, &m) == nil {
				t.dispatch(m)
			}
		}
		if err != nil {
			t.err = err
			return
		}
	}
}

func (t *stdioTransport) dispatch(m rpcMsg) {
	if m.ID == nil {
		return // notification from the server
	}
	if m.Method != "" {
		// a request from the server (roots/list, sampling…): unsupported
		_ = t.write(rpcMsg{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: -32601, Message: "not supported by cometcli"}})
		return
	}
	t.pmu.Lock()
	ch := t.wait[string(*m.ID)]
	delete(t.wait, string(*m.ID))
	t.pmu.Unlock()
	if ch != nil {
		ch <- m
	}
}

func (t *stdioTransport) write(m rpcMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err = t.stdin.Write(append(b, '\n'))
	return err
}

func (t *stdioTransport) roundTrip(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	rid := json.RawMessage(fmt.Sprint(id))
	ch := make(chan rpcMsg, 1)
	t.pmu.Lock()
	t.wait[string(rid)] = ch
	t.pmu.Unlock()
	if err := t.write(rpcMsg{JSONRPC: "2.0", ID: &rid, Method: method, Params: params}); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, fmt.Errorf("%s (code %d)", m.Error.Message, m.Error.Code)
		}
		return m.Result, nil
	case <-t.done:
		t.reap() // stderr is fully copied once the process is reaped
		msg := strings.TrimSpace(t.stderr.String())
		if msg == "" && t.err != nil {
			msg = t.err.Error()
		}
		return nil, fmt.Errorf("server exited: %s", lastLines(msg, 3))
	case <-ctx.Done():
		t.pmu.Lock()
		delete(t.wait, string(rid))
		t.pmu.Unlock()
		return nil, ctx.Err()
	}
}

func (t *stdioTransport) notify(_ context.Context, method string, params any) error {
	return t.write(rpcMsg{JSONRPC: "2.0", Method: method, Params: params})
}

func (t *stdioTransport) close() error {
	t.stdin.Close()
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
		_ = t.cmd.Process.Kill()
	}
	t.reap()
	return nil
}

type limitedBuffer struct {
	mu  sync.Mutex
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := l.max - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// String returns what was captured so far.
func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// --- streamable HTTP -----------------------------------------------------------

type httpTransport struct {
	url     string
	headers map[string]string
	hc      *http.Client
	session atomic.Value // string
}

func (h *httpTransport) post(ctx context.Context, m rpcMsg) (*http.Response, error) {
	b, _ := json.Marshal(m)
	req, err := http.NewRequestWithContext(ctx, "POST", h.url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	if s, _ := h.session.Load().(string); s != "" {
		req.Header.Set("Mcp-Session-Id", s)
	}
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		h.session.Store(s)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func (h *httpTransport) roundTrip(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	rid := json.RawMessage(fmt.Sprint(id))
	resp, err := h.post(ctx, rpcMsg{JSONRPC: "2.0", ID: &rid, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m rpcMsg
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		found := false
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		var data []string
		for sc.Scan() && !found {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			case line == "" && len(data) > 0:
				var x rpcMsg
				if json.Unmarshal([]byte(strings.Join(data, "\n")), &x) == nil && x.ID != nil && string(*x.ID) == string(rid) && x.Method == "" {
					m, found = x, true
				}
				data = nil
			}
		}
		if !found {
			return nil, errors.New("stream ended without a response")
		}
	} else if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	if m.Error != nil {
		return nil, fmt.Errorf("%s (code %d)", m.Error.Message, m.Error.Code)
	}
	return m.Result, nil
}

func (h *httpTransport) notify(ctx context.Context, method string, params any) error {
	resp, err := h.post(ctx, rpcMsg{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (h *httpTransport) close() error {
	s, _ := h.session.Load().(string)
	if s == "" {
		return nil
	}
	req, err := http.NewRequest("DELETE", h.url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Mcp-Session-Id", s)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if resp, err := h.hc.Do(req.WithContext(ctx)); err == nil {
		resp.Body.Close()
	}
	return nil
}

// --- ${VAR} expansion ------------------------------------------------------------

// expand substitutes ${VAR} and ${VAR:-default} from the environment.
func expand(s string) string {
	return os.Expand(s, func(k string) string {
		if name, d, ok := strings.Cut(k, ":-"); ok {
			if v := os.Getenv(name); v != "" {
				return v
			}
			return d
		}
		return os.Getenv(k)
	})
}

func expandMap(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = expand(v)
	}
	return out
}
