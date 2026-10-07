package val

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

const nodeLogs = `I[2026-10-06|21:40:02] executed block height=161990 module=state
E[2026-10-06|21:41:13] Stopping peer for error err="read tcp 172.18.0.3:26656: i/o timeout" module=p2p
E[2026-10-06|21:41:15] Stopping peer for error err="EOF" module=p2p
E[2026-10-06|21:41:40] dial tcp 10.0.0.2:26656: connect: connection refused module=p2p
E[2026-10-06|21:43:02] Failed to sign vote err="error signing vote: remote signer timed out" module=consensus
INF slashing and jailing validator due to liveness fault height=162050 jailed_until=2026-10-06T21:54:31Z min_height=152050 module=x/slashing threshold=5000
INF validator jailed module=x/staking validator=cosmosvalcons1someoneelse000
`

func TestJailCheckExplainsAndBlocks(t *testing.T) {
	n, c, valoper, chain := setupChain(t)
	c.Profile.Service = config.Service{Type: "docker", Unit: "primium-validator0"}
	c.Profile.Home = t.TempDir()
	c.SetHost(&host.Local{})
	fakenode.InstallFakeDocker(t, "test", "")
	fakenode.SetNodeLogs(t, nodeLogs, "status=running restarts=3 oom_killed=false started=2026-10-06T21:50:10Z")
	jail(n, time.Now().Add(8*time.Minute))
	chain.Set(func(c *fakenode.Comet) { c.CatchingUp = true; c.InSet = false })

	res, err := (jailCheckTool{}).Run(c, toolkit.Args{"validator": valoper})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "someoneelse") {
		t.Error("another validator's jailing shown as evidence")
	}
	for _, want := range []string{"reason:    downtime", "liveness fault height=162050", "peer loss / network ×3",
		"signer / privval", "remote signer timed out", "restarts=3", "jail period runs until", "node not synced", "wait.until condition=synced"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("missing %q in:\n%s", want, res.Text)
		}
	}
	if res.Data["can_unjail_now"] != false {
		t.Fatal("reported unjailable while blocked")
	}
	// the unjail tool refuses with the same blockers, nothing broadcast
	if _, err := (unjailTool{}).Run(c, toolkit.Args{}); err == nil || !strings.Contains(err.Error(), "not unjailing yet") {
		t.Fatalf("unjail err = %v", err)
	}
	if bs, _, _ := n.Snapshot(); len(bs) != 0 {
		t.Fatal("blocked unjail broadcast")
	}

	// tombstoned: never unjailable, says why
	n.Set(func(n *fakenode.Node) { n.SigningInfo.Tombstoned = true })
	res, _ = (jailCheckTool{}).Run(c, toolkit.Args{"validator": valoper})
	if !strings.Contains(res.Text, "TOMBSTONED") || !strings.Contains(res.Text, "can never be unjailed") {
		t.Fatalf("tombstone:\n%s", res.Text)
	}
}

func TestJailCheckSelfDelegationAndFees(t *testing.T) {
	n, c, valoper, _ := setupChain(t)
	jail(n, time.Now().Add(-time.Minute))
	n.Set(func(n *fakenode.Node) {
		n.Validator.MinSelfDelegation = "2000000000000000000"
		n.Balance = "10"
	})
	res, err := (jailCheckTool{}).Run(c, toolkit.Args{"validator": valoper})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "below min_self_delegation") || !strings.Contains(res.Text, "fund the operator account") {
		t.Fatalf("blockers:\n%s", res.Text)
	}
}

func TestConsensusVerdict(t *testing.T) {
	n, c, valoper, chain := setupChain(t)
	res, err := (consensusTool{}).Run(c, toolkit.Args{"validator": valoper, "window": float64(10)})
	if err != nil || res.Data["in_consensus"] != true || res.Data["signed"] != int64(10) {
		t.Fatalf("healthy: %v %v\n%s", res.Data, err, res.Text)
	}
	// unjailed on paper but not signing
	chain.Set(func(c *fakenode.Comet) { c.Signs = func(h int64) bool { return h%3 == 0 } })
	res, _ = (consensusTool{}).Run(c, toolkit.Args{"validator": valoper, "window": float64(10)})
	if res.Data["in_consensus"] != false || !strings.Contains(res.Text, "NOT participating") {
		t.Fatalf("not signing:\n%s", res.Text)
	}
	jail(n, time.Now())
	chain.Set(func(c *fakenode.Comet) { c.InSet = false; c.Signs = nil })
	res, _ = (consensusTool{}).Run(c, toolkit.Args{"validator": valoper})
	if res.Data["in_consensus"] != false || res.Data["voting_power"] != int64(0) {
		t.Fatalf("jailed:\n%s", res.Text)
	}
}

func TestUnjailBlockedWhileSignerAhead(t *testing.T) {
	n, c, valoper, _ := setupChain(t)
	jail(n, time.Now().Add(-time.Minute))
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	// restored from an older snapshot: the signer last signed 200 blocks ahead
	if err := os.WriteFile(filepath.Join(home, "data", "priv_validator_state.json"), []byte(`{"height":"1200","round":0,"step":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Profile.Home = home
	c.SetHost(&host.Local{})
	res, err := (jailCheckTool{}).Run(c, toolkit.Args{"validator": valoper})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "199 blocks ahead") || !strings.Contains(res.Text, "never reset priv_validator_state.json") || res.Data["can_unjail_now"] != false {
		t.Fatalf("signer-ahead not a blocker:\n%s", res.Text)
	}
	if _, err := (unjailTool{}).Run(c, toolkit.Args{"validator": valoper}); err == nil || !strings.Contains(err.Error(), "ahead") {
		t.Fatalf("unjail not refused: %v", err)
	}
}

func TestUnjailBlockedOnWrongConsensusKey(t *testing.T) {
	n, c, valoper, chain := setupChain(t)
	jail(n, time.Now().Add(-time.Minute))
	chain.Set(func(c *fakenode.Comet) { c.NodeKeyAddr = make([]byte, 20) })
	res, err := (jailCheckTool{}).Run(c, toolkit.Args{"validator": valoper})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "but the validator is registered with") || res.Data["can_unjail_now"] != false {
		t.Fatalf("wrong key not a blocker:\n%s", res.Text)
	}
}
