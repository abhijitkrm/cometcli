// Package scenario runs whole incidents end to end: the real agent loop,
// the real tool registry and the incident procedures, against a simulated
// chain (fakenode gRPC + fake CometBFT RPC) that evolves while the agent
// works. The model is scripted: it only reacts to what the tools return,
// which proves the tools surface enough to drive the recovery.
package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools"
	"github.com/abhijitkrm/cometcli/internal/tools/waittool"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

func TestMain(m *testing.M) {
	fakenode.MaybeRunFakeDocker()
	waittool.MinInterval = 10 * time.Millisecond
	tx.ConfirmTimeout, tx.ConfirmPoll = time.Second, 20*time.Millisecond
	os.Exit(m.Run())
}

const jailLogs = `E[2026-10-06|21:41:13] Stopping peer for error err="read tcp 172.18.0.3:26656: i/o timeout" module=p2p
E[2026-10-06|21:41:40] dial tcp 10.0.0.2:26656: connect: connection refused module=p2p
INF slashing and jailing validator due to liveness fault height=162050 jailed_until=2026-10-06T21:54:31Z module=x/slashing
`

// scripted plays the model: each response depends only on the last
// message, the way the /recover-jail procedure tells a model to act.
type scripted struct {
	t         *testing.T
	syncStart chan struct{} // closed when the model starts waiting for sync
	mu    sync.Mutex
	calls []string
	seen  map[string]string // tool → last result text
}

func call(name string, args map[string]any) *agent.Response {
	b, _ := json.Marshal(args)
	return &agent.Response{Calls: []agent.Call{{ID: fmt.Sprintf("c%d", time.Now().UnixNano()), Name: name, Args: b}}}
}

func (s *scripted) Name() string { return "scripted" }
func (s *scripted) Chat(_ context.Context, r *agent.Request) (*agent.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := r.Messages[len(r.Messages)-1]
	if last.Role == "user" {
		if !strings.Contains(last.Text, "val.jail-check") {
			s.t.Errorf("first prompt isn't the recovery procedure: %.200s", last.Text)
		}
		s.calls = append(s.calls, "val.jail-check")
		return call("val__jail-check", nil), nil
	}
	s.seen[last.ToolName] = last.Text
	next := func(name string, args map[string]any) (*agent.Response, error) {
		s.calls = append(s.calls, name)
		return call(strings.ReplaceAll(name, ".", "__"), args), nil
	}
	fast := map[string]any{"interval": 0.02, "timeout": 20}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range fast {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	switch last.ToolName {
	case "val.jail-check":
		if strings.Contains(last.Text, "TOMBSTONED") {
			return &agent.Response{Text: "tombstoned — stopping", Done: true}, nil
		}
		if strings.Contains(last.Text, "node not synced") {
			close(s.syncStart) // the simulated node starts catching up now
			return next("wait.until", with("condition", "synced"))
		}
		return next("wait.until", with("condition", "unjailable"))
	case "wait.until":
		switch {
		case strings.Contains(last.Text, "✓ synced"):
			return next("wait.until", with("condition", "unjailable"))
		case strings.Contains(last.Text, "✓ unjailable"):
			return next("tool_search", map[string]any{"query": "select:val.unjail"})
		case strings.Contains(last.Text, "✓ in-consensus"):
			return next("val.consensus", map[string]any{"window": 20})
		}
		return &agent.Response{Text: "wait failed: " + last.Text, Done: true}, nil
	case "tool_search":
		return next("val.unjail", nil)
	case "val.unjail":
		switch {
		case strings.Contains(last.Text, "tx hash"):
			return next("wait.until", with("condition", "in-consensus", "window", 20, "min_signed_pct", 95))
		case strings.Contains(last.Text, "raise the gas price"):
			return next("val.unjail", map[string]any{"gas-price": "2000000000"})
		}
		return &agent.Response{Text: "unjail failed: " + last.Text, Done: true}, nil
	case "val.consensus":
		return &agent.Response{Text: "Recovered. " + strings.ReplaceAll(last.Text, "\n", "; "), Done: true}, nil
	}
	return &agent.Response{Text: "unexpected tool " + last.ToolName, Done: true}, nil
}

func TestJailRecoveryEndToEnd(t *testing.T) {
	n := fakenode.Start(t, "primium-1")
	chain := fakenode.StartComet(t)
	p := n.Profile(t, "eth_secp256k1")
	fakenode.WithComet(p, chain)
	p.Service = config.Service{Type: "docker", Unit: "primium-validator0"}
	p.Home = t.TempDir()
	fakenode.InstallFakeDocker(t, "test", "")
	fakenode.SetNodeLogs(t, jailLogs, "status=running restarts=2 oom_killed=false")

	c := &toolkit.Context{Context: context.Background(), Profile: p, Cfg: &config.Config{Profiles: map[string]*config.Profile{p.Name: p}}}
	c.SetHost(&host.Local{})
	t.Cleanup(c.Close)
	tb, err := c.Tx()
	if err != nil {
		t.Fatal(err)
	}
	valoper, _ := tb.ValAddress()
	p.Metadata["valoper"] = valoper

	// the incident: jailed for downtime, jail period ends shortly in
	// chain time, the node is behind and syncing, and the node's minimum
	// gas price was raised so the first unjail is rejected for its fee
	n.Set(func(n *fakenode.Node) {
		n.Validator = &stakingv1beta1.Validator{OperatorAddress: valoper, ConsensusPubkey: fakenode.ConsPubAny(),
			Jailed: true, Status: stakingv1beta1.BondStatus_BOND_STATUS_UNBONDING, MinSelfDelegation: "1"}
		n.SigningInfo = &slashingv1beta1.ValidatorSigningInfo{JailedUntil: timestamppb.New(time.Now().Add(20 * time.Second))}
		n.CheckFailOnce = &struct {
			Code uint32
			Log  string
		}{13, "insufficient fee; got: 140000000000000adex required: 280000000000000adex"}
		n.OnCommit = func(n *fakenode.Node, t fakenode.Tx) {
			if t.Msgs[0].TypeUrl == "/cosmos.slashing.v1beta1.MsgUnjail" {
				n.Validator.Jailed = false
				n.Validator.Status = stakingv1beta1.BondStatus_BOND_STATUS_BONDED
				chain.Set(func(c *fakenode.Comet) {
					from := c.Height + 2
					c.InSet, c.Signs = true, func(h int64) bool { return h >= from }
				})
			}
		}
	})
	behind := time.Now().Add(-15 * time.Minute)
	chain.Set(func(c *fakenode.Comet) { c.InSet, c.CatchingUp, c.LatestTimeOverride = false, true, &behind })
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	syncStart := make(chan struct{})
	go func() { // the chain keeps producing blocks; the node catches up
		select { // stays behind until the agent is waiting for it
		case <-stop:
			return
		case <-syncStart:
		}
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(15 * time.Millisecond):
			}
			chain.Set(func(c *fakenode.Comet) {
				c.Height += 2
				if c.CatchingUp {
					lt := behind.Add(time.Duration(i) * 30 * time.Second)
					c.LatestTimeOverride = &lt
					if i >= 20 {
						c.CatchingUp, c.LatestTimeOverride = false, nil
					}
				}
			})
		}
	}()

	reg := toolkit.NewRegistry()
	tools.RegisterAll(reg)
	p.Agent = config.AgentConf{Provider: "openai-compat", BaseURL: "http://127.0.0.1:1"}
	a, err := agent.New(c, reg)
	if err != nil {
		t.Fatal(err)
	}
	model := &scripted{t: t, seen: map[string]string{}, syncStart: syncStart}
	a.Provider = model
	var txApprovals, signerChoices int
	a.Ctx.Chooser = func(_ *toolkit.Context, _ string, opts []string) (int, error) {
		signerChoices++
		return 0, nil // cometcli keyring
	}
	a.Ctx.Approver = func(_ *toolkit.Context, _ string, tier toolkit.Tier, _ map[string]any) (bool, error) {
		if tier == toolkit.TierOnChain {
			txApprovals++
		}
		return true, nil
	}

	cmd, err := agent.RunCommand(a, a.Ctx, reg, "/recover-jail peers were flapping overnight")
	if err != nil || !strings.Contains(cmd.Prompt, "peers were flapping overnight") {
		t.Fatalf("/recover-jail: %v %v", cmd, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := a.Run(ctx, cmd.Prompt)
	if err != nil {
		t.Fatal(err)
	}

	want := "val.jail-check,wait.until,wait.until,tool_search,val.unjail,val.unjail,wait.until,val.consensus"
	if got := strings.Join(model.calls, ","); got != want {
		t.Fatalf("procedure:\n got  %s\n want %s\nlast results: %v", got, want, model.seen)
	}
	jc := model.seen["val.jail-check"]
	for _, w := range []string{"reason:    downtime", "liveness fault", "peer loss / network", "restarts=2", "node not synced"} {
		if !strings.Contains(jc, w) {
			t.Errorf("jail-check missing %q:\n%s", w, jc)
		}
	}
	bs, rej, _ := n.Snapshot()
	if len(bs) != 1 || len(rej) != 1 || !strings.Contains(rej[0], "insufficient fee") || txApprovals != 2 {
		t.Fatalf("broadcasts=%d rejected=%v approvals=%d", len(bs), rej, txApprovals)
	}
	if fee := bs[0].Fee.Amount[0].Amount; fee == "" || len(fee) < 15 {
		t.Fatalf("retry fee = %v", bs[0].Fee)
	}
	if signerChoices != 2 {
		t.Fatalf("signer asked %d times, want once per unjail attempt", signerChoices)
	}
		if !strings.Contains(out, "participating in consensus") || strings.Contains(out, "NOT participating") {
		t.Fatalf("final: %s", out)
	}
	t.Logf("final answer: %s", out)
}
