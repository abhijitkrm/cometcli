package toolkit

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/client/comet"
	evmclient "github.com/abhijitkrm/cometcli/internal/client/evm"
	grpcclient "github.com/abhijitkrm/cometcli/internal/client/grpc"
	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

// Context carries everything a tool needs: the active profile, lazily
// constructed clients for each data plane, the audit log, output writer,
// and the approval function. Construct once per invocation.
type Context struct {
	context.Context
	Profile  *config.Profile
	Cfg      *config.Config
	Out      io.Writer
	Audit    *audit.Logger
	Approver Approver
	// AutoApprove tiers below this run without prompting (agent/CI use).
	AutoApproveBelow Tier
	// Session is agent-session state for general tools (cwd, reads).
	Session *Session
	// Rules are the session's allow/ask/deny permission patterns.
	Rules *Rules
	// ReadOnly refuses any action whose effective tier mutates.
	ReadOnly bool
	// AcceptEdits auto-approves file writes inside WorkRoot.
	AcceptEdits bool
	// WorkRoot anchors relative rule patterns and accept-edits scope.
	WorkRoot string
	// ToolName is the tool being run (for approval rule suggestions).
	ToolName string
	// Chooser and Secret let tools ask the operator to pick an option or
	// type a secret (e.g. which key signs; a container keyring password).
	Chooser Chooser
	Secret  SecretFunc
	// Progress, when set, receives status lines from long-running tools
	// (shown live by front-ends instead of printed per update).
	Progress func(string)
	// GRPCFallback is set once chain queries go through
	// endpoints.fallback_grpc because the node's own gRPC is down.
	GRPCFallback bool
	// HookDecision is a PreToolUse hook's verdict for this call:
	// "allow" skips the prompt, "ask" forces one ("" = no opinion).
	HookDecision string

	mu      sync.Mutex
	comet   *comet.Client
	cometEr error
	grpc    *grpcclient.Conn
	evm     *evmclient.Client
	evmErr  error
	host    host.Host
	hostErr error
	txb     *tx.Builder
	txErr   error
	signerB *tx.Builder // TxSigner's choice, reused for the context's life
}

var errNoProfile = fmt.Errorf("no active profile — run `cometcli profile add`")

// Comet lazily dials the CometBFT RPC endpoint.
func (c *Context) Comet() (*comet.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Profile == nil {
		return nil, errNoProfile
	}
	if c.comet == nil && c.cometEr == nil {
		ep := c.Profile.Endpoints.Comet
		if ep == "" {
			ep = "tcp://127.0.0.1:26657"
		}
		c.comet, c.cometEr = comet.New(ep)
	}
	return c.comet, c.cometEr
}

// GRPC lazily dials the Cosmos gRPC endpoint. When the node's own
// endpoint is down, chain queries go through endpoints.fallback_grpc
// (another node of the same chain), so a dead node can still be
// diagnosed. A failed dial isn't cached: a node restarted mid-wait is
// picked up on the next call.
func (c *Context) GRPC() (*grpcclient.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Profile == nil {
		return nil, errNoProfile
	}
	if c.grpc != nil {
		return c.grpc, nil
	}
	ep := c.Profile.Endpoints.GRPC
	if ep == "" {
		return nil, fmt.Errorf("profile %q has no grpc endpoint configured", c.Profile.Name)
	}
	conn, err := grpcclient.Dial(c.Context, ep)
	if err != nil && c.Profile.Endpoints.FallbackGRPC != "" {
		if fb, ferr := grpcclient.Dial(c.Context, c.Profile.Endpoints.FallbackGRPC); ferr == nil {
			conn, err, c.GRPCFallback = fb, nil, true
		}
	}
	c.grpc = conn
	return conn, err
}

// FallbackGRPC switches chain queries to endpoints.fallback_grpc — for a
// node that dialed fine but can't answer yet (just restarted, still
// loading state). It returns false when there is no fallback.
func (c *Context) FallbackGRPC() (*grpcclient.Conn, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Profile == nil || c.Profile.Endpoints.FallbackGRPC == "" {
		return nil, false
	}
	if c.GRPCFallback && c.grpc != nil {
		return c.grpc, true
	}
	fb, err := grpcclient.Dial(c.Context, c.Profile.Endpoints.FallbackGRPC)
	if err != nil {
		return nil, false
	}
	c.grpc, c.GRPCFallback = fb, true // the node's own conn is closed with the context's
	return fb, true
}

// EVM lazily connects the Ethereum JSON-RPC endpoint.
func (c *Context) EVM() (*evmclient.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Profile == nil {
		return nil, errNoProfile
	}
	if c.evm == nil && c.evmErr == nil {
		ep := c.Profile.Endpoints.EVM
		if ep == "" {
			c.evmErr = fmt.Errorf("profile %q has no evm (JSON-RPC) endpoint — expected on validator nodes", c.Profile.Name)
		} else {
			c.evm = evmclient.New(ep)
		}
	}
	return c.evm, c.evmErr
}

// Host lazily connects the host-plane transport (local exec or SSH).
func (c *Context) Host() (host.Host, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.host == nil && c.hostErr == nil {
		if c.Profile == nil {
			// no node profile (general mode): the operator's own machine
			c.host = &host.Local{}
		} else {
			c.host, c.hostErr = host.Connect(c.Context, c.Profile)
		}
	}
	return c.host, c.hostErr
}

// SetHost overrides the host transport — used by tests and by tooling that
// already resolved a connection.
func (c *Context) SetHost(h host.Host) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.host, c.hostErr = h, nil
}

// Tx lazily builds the transaction pipeline (keyring + grpc).
func (c *Context) Tx() (*tx.Builder, error) {
	// Fail fast before any dialing — Profile is immutable post-NewCtx.
	if c.Profile == nil {
		return nil, errNoProfile
	}
	if c.Profile.Signer.Key == "" && (c.signerContainer() == "" || c.containerKey() == "") {
		return nil, fmt.Errorf("profile %q has no signer.key — run `cometcli keys add` and set it, or configure signer.container_key", c.Profile.Name)
	}
	// GRPC() manages its own locking — call it before taking c.mu or we
	// self-deadlock on the non-reentrant mutex.
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.txb == nil && c.txErr == nil {
		c.txb, c.txErr = tx.NewBuilder(c.Context, g, c.Profile, c.Audit)
		if c.txErr == nil {
			if why := c.wrongAccount(c.txb); why != "" {
				c.txb, c.txErr = nil, fmt.Errorf("%s", why)
			}
		}
		if c.txErr != nil && c.signerContainer() != "" && c.containerKey() != "" {
			// no local key: the node container's key answers for the address
			c.mu.Unlock()
			b, err := c.containerBuilder()
			c.mu.Lock()
			c.txb, c.txErr = b, err
		}
	}
	return c.txb, c.txErr
}

// WithDeadline returns a fresh Context sharing c's fields but with a derived
// ctx carrying the given timeout. The parent is c's embedded std ctx — never
// c itself, or Value() would recurse through a child that points back to its
// parent. A fresh Context also avoids copying the mutex (go vet copylocks).
// The caller must invoke cancel and close the sub-context's own lazy clients.
func WithDeadline(c *Context, timeout time.Duration) (*Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(c.Context, timeout)
	return &Context{
		Context: ctx, Profile: c.Profile, Cfg: c.Cfg, Out: c.Out,
		Audit: c.Audit, Approver: c.Approver, AutoApproveBelow: c.AutoApproveBelow,
		Session: c.Session, Rules: c.Rules, ReadOnly: c.ReadOnly,
		AcceptEdits: c.AcceptEdits, WorkRoot: c.WorkRoot, HookDecision: c.HookDecision, ToolName: c.ToolName,
		Chooser: c.Chooser, Secret: c.Secret, Progress: c.Progress,
	}, cancel
}

// WithCancel is WithDeadline without a timeout — for long-running tools
// that still need their own lazy clients and a cancel hook.
func WithCancel(c *Context) (*Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(c.Context)
	return &Context{
		Context: ctx, Profile: c.Profile, Cfg: c.Cfg, Out: c.Out,
		Audit: c.Audit, Approver: c.Approver, AutoApproveBelow: c.AutoApproveBelow,
		Session: c.Session, Rules: c.Rules, ReadOnly: c.ReadOnly,
		AcceptEdits: c.AcceptEdits, WorkRoot: c.WorkRoot, HookDecision: c.HookDecision, ToolName: c.ToolName,
		Chooser: c.Chooser, Secret: c.Secret, Progress: c.Progress,
	}, cancel
}

// LogShell records a host command in the audit log.
func (c *Context) LogShell(cmd string, code int) {
	if c.Audit != nil {
		name := ""
		if c.Profile != nil {
			name = c.Profile.Name
		}
		c.Audit.Shell(name, cmd, code)
	}
}

// Approve runs the approval gate honoring AutoApproveBelow.
func (c *Context) Approve(prompt string, tier Tier, detail map[string]any) error {
	if tier < c.AutoApproveBelow {
		return nil
	}
	return RequireApproval(c, prompt, tier, detail)
}

// Gate describes an action whose risk is decided per call (a shell
// command, a file path) rather than by the tool's static tier.
type Gate struct {
	Request
	Tier   Tier
	Prompt string
	Detail map[string]any
	// Forbidden, when set, refuses the action in every mode and despite
	// any rule (consensus keys, state resets): the reason is shown.
	Forbidden string
	// InRoot marks a file edit inside WorkRoot (auto under accept-edits).
	InRoot bool
	// AskByDefault prompts when no rule matches even though the tier is
	// read-only (web fetches: the URL itself can carry data out). Bypass
	// mode still auto-approves.
	AskByDefault bool
}

// Check runs the permission pipeline for a dynamic action: forbidden →
// read-only mode → deny/ask/allow rules → accept-edits → tier policy.
// On-chain actions always reach the human, whatever the rules say.
func (c *Context) Check(g Gate) error {
	if g.Forbidden != "" {
		return fmt.Errorf("refused: %s", g.Forbidden)
	}
	if c.ReadOnly && g.Tier >= TierLocalChange {
		return fmt.Errorf("blocked: this is a %s action and the session is read-only — recommend it to the operator instead", g.Tier)
	}
	if g.Root == "" {
		g.Root = c.WorkRoot
	}
	if g.Tier < TierOnChain {
		if g.Detail == nil {
			g.Detail = map[string]any{}
		}
		if r := SuggestRule(g.Request); r != "" {
			g.Detail[RuleHint] = r
		}
	}
	d, rule := c.Rules.Decide(g.Request)
	switch {
	case d == DecideDeny:
		return fmt.Errorf("denied by permission rule %s", rule)
	case d == DecideAsk || g.Tier == TierOnChain || c.HookDecision == "ask":
		return RequireApproval(c, g.Prompt, g.Tier, g.Detail)
	case d == DecideAllow || c.HookDecision == "allow":
		return nil
	case c.AcceptEdits && g.InRoot && g.Tier == TierLocalChange:
		return nil
	case g.AskByDefault && c.AutoApproveBelow <= TierLocalChange:
		return RequireApproval(c, g.Prompt, g.Tier, g.Detail)
	}
	return c.Approve(g.Prompt, g.Tier, g.Detail)
}

// Close releases lazy clients. Audit is deliberately not closed here:
// derived contexts (WithDeadline) share the parent's logger, and closing
// it per-call would kill auditing for the rest of the session. The owner
// that opened the logger closes it at shutdown.
func (c *Context) Close() {
	if c.grpc != nil {
		c.grpc.Close()
	}
	if h, ok := c.host.(io.Closer); ok && c.hostErr == nil {
		h.Close()
	}
}

// Derive returns a Context bound to ctx that shares c's already-dialed
// clients (comet, grpc, evm, host) — for fanning out concurrent calls with
// their own deadlines. Never Close a derived Context: c owns the clients.
func (c *Context) Derive(ctx context.Context) *Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &Context{
		Context: ctx, Profile: c.Profile, Cfg: c.Cfg, Out: c.Out,
		Audit: c.Audit, Approver: c.Approver, AutoApproveBelow: c.AutoApproveBelow,
		Session: c.Session, Rules: c.Rules, ReadOnly: c.ReadOnly,
		AcceptEdits: c.AcceptEdits, WorkRoot: c.WorkRoot, HookDecision: c.HookDecision, ToolName: c.ToolName,
		Chooser: c.Chooser, Secret: c.Secret, Progress: c.Progress,
		comet: c.comet, cometEr: c.cometEr, grpc: c.grpc, GRPCFallback: c.GRPCFallback,
		evm: c.evm, evmErr: c.evmErr, host: c.host, hostErr: c.hostErr,
	}
}
