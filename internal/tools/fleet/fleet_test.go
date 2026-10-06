package fleet

import (
	"context"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func ctx(t *testing.T, bypass bool) (*toolkit.Context, *[]string) {
	t.Helper()
	var asked []string
	cfg := &config.Config{Profiles: map[string]*config.Profile{
		"val0": {Name: "val0", Binary: "evmd", Transport: config.Transport{Type: "local"}},
		"val1": {Name: "val1", Binary: "evmd", Transport: config.Transport{Type: "local"}},
	}}
	c := &toolkit.Context{Context: context.Background(), Cfg: cfg, AutoApproveBelow: toolkit.TierLocalChange,
		Approver: func(_ *toolkit.Context, p string, _ toolkit.Tier, _ map[string]any) (bool, error) {
			asked = append(asked, p)
			return false, nil
		}}
	if bypass {
		c.AutoApproveBelow = toolkit.TierOnChain
	}
	return c, &asked
}

func TestFleetShellReadRunsEverywhere(t *testing.T) {
	c, asked := ctx(t, false)
	res, err := (shellTool{}).Run(c, toolkit.Args{"cmd": "echo up"})
	if err != nil || strings.Count(res.Text, "up") != 2 || len(*asked) != 0 {
		t.Fatalf("read across fleet: %v %v asked=%v", res, err, *asked)
	}
}

func TestFleetShellGatesLikeBash(t *testing.T) {
	c, asked := ctx(t, true) // bypass: local changes don't ask
	if _, err := (shellTool{}).Run(c, toolkit.Args{"cmd": "cat ~/.evmd/config/priv_validator_key.json"}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("key material: %v", err)
	}
	if _, err := (shellTool{}).Run(c, toolkit.Args{"cmd": "docker exec v evmd tx gov vote 3 yes --from v"}); err == nil || len(*asked) != 1 {
		t.Fatalf("a fleet-wide tx must ask even in bypass: %v asked=%v", err, *asked)
	}
	if _, err := (shellTool{}).Run(c, toolkit.Args{"cmd": "./vote.sh"}); err == nil || len(*asked) != 2 {
		t.Fatalf("fleet-wide script must ask: %v asked=%v", err, *asked)
	}
	c.Rules, _ = toolkit.NewRules(nil, nil, []string{"fleet.shell(systemctl:*)"})
	if _, err := (shellTool{}).Run(c, toolkit.Args{"cmd": "systemctl restart evmd"}); err == nil || !strings.Contains(err.Error(), "denied by permission rule") {
		t.Fatalf("rules ignored: %v", err)
	}
}

func TestFleetExecRefusesMutatingTools(t *testing.T) {
	reg := toolkit.NewRegistry()
	Register(reg)
	c, _ := ctx(t, false)
	ex, _ := reg.Get("fleet.exec")
	if _, err := ex.Run(c, toolkit.Args{"tool": "fleet.shell"}); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("fan-out of a mutating tool: %v", err)
	}
	if _, err := ex.Run(c, toolkit.Args{"tool": "nope"}); err == nil {
		t.Fatal("unknown tool accepted")
	}
}
