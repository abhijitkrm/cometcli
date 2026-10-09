package drill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func TestGenesisAndConfigPatching(t *testing.T) {
	n := &Net{Dir: t.TempDir(), Validators: 2, Image: "evmd:test"}
	n.Defaults()
	for i := 0; i < 2; i++ {
		_ = os.MkdirAll(filepath.Join(n.home(i), "config"), 0o755)
		_ = os.WriteFile(filepath.Join(n.home(i), "config", "config.toml"), []byte("addr_book_strict = true\n"), 0o644)
		_ = os.WriteFile(filepath.Join(n.home(i), "config", "app.toml"), []byte("minimum-gas-prices = \"0stake\"\naddress = \"localhost:9090\"\n"), 0o644)
	}
	g := `{"app_state":{"slashing":{"params":{"signed_blocks_window":"100","downtime_jail_duration":"600s"}},
	  "gov":{"params":{"voting_period":"172800s"}},"evm":{"params":{"evm_denom":"aevmos"}}}}`
	_ = os.WriteFile(filepath.Join(n.home(0), "config", "genesis.json"), []byte(g), 0o644)
	denom, err := n.patchGenesis()
	if err != nil || denom != "aevmos" {
		t.Fatalf("denom %q %v", denom, err)
	}
	raw, _ := os.ReadFile(filepath.Join(n.home(1), "config", "genesis.json"))
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	sl := got["app_state"].(map[string]any)["slashing"].(map[string]any)["params"].(map[string]any)
	if sl["signed_blocks_window"] != "20" || sl["downtime_jail_duration"] != "30s" {
		t.Fatalf("slashing not patched (or genesis not copied): %v", sl)
	}
	if err := n.patchConfigs(1, denom); err != nil {
		t.Fatal(err)
	}
	app, _ := os.ReadFile(filepath.Join(n.home(1), "config", "app.toml"))
	if !strings.Contains(string(app), `"1000000000aevmos"`) || !strings.Contains(string(app), "0.0.0.0:9090") {
		t.Fatalf("app.toml:\n%s", app)
	}
	compose := n.composeFile("501:20")
	for _, want := range []string{"container_name: cometcli-drill-1", "127.0.0.1:47067:26657", "image: evmd:test", "172.30.9.3"} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose lacks %q", want)
		}
	}
}

func TestApprovalsOnlyForDrillProfiles(t *testing.T) {
	if ok, err := drillApprover(&toolkit.Context{Profile: &config.Profile{Name: "val01"}}, "restart", toolkit.TierLocalChange, nil); ok || err == nil {
		t.Fatal("approved a change on a real profile")
	}
	if ok, err := drillApprover(&toolkit.Context{}, "rm -rf", toolkit.TierLocalChange, nil); ok || err == nil {
		t.Fatal("approved a change on this machine (no profile)")
	}
	drill := &config.Profile{Name: "drill3", Metadata: map[string]string{"drill": "true"}}
	if ok, _ := drillApprover(&toolkit.Context{Profile: drill}, "unjail", toolkit.TierOnChain, nil); !ok {
		t.Fatal("drill profile not approved")
	}
	res := RunScenario(context.Background(), &Env{Profile: &config.Profile{Name: "val01"}}, Scenarios[0], nil, func(string) {})
	if !strings.Contains(res.Err, "not a drill profile") {
		t.Fatalf("ran a scenario on a real profile: %+v", res)
	}
}

func TestScoreboard(t *testing.T) {
	s := Scoreboard([]Result{
		{Scenario: "jail-downtime", TriageCase: "val-jailed-downtime", TriageOK: true, Fixed: true, Steps: 9, Tokens: 41000, Duration: 3 * time.Minute},
		{Scenario: "oom", TriageCase: "node-crash-loop", Fixed: false, FixDetail: "memory limit still 120 MB", Steps: 30},
	})
	if !strings.Contains(s, "score: triage 1/2 · fixed 1/2") || !strings.Contains(s, "memory limit still 120 MB") {
		t.Fatalf("scoreboard:\n%s", s)
	}
}

func TestDrillSpecKeepsTheNetworkButShortensTheClock(t *testing.T) {
	base := &netspec.Spec{
		Chain:       netspec.Chain{ID: "primium-1", Denom: "aprm", EVMChainID: 9000},
		Image:       netspec.Image{Name: "primium-evm", Tag: "v0.7.2", Naming: "primium-{tag}"},
		MinGasPrice: "1000000000", DBBackend: "rocksdb",
		Consensus: netspec.Consensus{TimeoutCommitMS: 200},
		Services:  netspec.Services{API: true, WS: true},
		HostHome:  "/integral/primium",
	}
	base.Genesis.Slashing.SignedBlocksWindow = 10000
	base.Genesis.Gov.VotingPeriodS = 172800
	base.Genesis.EVM.Precompiles = []string{"staking", "bank"}
	ds, err := DrillSpec(base, 4)
	if err != nil {
		t.Fatal(err)
	}
	if ds.Chain.ID != "primium-1-drill" || ds.Chain.Denom != "aprm" || ds.Chain.EVMChainID != 9000 {
		t.Errorf("chain = %+v", ds.Chain)
	}
	if ds.Image != base.Image || ds.DBBackend != "rocksdb" || ds.Consensus.TimeoutCommitMS != 200 || len(ds.Genesis.EVM.Precompiles) != 2 {
		t.Errorf("the network's settings weren't kept: %+v", ds)
	}
	if ds.Genesis.Slashing.SignedBlocksWindow != 20 || ds.Genesis.Slashing.DowntimeJailDurationS != 60 || ds.Genesis.Gov.VotingPeriodS != 60 {
		t.Errorf("drill timing not applied: %+v %+v", ds.Genesis.Slashing, ds.Genesis.Gov)
	}
	if !ds.Services.GRPC || !ds.Services.JSONRPC || !ds.Services.WS || ds.HostHome != "" || ds.Genesis.Validators != 4 {
		t.Errorf("services/home: %+v %q", ds.Services, ds.HostHome)
	}
	if base.Chain.ID != "primium-1" || base.Genesis.Slashing.SignedBlocksWindow != 10000 {
		t.Error("the base spec was modified")
	}
	again, _ := DrillSpec(ds, 4)
	if again.Chain.ID != "primium-1-drill" {
		t.Errorf("suffix applied twice: %s", again.Chain.ID)
	}
}
