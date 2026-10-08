package triage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/testutil/fakenode"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func TestMain(m *testing.M) {
	fakenode.MaybeRunFakeDocker()
	os.Exit(m.Run())
}

const logs = `E[2026-10-06|21:41:13] Stopping peer for error err="read tcp 172.18.0.3:26656: i/o timeout" module=p2p
INF slashing and jailing validator due to liveness fault height=162050 module=x/slashing
E[2026-10-06|21:42:00] failed to write to db: no space left on device
`

func writeHome(t *testing.T, configToml, appToml, pvs string) string {
	t.Helper()
	home := t.TempDir()
	for rel, body := range map[string]string{"config/config.toml": configToml, "config/app.toml": appToml, "data/priv_validator_state.json": pvs} {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// setup builds a jailed, catching-up validator on the fake chain.
func setup(t *testing.T, binary, configToml, appToml string) (*toolkit.Context, *fakenode.Comet) {
	n := fakenode.Start(t, "mychain-1")
	chain := fakenode.StartComet(t)
	p := n.Profile(t, "eth_secp256k1")
	fakenode.WithComet(p, chain)
	p.Binary = binary
	p.Service = config.Service{Type: "docker", Unit: "val0"}
	p.Home = writeHome(t, configToml, appToml, `{"height":"1000","round":0,"step":3}`)
	fakenode.InstallFakeDocker(t, "test", "")
	fakenode.SetNodeLogs(t, logs, "status=running restarts=4 oom_killed=false exit=0")
	c := &toolkit.Context{Context: context.Background(), Profile: p, Cfg: &config.Config{Profiles: map[string]*config.Profile{p.Name: p}}}
	c.SetHost(&host.Local{})
	t.Cleanup(c.Close)
	tb, err := c.Tx()
	if err != nil {
		t.Fatal(err)
	}
	valoper, _ := tb.ValAddress()
	p.Metadata["valoper"] = valoper
	n.Set(func(n *fakenode.Node) {
		n.Validator = &stakingv1beta1.Validator{OperatorAddress: valoper, ConsensusPubkey: fakenode.ConsPubAny(),
			Jailed: true, Status: stakingv1beta1.BondStatus_BOND_STATUS_UNBONDING, MinSelfDelegation: "1"}
		n.SigningInfo = &slashingv1beta1.ValidatorSigningInfo{JailedUntil: timestamppb.New(time.Now().Add(5 * time.Minute)), MissedBlocksCounter: 600}
	})
	behind := time.Now().Add(-2 * time.Hour)
	chain.Set(func(c *fakenode.Comet) { c.InSet, c.CatchingUp, c.LatestTimeOverride = false, true, &behind })
	return c, chain
}

func ids(hits []kb.Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Case.ID)
	}
	return out
}

func has(list []string, id string) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

func TestTriageJailedEVMValidator(t *testing.T) {
	c, _ := setup(t, "evmd",
		"db_backend = \"goleveldb\"\n[p2p]\npersistent_peers = \"a@1.2.3.4:26656,b@5.6.7.8:26656\"\n[consensus]\ndouble_sign_check_height = 10\n",
		"minimum-gas-prices = \"10000000000adex\"\npruning = \"custom\"\n[json-rpc]\nenable = false\n")
	r := Collect(c, 30*time.Minute)
	s := r.Signals
	if r.Chain != "cosmos-evm" {
		t.Fatalf("chain = %s", r.Chain)
	}
	want := map[string]any{
		"val.jailed": true, "val.tombstoned": false, "val.bonded": false, "node.catching_up": true,
		"node.reachable": true, "node.peers": 3.0, "proc.running": true, "proc.restarts": 4.0,
		"logs.jail": 1.0, "logs.disk_io": 1.0, "config.persistent_peers": 2.0, "config.db_backend": "goleveldb",
		"app.min_gas_prices_empty": false, "pvs.height": 1000.0, "node.key_mismatch": false, "val.missed_pct": 6.0,
	}
	for k, v := range want {
		if s[k] != v {
			t.Errorf("%s = %v (%T), want %v", k, s[k], s[k], v)
		}
	}
	if r.Sources["comet"] != "ok" || r.Sources["chain"] != "ok" || r.Sources["logs"] != "ok" || r.Sources["config"] != "ok" {
		t.Errorf("sources: %v", r.Sources)
	}
	if r.Sources["evm"] == "ok" || r.Sources["evm"] == "" {
		t.Errorf("evm collector should run on cosmos-evm and report the missing endpoint: %q", r.Sources["evm"])
	}
	hits := LoadKB(c).Match(s, r.Chain)
	got := ids(hits)
	for _, id := range []string{"val-jailed-downtime", "val-jailed-waiting-period", "node-catching-up", "host-disk-full"} {
		if !has(got, id) {
			t.Errorf("missing case %s in %v", id, got)
		}
	}
	// the disk is the root; the jail and the lag are its symptoms
	if got[0] != "host-disk-full" {
		t.Errorf("top case %s", got[0])
	}
	for _, h := range hits {
		if h.Case.ID == "val-jailed-downtime" && h.SymptomOf != "host-disk-full" {
			t.Errorf("jail symptom of %q", h.SymptomOf)
		}
	}
	text := Render(r, hits, "")
	t.Log("\n" + text)
	for _, sub := range []string{"val: ", "jailed=true", "matched cases", "symptoms of the above", "kb.show", "disk_io:"} {
		if !strings.Contains(text, sub) {
			t.Errorf("render lacks %q:\n%s", sub, text)
		}
	}
	if len(text) > 3000 {
		t.Errorf("render is %d bytes — too big for small TPM budgets:\n%s", len(text), text)
	}
}

func TestTriagePlainSDKConfigProblems(t *testing.T) {
	c, chain := setup(t, "gaiad",
		"db_backend = \"goleveldb\"\n[mempool]\ntype = \"app\"\n",
		"minimum-gas-prices = \"\"\napp-db-backend = \"pebbledb\"\n[mempool]\nmax-txs = -1\n")
	chain.Set(func(c *fakenode.Comet) { c.CatchingUp, c.LatestTimeOverride = false, nil })
	r := Collect(c, 30*time.Minute)
	if r.Chain != "cosmos-sdk" {
		t.Fatalf("chain = %s", r.Chain)
	}
	if _, ran := r.Sources["evm"]; ran {
		t.Error("evm collector ran on a plain cosmos-sdk chain")
	}
	for k, v := range map[string]any{"config.db_backend_mismatch": true, "config.mempool_mismatch": true, "app.min_gas_prices_empty": true} {
		if r.Signals[k] != v {
			t.Errorf("%s = %v", k, r.Signals[k])
		}
	}
	got := ids(LoadKB(c).Match(r.Signals, r.Chain))
	for _, id := range []string{"cfg-db-backend-mismatch", "cfg-mempool-mismatch", "cfg-min-gas-prices-empty"} {
		if !has(got, id) {
			t.Errorf("missing %s in %v", id, got)
		}
	}
	for _, id := range got {
		if strings.HasPrefix(id, "evm-") {
			t.Errorf("evm case %s on a plain sdk chain", id)
		}
	}
}

func TestTriageUnreachableNodeStillReports(t *testing.T) {
	p := &config.Profile{Name: "x", Role: "validator", Binary: "gaiad", Home: t.TempDir(),
		Endpoints: config.Endpoints{Comet: "http://127.0.0.1:1", GRPC: "127.0.0.1:1"}, Service: config.Service{Type: "none"}}
	c := &toolkit.Context{Context: context.Background(), Profile: p}
	c.SetHost(&host.Local{})
	old := Timeout
	Timeout = 3 * time.Second
	defer func() { Timeout = old }()
	r := Collect(c, 10*time.Minute)
	if r.Signals["node.reachable"] != false {
		t.Fatalf("node.reachable = %v", r.Signals["node.reachable"])
	}
	if r.Sources["comet"] == "ok" || r.Sources["process"] == "ok" {
		t.Errorf("sources: %v", r.Sources)
	}
	if !has(ids(LoadKB(c).Match(r.Signals, r.Chain)), "node-rpc-down") {
		t.Error("node-rpc-down not matched")
	}
}

func TestKBTools(t *testing.T) {
	t.Setenv("COMETCLI_HOME", t.TempDir())
	c := &toolkit.Context{Context: context.Background()}
	res, err := Search{}.Run(c, toolkit.Args{"query": "app hash"})
	if err != nil || !strings.Contains(res.Text, "state-apphash-mismatch") {
		t.Fatalf("search: %v %v", res, err)
	}
	res, err = Show{}.Run(c, toolkit.Args{"id": "val-jailed-downtime"})
	if err != nil || !strings.Contains(res.Text, "NEVER:") || !strings.Contains(res.Text, "[tx] val.unjail") {
		t.Fatalf("show: %v %v", res, err)
	}
	yaml := "id: custom-sentry-lost\ntitle: Sentry lost\nseverity: high\nmatch: {all: [\"node.peers == 0\", \"config.pex == false\"]}\nfix: [\"[change] restart sentry\"]\ntest: {signals: {node.peers: 0, config.pex: false}}\n"
	if _, err := (Add{}).Run(c, toolkit.Args{"yaml": yaml}); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadKB(c).Cases["custom-sentry-lost"]; !ok {
		t.Fatal("added case not loaded")
	}
	if _, err := (Add{}).Run(c, toolkit.Args{"yaml": strings.Replace(yaml, "custom-sentry-lost", "../evil", 1)}); err == nil {
		t.Fatal("path-traversal id accepted")
	}
}

func TestTriageRemembersChangesIncidentsAndHistory(t *testing.T) {
	c, chain := setup(t, "evmd", "db_backend = \"goleveldb\"\n", "minimum-gas-prices = \"1adex\"\n")
	if _, err := (Triage{}).Run(c, toolkit.Args{}); err != nil {
		t.Fatal(err)
	}
	chain.Set(func(c *fakenode.Comet) { c.Peers = 0 }) // something breaks between sweeps
	res, err := (Triage{}).Run(c, toolkit.Args{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "changed since the last check") || !strings.Contains(res.Text, "node.peers 3→0") {
		t.Fatalf("no deltas:\n%s", res.Text)
	}
	// the agent records the incident when it's done
	rec, err := (RecordIncident{}).Run(c, toolkit.Args{"title": "Peers lost after firewall change", "case": "node-no-peers",
		"root_cause": "ufw blocked 26656", "actions": "- reopened 26656\n- restarted", "outcome": "resolved"})
	if err != nil || !strings.Contains(rec.Text, "recorded incident") {
		t.Fatalf("%v %v", rec, err)
	}
	res, _ = (Triage{}).Run(c, toolkit.Args{})
	if !strings.Contains(res.Text, "past incidents on this node (30d): 1") || !strings.Contains(res.Text, "recurring: node-no-peers ×1") {
		t.Fatalf("past incident not surfaced:\n%s", res.Text)
	}
	list, _ := (ListIncidents{}).Run(c, toolkit.Args{})
	id := list.Data["incidents"].([]string)[0]
	show, _ := (ShowIncident{}).Run(c, toolkit.Args{"id": id})
	if !strings.Contains(show.Text, "ufw blocked 26656") || !strings.Contains(show.Text, "- reopened 26656") {
		t.Fatalf("show:\n%s", show.Text)
	}
	h, err := (History{}).Run(c, toolkit.Args{"signals": "node.peers,val.", "since": "1h"})
	if err != nil || !strings.Contains(h.Text, "node.peers: 3 → 0") || !strings.Contains(h.Text, "val.jailed") || !strings.Contains(h.Text, "3 samples") {
		t.Fatalf("history:\n%s %v", h.Text, err)
	}
}
