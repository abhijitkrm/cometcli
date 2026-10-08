package triage

import (
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func node(name, chain, role string, sig kb.Signals) NodeReport {
	return NodeReport{Profile: &config.Profile{Name: name, ChainID: chain, Role: role}, Report: &Report{Signals: sig}}
}

func up(h float64, extra ...any) kb.Signals {
	s := kb.Signals{"node.reachable": true, "node.height": h, "node.block_age_s": 2.0, "node.version": "0.39.0", "node.peers": 5.0}
	for i := 0; i+1 < len(extra); i += 2 {
		s[extra[i].(string)] = extra[i+1]
	}
	return s
}

func TestFleetFindings(t *testing.T) {
	sentry := node("sentry1", "c-1", "sentry", kb.Signals{"node.reachable": false})
	val1 := node("val1", "c-1", "validator", up(1000, "node.cons_addr_hex", "AAAA1111", "node.voting_power", 10.0))
	val1.Profile.Sentries = []string{"sentry1"}
	val2 := node("val2", "c-1", "validator", up(700, "node.cons_addr_hex", "AAAA1111", "node.voting_power", 10.0, "node.version", "0.38.0"))
	val3 := node("val3", "c-1", "validator", up(1000))
	val2.Profile.Transport = config.Transport{Type: "ssh", Host: "10.0.0.5"}
	val3.Profile.Transport = config.Transport{Type: "ssh", Host: "10.0.0.5"}
	got := strings.Join(FleetFindings([]NodeReport{sentry, val1, val2, val3}), "\n")
	for _, want := range []string{
		"sentry1 is unreachable",
		"val2 is 300 blocks behind the other c-1 nodes",
		"version mismatch on c-1",
		"DOUBLE-SIGN RISK: val1 and val2 run the same consensus key",
		"validators val2, val3 share host 10.0.0.5",
		"val1: all its sentries are down (sentry1)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "CHAIN HALT") {
		t.Error("halt reported while nodes advance")
	}
	// every reachable node stalled: the chain halted, nobody is "behind"
	a := node("a", "c-2", "validator", up(3644, "node.block_age_s", 300.0))
	b := node("b", "c-2", "validator", up(3640, "node.block_age_s", 310.0))
	got = strings.Join(FleetFindings([]NodeReport{a, b}), "\n")
	if !strings.Contains(got, "CHAIN HALT on c-2") || strings.Contains(got, "behind") {
		t.Fatalf("halt:\n%s", got)
	}
	// healthy fleet: nothing to say
	if f := FleetFindings([]NodeReport{node("x", "c-3", "validator", up(10)), node("y", "c-3", "validator", up(10))}); len(f) != 0 {
		t.Fatalf("healthy fleet findings: %v", f)
	}
}

func TestFleetTriageRunsEveryProfile(t *testing.T) {
	c, chain := setup(t, "evmd", "db_backend = \"goleveldb\"\n", "minimum-gas-prices = \"1adex\"\n")
	_ = chain
	p := c.Profile
	other := *p
	other.Name, other.Role = "rpc1", "rpc"
	c.Cfg.Profiles = map[string]*config.Profile{p.Name: p, "rpc1": &other}
	general := &toolkit.Context{Context: c.Context, Cfg: c.Cfg} // general mode: no active profile
	res, err := (FleetTriage{}).Run(general, toolkit.Args{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "fleet triage — 2 nodes") || !strings.Contains(res.Text, "rpc1") || !strings.Contains(res.Text, p.Name) {
		t.Fatalf("report:\n%s", res.Text)
	}
	if !toolkit.IsFleetWide(FleetTriage{}) {
		t.Fatal("fleet.triage must be offered in general mode")
	}
}
