package kb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltinCasesLoadAndSelfMatch(t *testing.T) {
	b := Load("", "")
	if len(b.Errs) > 0 {
		t.Fatalf("load errors: %v", b.Errs)
	}
	if len(b.Cases) < 50 {
		t.Fatalf("only %d cases", len(b.Cases))
	}
	for id, c := range b.Cases {
		if len(c.Test.Signals) == 0 {
			t.Errorf("%s: no test fixture", id)
			continue
		}
		if len(c.Fix) == 0 {
			t.Errorf("%s: no fix steps", id)
		}
		chain := "cosmos-sdk"
		if len(c.Chains) == 1 {
			chain = c.Chains[0]
		}
		hits := b.Match(c.Test.Signals, chain)
		rank := -1
		for i, h := range hits {
			if h.Case.ID == id {
				rank = i
			}
		}
		if rank < 0 || rank > 2 {
			var top []string
			for i := 0; i < len(hits) && i < 4; i++ {
				top = append(top, hits[i].Case.ID)
			}
			t.Errorf("%s: fixture ranks %d (top: %v)", id, rank, top)
		}
	}
}

func TestHealthyNodeMatchesNothing(t *testing.T) {
	healthy := Signals{
		"chain.type": "cosmos-evm", "profile.role": "validator",
		"node.reachable": true, "node.height": 1000.0, "node.catching_up": false, "node.block_age_s": 2.0,
		"node.peers": 12.0, "node.stalled": false, "node.key_mismatch": false,
		"val.jailed": false, "val.tombstoned": false, "val.bonded": true, "val.status": "BONDED", "val.in_set": true,
		"val.signed_recent_pct": 100.0, "val.missed_pct": 0.1, "val.downtime_budget_used_pct": 0.2, "val.self_below_min": false,
		"val.fee_balance_zero": false, "upgrade.pending": false, "gov.unvoted": 0.0,
		"host.disk_used_pct": 40.0, "host.inode_used_pct": 5.0, "host.mem_used_pct": 50.0, "host.load_per_cpu": 0.4,
		"clock.ntp_synced": true, "proc.running": true, "proc.restarts": 0.0, "proc.oom_killed": false, "proc.uptime_s": 86400.0,
		"logs.panic": 0.0, "logs.apphash": 0.0, "logs.peer_net": 12.0,
		"config.db_backend_mismatch": false, "config.mempool_mismatch": false, "config.remote_signer": false,
		"config.persistent_peers": 3.0, "config.seeds": 1.0, "config.statesync_enable": false, "config.pex": true,
		"config.double_sign_check_height": 10.0,
		"app.min_gas_prices_empty":        false, "app.pruning": "custom", "app.api_enable": false, "app.json_rpc_enable": false,
		"evm.reachable": true, "evm.drift": 0.0, "evm.chain_id_mismatch": false, "pvs.ahead_by": 0.0,
	}
	if hits := Load("", "").Match(healthy, "cosmos-evm"); len(hits) > 0 {
		var ids []string
		for _, h := range hits {
			ids = append(ids, h.Case.ID)
		}
		t.Fatalf("healthy node matched %v", ids)
	}
}

func TestCond(t *testing.T) {
	s := Signals{"a": 5.0, "b": true, "c": "Pebbledb", "d": "0.0.0.0:8545"}
	for expr, want := range map[string]bool{
		"a > 4": true, "a >= 5": true, "a < 5": false, "a == 5": true, "a != 5": false,
		"b == true": true, "b == false": false, "c == pebbledb": true, `c != "goleveldb"`: true,
		"d contains 0.0.0.0": true, "x exists": false, "x missing": true, "a exists": true,
		"x > 0": false, "x == false": false, // uncollected never holds
	} {
		c, err := ParseCond(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got := c.Eval(s); got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
	for _, bad := range []string{"a", "a >", "a b > 1", "exists"} {
		if _, err := ParseCond(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestChainFiltering(t *testing.T) {
	b := Load("", "")
	s := Signals{"evm.reachable": false, "node.reachable": true}
	for _, h := range b.Match(s, "cosmos-sdk") {
		if h.Case.ID == "evm-rpc-down" {
			t.Fatal("evm case matched a plain cosmos-sdk chain")
		}
	}
	found := false
	for _, h := range b.Match(s, "cosmos-evm") {
		found = found || h.Case.ID == "evm-rpc-down"
	}
	if !found {
		t.Fatal("evm case missed on cosmos-evm")
	}
}

func TestUserOverrideAndValidate(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`id: node-no-peers
title: Custom no peers
severity: low
match: {all: ["node.peers == 0"]}
fix: ["[change] call the NOC"]
test: {signals: {node.peers: 0}}
`)
	if _, err := Validate(raw); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	b := Load(dir, "")
	if c := b.Cases["node-no-peers"]; c.Title != "Custom no peers" || c.Source != "user" {
		t.Fatalf("override not applied: %+v", c)
	}
	bad := []byte("id: x1\ntitle: t\nseverity: low\nmatch: {all: [\"a == 1\"]}\nfix: [\"do it\"]\n")
	if _, err := Validate(bad); err == nil {
		t.Fatal("untagged fix step accepted")
	}
	mismatch := []byte("id: x2\ntitle: t\nseverity: low\nmatch: {all: [\"a == 1\"]}\nfix: [\"[read] x\"]\ntest: {signals: {a: 2}}\n")
	if _, err := Validate(mismatch); err == nil {
		t.Fatal("case not matching its own fixture accepted")
	}
}

func TestSearch(t *testing.T) {
	cs := Load("", "").Search("app hash mismatch", "cosmos-sdk", 3)
	if len(cs) == 0 || cs[0].ID != "state-apphash-mismatch" {
		t.Fatalf("search: %v", cs)
	}
}
