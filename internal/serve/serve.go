// Package serve exposes one agent session over a loopback-only HTTP API
// with a small embedded web chat. It is a third front-end over the same
// agent.Agent the TUI and REPL drive — same tools, same policy, same
// redaction, same audit trail.
//
// Security model: binds to loopback only; every request must carry the
// per-process token (cookie set by the one-time ?token= link, or a Bearer
// header) and a loopback Host header (defeats DNS rebinding). Mutating
// calls require a JSON content type, which cross-site forms cannot send
// without a CORS preflight that this server never answers.
package serve

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/monitor"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

//go:embed web/index.html
var indexHTML []byte

const cookieName = "cometcli_token"

// Server is one web chat session.
type Server struct {
	Agent    *agent.Agent // nil when no provider is configured
	AgentErr error
	Ctx      *toolkit.Context
	Reg      *toolkit.Registry
	Token    string

	busy    atomic.Bool
	mu      sync.Mutex // guards sink + cancel
	sink    chan agent.Event
	cancel  context.CancelFunc
	pending sync.Map // approval id → *agent.Approval
	port    string
}

// New wires a server around an agent (which may be nil, with err set).
func New(c *toolkit.Context, reg *toolkit.Registry, a *agent.Agent, agentErr error) *Server {
	var b [24]byte
	_, _ = rand.Read(b[:])
	s := &Server{Agent: a, AgentErr: agentErr, Ctx: c, Reg: reg, Token: hex.EncodeToString(b[:])}
	if a != nil {
		a.OnEvent = s.emit
		// approvals become modal prompts in the browser
		a.Ctx.Approver = agent.EventApprover(s.emit)
	}
	return s
}

// emit forwards an agent event to the active SSE stream (dropped if none).
func (s *Server) emit(e agent.Event) {
	if e.Approval != nil {
		s.pending.Store(e.Approval.ID, e.Approval)
	}
	s.mu.Lock()
	ch := s.sink
	s.mu.Unlock()
	if ch == nil {
		if e.Approval != nil { // nobody to ask — deny rather than hang
			e.Approval.Answer(false)
		}
		return
	}
	select {
	case ch <- e:
	case <-time.After(5 * time.Second): // stream gone and buffer full — never wedge the agent
		if e.Approval != nil {
			e.Approval.Answer(false)
		}
	}
}

// Listen validates addr is loopback and returns a listener.
func Listen(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("refusing to listen on %s — the web chat binds to loopback only (use ssh -L to reach it remotely)", host)
	}
	return net.Listen("tcp", addr)
}

// Serve runs the HTTP server on ln until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	_, s.port, _ = net.SplitHostPort(ln.Addr().String())
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Handler returns the routed, guarded handler (exported for tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("POST /api/chat", s.chat)
	mux.HandleFunc("POST /api/approve", s.approve)
	mux.HandleFunc("POST /api/cancel", s.cancelTurn)
	mux.HandleFunc("POST /api/command", s.command)
	return s.guard(mux)
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		// one-time link: ?token=… on the page sets the cookie, then strips it
		if r.Method == http.MethodGet && r.URL.Path == "/" && r.URL.Query().Has("token") {
			if !s.tokenOK(r.URL.Query().Get("token")) {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.Token, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if !s.authed(r) {
			http.Error(w, "unauthorized — open the link printed by `cometcli serve`", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1
}

func (s *Server) authed(r *http.Request) bool {
	if c, err := r.Cookie(cookieName); err == nil && s.tokenOK(c.Value) {
		return true
	}
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && s.tokenOK(t) {
		return true
	}
	return false
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:")
	_, _ = w.Write(indexHTML)
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	p := s.Ctx.Profile
	out := map[string]any{
		"profile": p.Name, "chain_id": p.ChainID, "evm_chain_id": p.EVMChainID,
		"role": p.Role, "busy": s.busy.Load(), "audit": s.Ctx.Audit.Path(),
	}
	if s.Agent != nil {
		out["provider"] = s.Agent.Provider.Name()
		out["model"] = s.Agent.Model
		out["mode"] = string(s.Agent.Policy.Mode)
		out["autopilot_local"] = s.Agent.Policy.AutoLocal
		out["policy"] = s.Agent.Policy.String()
		out["session"] = s.Agent.ID()
	} else if s.AgentErr != nil {
		out["agent_error"] = s.AgentErr.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	sub := &toolkit.Context{Context: ctx, Profile: s.Ctx.Profile, Cfg: s.Ctx.Cfg, Audit: s.Ctx.Audit}
	defer sub.Close()
	writeJSON(w, http.StatusOK, monitor.Collect(sub))
}

// chat runs one agent turn and streams its events as SSE. One turn at a
// time; closing the stream (tab closed, Stop pressed) cancels the turn.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text required"})
		return
	}
	if s.Agent == nil {
		msg := "no agent configured — set agent.provider in the profile"
		if s.AgentErr != nil {
			msg = s.AgentErr.Error()
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msg})
		return
	}
	if !s.busy.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a turn is already running — stop it first"})
		return
	}
	defer s.busy.Store(false)
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ch := make(chan agent.Event, 256)
	s.mu.Lock()
	s.sink, s.cancel = ch, cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sink, s.cancel = nil, nil
		s.mu.Unlock()
		// deny anything still waiting so the agent goroutine can't hang
		s.pending.Range(func(k, v any) bool {
			v.(*agent.Approval).Answer(false)
			s.pending.Delete(k)
			return true
		})
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}

	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		out, err := s.Agent.Run(ctx, req.Text)
		done <- result{out, err}
	}()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case e := <-ch:
			send(e)
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case res := <-done:
			for drained := false; !drained; { // flush events emitted just before return
				select {
				case e := <-ch:
					send(e)
				default:
					drained = true
				}
			}
			if res.err != nil {
				msg := res.err.Error()
				if errors.Is(res.err, context.Canceled) {
					msg = "cancelled"
				}
				send(map[string]string{"kind": "error", "error": msg})
			} else {
				send(map[string]string{"kind": "done"})
			}
			return
		}
	}
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
		OK bool   `json:"ok"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	v, ok := s.pending.LoadAndDelete(req.ID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending approval " + req.ID})
		return
	}
	v.(*agent.Approval).Answer(req.OK)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": req.OK})
}

func (s *Server) cancelTurn(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": c != nil})
}

func (s *Server) command(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Line string `json:"line"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if strings.HasPrefix(strings.TrimSpace(req.Line), "/help") {
		writeJSON(w, http.StatusOK, map[string]string{"text": "commands:\n" + agent.CommandHelp + "\n  /stop                         cancel the running turn"})
		return
	}
	if s.busy.Load() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a turn is running — wait or stop it before changing session settings"})
		return
	}
	res, err := agent.RunCommand(s.Agent, s.Ctx, s.Reg, req.Line)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, agent.ErrUnknownCommand) {
			err = fmt.Errorf("unknown command — /help")
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	if res.Job != nil {
		txt, err := res.Job(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		res.Text = txt
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": res.Text, "prompt": res.Prompt})
}
