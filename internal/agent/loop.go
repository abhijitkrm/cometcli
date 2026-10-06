package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/redact"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Agent runs the LLM ↔ tool loop over the shared registry. One Agent is one
// session: it owns the conversation, the approval policy, the redactor, and
// a session id stamped on every audit event.
type Agent struct {
	Provider Provider
	Model    string
	Reg      *toolkit.Registry
	Ctx      *toolkit.Context
	MaxIter  int
	// MaxCalls bounds tool invocations per Run (0 = unlimited). Bounds
	// spend, not just rounds — MaxIter counts model turns.
	MaxCalls int
	// Policy decides which tiers are advertised, auto-run, confirmed, or
	// refused. Read-only mode filters mutating tools out entirely.
	Policy Policy
	// Redact scrubs everything entering the LLM context (user input,
	// snapshot, tool output) and everything echoed to UIs/logs.
	Redact *redact.Redactor
	// Stream enables token streaming when the provider supports it.
	Stream bool
	// OnEvent receives renderable steps of each turn (may be nil).
	OnEvent func(Event)
	// SnapshotFn builds the live node digest for the system prompt;
	// defaults to SnapshotText. Refreshed at most every SnapshotTTL.
	SnapshotFn  func(*toolkit.Context) string
	SnapshotTTL time.Duration

	conf      config.AgentConf
	id        string
	audit     *audit.Logger
	history   []Msg
	snap      string
	snapAt    time.Time
	calls     int
	toolTrunc int
}

// New builds an agent for a context from the profile's agent config.
func New(c *toolkit.Context, reg *toolkit.Registry) (*Agent, error) {
	ac := c.Profile.Agent
	prov, err := NewProvider(ac)
	if err != nil {
		return nil, err
	}
	pol, err := PolicyFrom(ac)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		Provider: prov,
		Model:    def(ac.Model, ""),
		Reg:      reg,
		Ctx:      c,
		MaxIter:  16,
		Policy:   pol,
		Redact:   RedactorFor(c.Profile),
		Stream:   !ac.NoStream,
		conf:     ac,
	}
	if a.Model == "" {
		a.Model = modelOf(prov)
	}
	return a, nil
}

// RedactorFor builds the profile's redactor: base rules plus agent
// redact_hosts, and the profile's own endpoint/SSH hosts when
// redact_endpoints is set.
func RedactorFor(p *config.Profile) *redact.Redactor {
	if p == nil {
		return redact.NewRedactor()
	}
	hosts := append([]string{}, p.Agent.RedactHosts...)
	if p.Agent.RedactEndpoints {
		hosts = append(hosts, p.Transport.Host,
			redact.HostOf(p.Endpoints.Comet), redact.HostOf(p.Endpoints.GRPC),
			redact.HostOf(p.Endpoints.LCD), redact.HostOf(p.Endpoints.EVM))
	}
	return redact.NewRedactor(hosts...)
}

func modelOf(p Provider) string {
	switch t := p.(type) {
	case *anthropic:
		return t.model
	case *openai:
		return t.model
	}
	return ""
}

// ID returns the session id (assigned lazily on first use).
func (a *Agent) ID() string {
	if a.id == "" {
		var b [6]byte
		_, _ = rand.Read(b[:])
		a.id = time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
	}
	return a.id
}

// Audit returns the session-stamped audit logger (nil when auditing is off).
func (a *Agent) Audit() *audit.Logger {
	if a.audit == nil && a.Ctx != nil && a.Ctx.Audit != nil {
		a.audit = a.Ctx.Audit.WithSession(a.ID())
	}
	return a.audit
}

// Reset clears conversation history and starts a new audit session.
func (a *Agent) Reset() {
	a.history = nil
	a.id, a.audit = "", nil
	a.snap = ""
}

func (a *Agent) emit(e Event) {
	if a.OnEvent != nil {
		a.OnEvent(e)
	}
}

func (a *Agent) profileName() string {
	if a.Ctx != nil && a.Ctx.Profile != nil {
		return a.Ctx.Profile.Name
	}
	return ""
}

// toolDefs converts the registry to provider tool schemas, hiding tiers
// the policy refuses.
func (a *Agent) toolDefs() []ToolDef {
	var out []ToolDef
	for _, t := range a.Reg.All() {
		if toolkit.IsLongRunning(t) {
			continue // watchers never return inside a turn
		}
		if !a.Policy.Allows(t.Tier()) {
			continue
		}
		out = append(out, ToolDef{
			Name:   toolFnName(t.Name()),
			Desc:   fmt.Sprintf("[%s] %s", t.Tier(), t.Desc()),
			Schema: t.Schema(),
		})
	}
	return out
}

func toolFnName(n string) string { return strings.ReplaceAll(n, ".", "__") }

// Run processes one user turn, executing tools until the model finishes.
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	a.calls = 0
	input = a.Redact.Text(input)
	a.history = append(a.history, Msg{Role: "user", Text: input})
	if lg := a.Audit(); lg != nil {
		_ = lg.Log(audit.KindPrompt, a.profileName(), map[string]any{"text": input})
	}

	for i := 0; i < a.MaxIter; i++ {
		req := &Request{
			Model:    a.Model,
			System:   a.system(),
			Messages: a.history,
			Tools:    a.toolDefs(),
			MaxTok:   4096,
		}
		resp, err := a.chat(ctx, req)
		if err != nil {
			return "", err
		}
		a.history = append(a.history, Msg{Role: "assistant", Text: resp.Text, Calls: resp.Calls})
		a.logLLM(i, resp)
		shown := a.Redact.Text(resp.Text)
		if shown != "" {
			a.emit(Event{Kind: EvText, Text: shown})
		}
		if resp.Done || len(resp.Calls) == 0 {
			return shown, nil
		}
		for _, call := range resp.Calls {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			a.history = append(a.history, a.execCall(ctx, call))
		}
	}
	return "", fmt.Errorf("agent exceeded %d iterations", a.MaxIter)
}

// chat performs one model round, streaming text deltas when possible.
func (a *Agent) chat(ctx context.Context, req *Request) (*Response, error) {
	if s, ok := a.Provider.(Streamer); ok && a.Stream && a.OnEvent != nil {
		return s.Stream(ctx, req, func(chunk string) {
			a.emit(Event{Kind: EvDelta, Text: chunk})
		})
	}
	return a.Provider.Chat(ctx, req)
}

// logLLM records one model round — enough to replay the session.
func (a *Agent) logLLM(round int, resp *Response) {
	lg := a.Audit()
	if lg == nil {
		return
	}
	var calls []map[string]any
	for _, c := range resp.Calls {
		calls = append(calls, map[string]any{"id": c.ID, "name": c.Name, "args": a.Redact.Text(string(c.Args))})
	}
	_ = lg.Log(audit.KindLLM, a.profileName(), map[string]any{
		"round": round, "provider": a.Provider.Name(), "model": a.Model,
		"text": a.Redact.Text(resp.Text), "calls": calls, "done": resp.Done,
	})
}

func (a *Agent) toolErr(call Call, name, msg string) Msg {
	msg = a.Redact.Text(msg)
	a.emit(Event{Kind: EvToolResult, Tool: name, Err: msg})
	return Msg{Role: "tool", CallID: call.ID, ToolName: name, Text: msg, IsError: true}
}

func (a *Agent) execCall(ctx context.Context, call Call) Msg {
	name := toolkit.ResolveName(call.Name)
	var args map[string]any
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return a.toolErr(call, name, "bad args: "+err.Error())
	}
	t, ok := a.Reg.Get(name)
	if !ok {
		return a.toolErr(call, name, "no such tool: "+name)
	}
	shownArgs := a.Redact.Args(args)
	a.emit(Event{Kind: EvToolStart, Tool: name, Tier: t.Tier().String(), Args: shownArgs})
	if toolkit.IsLongRunning(t) {
		return a.toolErr(call, name, "refused: "+name+" is a long-running watcher and cannot finish inside an agent turn — suggest `cometcli "+strings.ReplaceAll(name, ".", " ")+"` to the operator")
	}
	if !a.Policy.Allows(t.Tier()) {
		msg := fmt.Sprintf("blocked: %s is a %s tool and the session is in readonly mode — recommend it to the operator instead", name, t.Tier())
		a.Audit().ToolSeen(a.profileName(), name, t.Tier().String(), shownArgs, nil, fmt.Errorf("%s", msg), "")
		return a.toolErr(call, name, msg)
	}
	a.calls++
	if a.MaxCalls > 0 && a.calls > a.MaxCalls {
		return a.toolErr(call, name, fmt.Sprintf("tool-call budget exhausted (%d) — stop calling tools and summarize findings so far", a.MaxCalls))
	}

	runCtx, cancel := a.toolCtx(ctx, t)
	res, err := t.Run(runCtx, args)
	runCtx.Close()
	cancel()

	var text string
	if err == nil {
		text = res.Text
		if text == "" && res.Data != nil {
			text = res.JSON()
		}
		limit := a.toolTrunc
		if limit == 0 {
			limit = 8192
		}
		text = ansiRe.ReplaceAllString(text, "") // colored logs waste tokens and confuse models
		if len(text) > limit {                   // bound context growth
			text = text[:limit] + "\n…[truncated]"
		}
		text = a.Redact.Text(text)
	}
	if lg := a.Audit(); lg != nil {
		var data map[string]any
		if res != nil {
			data = a.Redact.Args(res.Data)
		}
		lg.ToolSeen(a.profileName(), name, t.Tier().String(), shownArgs, data, err, text)
	}
	if err != nil {
		return a.toolErr(call, name, "error: "+err.Error())
	}
	a.emit(Event{Kind: EvToolResult, Tool: name, Tier: t.Tier().String(), Text: firstLine(text)})
	return Msg{Role: "tool", CallID: call.ID, ToolName: name, Text: text}
}

// toolCtx derives the per-call tool context: the turn's cancellation, a
// 90s deadline for short tools, the session audit logger, and the policy's
// approval threshold. Read-only sessions hard-deny any approval request
// that slips past filtering.
func (a *Agent) toolCtx(ctx context.Context, t toolkit.Tool) (*toolkit.Context, context.CancelFunc) {
	parent := &toolkit.Context{
		Context: ctx, Profile: a.Ctx.Profile, Cfg: a.Ctx.Cfg, Out: a.Ctx.Out,
		Audit: a.Audit(), Approver: a.Ctx.Approver,
	}
	var sub *toolkit.Context
	var cancel context.CancelFunc
	if toolkit.IsLongRunning(t) {
		sub, cancel = toolkit.WithCancel(parent)
	} else {
		sub, cancel = toolkit.WithDeadline(parent, 90*time.Second)
	}
	sub.AutoApproveBelow = a.Policy.AutoApproveBelow()
	if a.Policy.ReadOnly() {
		sub.Approver = toolkit.DenyApprover
	}
	return sub, cancel
}

// system returns the system prompt plus a cached, redacted live snapshot.
func (a *Agent) system() string {
	ttl := a.SnapshotTTL
	if ttl == 0 {
		ttl = 60 * time.Second
	}
	if a.snap == "" || time.Since(a.snapAt) > ttl {
		fn := a.SnapshotFn
		if fn == nil {
			fn = SnapshotText
		}
		a.snap = a.Redact.Text(fn(a.Ctx))
		a.snapAt = time.Now()
	}
	return a.Redact.Text(SystemPrompt(a.Ctx)) + a.snap
}

// History returns a copy of the conversation (for tests and replay).
func (a *Agent) History() []Msg { return append([]Msg(nil), a.history...) }

// ansiRe matches terminal escape sequences (colors, cursor moves).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
