package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

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
	// MaxTokens caps each model round's output (0 = provider default).
	MaxTokens int
	// Effort is the reasoning depth passed to the provider ("" = default).
	Effort string
	// CompactAt is the prompt size, in tokens, at which history is
	// summarized at the next turn boundary (0 = derived from the model's
	// context window).
	CompactAt int
	// Persist saves the session after every turn so it can be resumed.
	Persist bool
	// Rules are the session's permission rules (allow/ask/deny).
	Rules *toolkit.Rules
	// Tools is shell/file state shared by general tools (working
	// directory, files read).
	Tools *toolkit.Session
	// WorkRoot anchors relative permission patterns and accept-edits.
	WorkRoot string

	conf      config.AgentConf
	id        string
	created   time.Time
	audit     *audit.Logger
	history   []Msg
	sys       string // system prompt, frozen per session (cache prefix)
	toolSig   string // tool names last advertised
	carry     string // compaction summary awaiting the next user turn
	snap      string
	snapAt    time.Time
	calls     int
	toolTrunc int
	lastIn    int   // prompt tokens of the latest round (0 = unknown)
	total     Usage // session token totals
	todos     []Todo
	loaded    map[string]bool // tools loaded with tool_search
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
		Provider:  prov,
		Model:     def(ac.Model, ""),
		Reg:       reg,
		Ctx:       c,
		MaxIter:   def0(ac.MaxTurns, 50),
		Policy:    pol,
		Redact:    RedactorFor(c.Profile),
		Stream:    !ac.NoStream,
		MaxTokens: ac.MaxTokens,
		Effort:    ac.Effort,
		CompactAt: ac.CompactAt,
		conf:      ac,
	}
	if err := ValidEffort(a.Effort); err != nil {
		return nil, err
	}
	if a.Rules, err = toolkit.NewRules(ac.Permissions.Allow, ac.Permissions.Ask, ac.Permissions.Deny); err != nil {
		return nil, err
	}
	a.WorkRoot, _ = os.Getwd()
	a.Tools = toolkit.NewSession("")
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
	case *gemini:
		return t.model
	}
	return ""
}

func def0(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

// ValidEffort accepts "" or one of the neutral effort levels.
func ValidEffort(e string) error {
	switch e {
	case "", "minimal", "low", "medium", "high", "xhigh", "max":
		return nil
	}
	return fmt.Errorf("unknown effort %q — want low | medium | high | xhigh | max", e)
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
	a.snap, a.sys, a.toolSig, a.carry = "", "", "", ""
	a.lastIn, a.total = 0, Usage{}
	a.created = time.Time{}
	a.todos, a.loaded = nil, nil
	a.Tools = toolkit.NewSession("")
}

// Usage returns the session's accumulated token usage and the size of
// the latest prompt (0 when the provider doesn't report it).
func (a *Agent) Usage() (total Usage, lastPrompt int) { return a.total, a.lastIn }

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
		if !a.Policy.Allows(t.Tier()) && !toolkit.IsDynamic(t) {
			continue
		}
		if !a.advertised(t) {
			continue
		}
		out = append(out, ToolDef{
			Name:   toolFnName(t.Name()),
			Desc:   fmt.Sprintf("[%s] %s", t.Tier(), t.Desc()),
			Schema: t.Schema(),
		})
	}
	out = append(out, todoToolDef)
	if a.deferred() {
		out = append(out, toolSearchDef)
	}
	return out
}

func toolFnName(n string) string { return strings.ReplaceAll(n, ".", "__") }

// maxContinuations bounds automatic "continue" rounds after a reply is
// cut off by the output-token limit.
const maxContinuations = 3

// Run processes one user turn, executing tools until the model finishes.
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	out, err := a.run(ctx, input)
	if a.Persist {
		if serr := a.Save(); serr != nil && err == nil {
			a.emit(Event{Kind: EvNotice, Text: "session not saved: " + serr.Error()})
		}
	}
	return out, err
}

func (a *Agent) run(ctx context.Context, input string) (string, error) {
	a.calls = 0
	if a.created.IsZero() {
		a.created = time.Now()
	}
	// compact only at a turn boundary — never inside a tool round
	if a.needsCompact() {
		if err := a.Compact(ctx, ""); err != nil {
			a.emit(Event{Kind: EvNotice, Text: "auto-compact failed: " + err.Error()})
		}
	}
	input = a.Redact.Text(input)
	if lg := a.Audit(); lg != nil {
		_ = lg.Log(audit.KindPrompt, a.profileName(), map[string]any{"text": input})
	}
	a.history = append(a.history, Msg{Role: "user", Text: a.turnPrefix() + input})

	var texts []string
	conts, compacted := 0, false
	for i := 0; i < a.MaxIter; i++ {
		req := a.request()
		resp, err := a.chat(ctx, req)
		if err != nil && !compacted && isContextOverflow(err) {
			// the prompt outgrew the window mid-turn: summarize everything
			// so far and carry on from the summary
			compacted = true
			a.emit(Event{Kind: EvNotice, Text: "context window full — compacting and continuing"})
			if cerr := a.compactNow(ctx, "", true); cerr != nil {
				return "", fmt.Errorf("%w (compaction failed: %v)", err, cerr)
			}
			i--
			continue
		}
		if err != nil {
			return "", err
		}
		a.total.Add(resp.Usage)
		if resp.Usage.Input > 0 {
			a.lastIn = resp.Usage.Input + resp.Usage.Output
		}
		a.history = append(a.history, Msg{
			Role: "assistant", Text: resp.Text, Calls: resp.Calls,
			RawSteps: resp.RawSteps, RawProvider: rawProvider(a.Provider, resp),
		})
		a.logLLM(i, resp)
		shown := a.Redact.Text(resp.Text)
		if shown != "" {
			a.emit(Event{Kind: EvText, Text: shown})
			texts = append(texts, shown)
		}
		switch {
		case resp.Stop == StopRefusal:
			msg := "the model declined this request"
			if resp.StopDetail != "" {
				msg += " (" + resp.StopDetail + ")"
			}
			a.emit(Event{Kind: EvNotice, Text: msg})
			return strings.Join(texts, "\n"), fmt.Errorf("%s", msg)
		case resp.Stop == StopMaxTokens && len(resp.Calls) == 0:
			if conts >= maxContinuations {
				a.emit(Event{Kind: EvNotice, Text: "reply truncated at the output-token limit"})
				return strings.Join(texts, ""), nil
			}
			conts++
			a.emit(Event{Kind: EvNotice, Text: "reply hit the output-token limit — continuing"})
			a.history = append(a.history, Msg{Role: "user", Text: "Your reply was cut off by the output limit. Continue exactly where you stopped, without repeating anything."})
			continue
		}
		if resp.Done || len(resp.Calls) == 0 {
			if conts > 0 {
				return strings.Join(texts[len(texts)-conts-1:], ""), nil
			}
			return shown, nil
		}
		for _, call := range resp.Calls {
			if err := ctx.Err(); err != nil {
				// answer every outstanding call so the history stays valid
				a.history = append(a.history, Msg{Role: "tool", CallID: call.ID, ToolName: toolkit.ResolveName(call.Name), Text: "cancelled by operator", IsError: true})
				continue
			}
			a.history = append(a.history, a.execCall(ctx, call))
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("agent exceeded %d iterations", a.MaxIter)
}

// rawProvider tags raw output with the provider that produced it, so it
// is only ever replayed to that provider.
func rawProvider(p Provider, resp *Response) string {
	if len(resp.RawSteps) == 0 {
		return ""
	}
	return p.Name()
}

// request builds one model round. The system prompt is frozen for the
// session and the tool set is stable, so providers can reuse the cached
// prefix; when the tool set does change (e.g. /mode readonly), replayed
// thinking bound to the old prefix is dropped first.
func (a *Agent) request() *Request {
	tools := a.toolDefs()
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	sig := strings.Join(names, ",")
	if a.toolSig != "" && sig != a.toolSig {
		a.dropBoundRaw()
	}
	a.toolSig = sig
	return &Request{
		Model:    a.Model,
		System:   a.system(),
		Messages: a.history,
		Tools:    tools,
		MaxTok:   a.MaxTokens,
		Effort:   a.Effort,
		OnThinking: func(t string) {
			if a.OnEvent != nil && t != "" {
				a.emit(Event{Kind: EvThinking, Text: a.Redact.Text(t)})
			}
		},
	}
}

// dropBoundRaw discards raw assistant output whose validity is tied to
// the exact request prefix (Anthropic thinking signatures). Those turns
// fall back to their plain text + tool calls.
func (a *Agent) dropBoundRaw() {
	for i := range a.history {
		if a.history[i].RawProvider == "anthropic" {
			a.history[i].RawSteps, a.history[i].RawProvider = nil, ""
		}
	}
}

// chat performs one model round, streaming text deltas when possible.
// Retries surface as notices so a backoff never looks like a hang.
func (a *Agent) chat(ctx context.Context, req *Request) (*Response, error) {
	ctx = WithRetryNotice(ctx, func(attempt int, wait time.Duration, reason string) {
		a.emit(Event{Kind: EvNotice, Text: fmt.Sprintf("%s: %s — retrying in %s (attempt %d)", a.Provider.Name(), reason, wait.Round(100*time.Millisecond), attempt+1)})
	})
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
	switch name {
	case todoToolName:
		return a.todoWrite(call, args)
	case toolSearchName:
		return a.toolSearch(call, args)
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
	if !a.Policy.Allows(t.Tier()) && !toolkit.IsDynamic(t) {
		msg := fmt.Sprintf("blocked: %s is a %s tool and the session is in readonly mode — recommend it to the operator instead", name, t.Tier())
		a.Audit().ToolSeen(a.profileName(), name, t.Tier().String(), shownArgs, nil, fmt.Errorf("%s", msg), "")
		return a.toolErr(call, name, msg)
	}
	a.calls++
	if a.MaxCalls > 0 && a.calls > a.MaxCalls {
		return a.toolErr(call, name, fmt.Sprintf("tool-call budget exhausted (%d) — stop calling tools and summarize findings so far", a.MaxCalls))
	}

	runCtx, cancel := a.toolCtx(ctx, t, args)
	var res *toolkit.Result
	err := a.ruleGate(runCtx, t, shownArgs)
	if err == nil {
		res, err = t.Run(runCtx, args)
	}
	runCtx.Close()
	cancel()

	var text string
	if err == nil {
		text = res.Text
		if text == "" && res.Data != nil {
			text = res.JSON()
		}
		limit := a.toolTrunc
		if ol, ok := t.(toolkit.OutputLimiter); ok && limit == 0 {
			limit = ol.OutputLimit()
		}
		if limit == 0 {
			limit = 16_000
		}
		text = ansiRe.ReplaceAllString(text, "") // colored logs waste tokens and confuse models
		text = clip(text, limit)                 // bound context growth
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
func (a *Agent) toolCtx(ctx context.Context, t toolkit.Tool, args toolkit.Args) (*toolkit.Context, context.CancelFunc) {
	parent := &toolkit.Context{
		Context: ctx, Profile: a.Ctx.Profile, Cfg: a.Ctx.Cfg, Out: a.Ctx.Out,
		Audit: a.Audit(), Approver: a.Ctx.Approver,
		Session: a.Tools, Rules: a.Rules, WorkRoot: a.WorkRoot,
		ReadOnly: a.Policy.ReadOnly(), AcceptEdits: a.Policy.AcceptEdits(),
	}
	var sub *toolkit.Context
	var cancel context.CancelFunc
	switch {
	case toolkit.IsLongRunning(t):
		sub, cancel = toolkit.WithCancel(parent)
	default:
		d := 90 * time.Second
		if to, ok := t.(toolkit.Timeouter); ok {
			d = to.Timeout(args)
		}
		sub, cancel = toolkit.WithDeadline(parent, d)
	}
	sub.AutoApproveBelow = a.Policy.AutoApproveBelow()
	if a.Policy.ReadOnly() {
		sub.Approver = toolkit.DenyApprover
	}
	return sub, cancel
}

// ruleGate applies name-level permission rules to registry tools that
// don't gate themselves (general tools match rules on their own specs):
// deny refuses, ask confirms first, allow skips the tool's local-change
// prompt. Transactions keep their own approval regardless.
func (a *Agent) ruleGate(c *toolkit.Context, t toolkit.Tool, shownArgs map[string]any) error {
	if toolkit.IsAgentOnly(t) {
		return nil
	}
	d, rule := a.Rules.Decide(toolkit.Request{Tool: t.Name()})
	switch d {
	case toolkit.DecideDeny:
		return fmt.Errorf("denied by permission rule %s", rule)
	case toolkit.DecideAsk:
		return toolkit.RequireApproval(c, fmt.Sprintf("run %s %s", t.Name(), CompactArgs(shownArgs)), t.Tier(), map[string]any{"rule": rule})
	case toolkit.DecideAllow:
		c.AutoApproveBelow = toolkit.TierOnChain
	}
	return nil
}

// system returns the session's system prompt: instructions plus the live
// snapshot taken when the session started. It is frozen until Reset or
// Compact — rebuilding it every request would defeat prompt caching and
// invalidate replayed thinking. Later snapshots ride on user turns.
func (a *Agent) system() string {
	if a.sys == "" {
		a.sys = a.Redact.Text(SystemPrompt(a.Ctx)) + a.toolCatalog() + a.snapshot()
	}
	return a.sys
}

// snapshot probes the live node digest and records when.
func (a *Agent) snapshot() string {
	fn := a.SnapshotFn
	if fn == nil {
		fn = SnapshotText
	}
	a.snap = a.Redact.Text(fn(a.Ctx))
	a.snapAt = time.Now()
	return a.snap
}

// turnPrefix is prepended to a user turn: a pending compaction summary,
// and a refreshed live snapshot once the previous one is older than
// SnapshotTTL. Appending keeps the request prefix byte-stable.
func (a *Agent) turnPrefix() string {
	var b strings.Builder
	if a.carry != "" {
		b.WriteString(a.carry)
		b.WriteString("\n\n")
		a.carry = ""
	}
	if a.sys == "" {
		a.system() // first turn: the snapshot goes in the system prompt
		return b.String()
	}
	ttl := a.SnapshotTTL
	if ttl == 0 {
		ttl = 60 * time.Second
	}
	if time.Since(a.snapAt) > ttl {
		b.WriteString("<refreshed-snapshot>\n")
		b.WriteString(strings.TrimSpace(a.snapshot()))
		b.WriteString("\n</refreshed-snapshot>\n\n")
	}
	return b.String()
}

// History returns a copy of the conversation (for tests and replay).
func (a *Agent) History() []Msg { return append([]Msg(nil), a.history...) }

// ansiRe matches terminal escape sequences (colors, cursor moves).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// clip bounds s to about limit bytes, keeping the head and the tail (log
// tails usually hold the error) and cutting only at UTF-8 boundaries.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	head, tail := limit*2/3, limit/3
	h := safeCut(s, head)
	t := s[len(s)-tail:]
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	return fmt.Sprintf("%s\n…[%d bytes truncated]…\n%s", h, len(s)-len(h)-len(t), t)
}

// safeCut returns the longest prefix of s within n bytes that ends on a
// rune boundary.
func safeCut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		return safeCut(s, 120) + "…"
	}
	return s
}
