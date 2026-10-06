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
	// HookDecision is a PreToolUse hook's verdict for this call:
	// "allow" skips the prompt, "ask" forces one ("" = no opinion).
	HookDecision string

	mu      sync.Mutex
	comet   *comet.Client
	cometEr error
	grpc    *grpcclient.Conn
	grpcErr error
	evm     *evmclient.Client
	evmErr  error
	host    host.Host
	hostErr error
	txb     *tx.Builder
	txErr   error
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

// GRPC lazily dials the Cosmos gRPC endpoint.
func (c *Context) GRPC() (*grpcclient.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Profile == nil {
		return nil, errNoProfile
	}
	if c.grpc == nil && c.grpcErr == nil {
		ep := c.Profile.Endpoints.GRPC
		if ep == "" {
			c.grpcErr = fmt.Errorf("profile %q has no grpc endpoint configured", c.Profile.Name)
		} else {
			c.grpc, c.grpcErr = grpcclient.Dial(c.Context, ep)
		}
	}
	return c.grpc, c.grpcErr
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
	if c.Profile.Signer.Key == "" {
		return nil, fmt.Errorf("profile %q has no signer.key — run `cometcli keys add` and set it", c.Profile.Name)
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
