package netspec

import (
	"strings"
	"testing"
)

const env = `
# Chain
COSMOS_CHAIN_ID="primium-1"
EVM_CHAIN_ID="123457"
CHAIN_DENOM="adex"
VOTING_PERIOD="600"           # 600s for local testing
EXPEDITED_VOTING_PERIOD="300" # must be < VOTING_PERIOD
QUORUM="0.334"
THRESHOLD="0.500"
EXPEDITED_THRESHOLD="0.667"
UNBONDING_TIME="600"
SIGNED_BLOCKS_WINDOW="10000"
MIN_SIGNED_PER_WINDOW="0.5"
ENABLE_PRECOMPILES="true"
PRECOMPILES_LIST="staking,distribution,vesting,bank,gov,slashing"
TIMEOUT_PROPOSE="400"
TIMEOUT_COMMIT="200"
ENABLE_JSONRPC="true"
ENABLE_WS="true"
WS_ORIGINS="*"
MIN_GAS_PRICE="1000000000"
MIN_DEPOSIT="10000000"
DB_BACKEND="goleveldb"
DOCKER_IMAGE="primium-evm"
DOCKER_TAG="v0.7.2"
PRIMIUM_HOST_HOME=/integral/logs/primium
SOMETHING_NEW="kept"
`

func TestImportEnv(t *testing.T) {
	s := FromEnv(ParseEnv(env))
	if s.Chain.ID != "primium-1" || s.Chain.EVMChainID != 123457 || s.Chain.Denom != "adex" {
		t.Fatalf("chain = %+v", s.Chain)
	}
	if s.Genesis.Gov.VotingPeriodS != 600 || s.Genesis.Gov.ExpeditedVotingPeriod != 300 || s.Genesis.Gov.Quorum != 0.334 {
		t.Fatalf("gov = %+v (comments must not leak into values)", s.Genesis.Gov)
	}
	if len(s.Genesis.EVM.Precompiles) != 6 || s.HostHome != "/integral/logs/primium" || s.Extra["SOMETHING_NEW"] != "kept" {
		t.Fatalf("precompiles=%v home=%q extra=%v", s.Genesis.EVM.Precompiles, s.HostHome, s.Extra)
	}
}

func TestReviewProd(t *testing.T) {
	s := FromEnv(ParseEnv(env))
	if f := s.Review(false); len(f) != 0 {
		t.Fatalf("a consistent test spec has no findings without prod: %+v", f)
	}
	var got []string
	for _, f := range s.Review(true) {
		got = append(got, f.What)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{"voting period is 10m", "unbonding time is 10m", "tuned for a LAN", "any origin", "test default"} {
		if !strings.Contains(all, want) {
			t.Errorf("prod review lacks %q:\n%s", want, all)
		}
	}
	s.Genesis.Gov.ExpeditedVotingPeriod = 900
	if f := s.Review(false); len(f) == 0 || f[0].Sev != Fail {
		t.Fatalf("expedited >= voting period must fail: %+v", f)
	}
}

func docs() (map[string]any, map[string]any) {
	cfg := map[string]any{
		"db_backend":      "goleveldb",
		"mempool":         map[string]any{"type": "flood"},
		"rpc":             map[string]any{"cors_allowed_origins": []any{"*"}},
		"p2p":             map[string]any{"external_address": ""},
		"consensus":       map[string]any{"timeout_propose": "400ms", "timeout_commit": "1s"},
		"tx_index":        map[string]any{"indexer": "null"},
		"instrumentation": map[string]any{"prometheus": false},
	}
	app := map[string]any{
		"app-db-backend":     "goleveldb",
		"minimum-gas-prices": "1000000000adex",
		"pruning":            "default",
		"mempool":            map[string]any{"max-txs": int64(-1)},
		"api":                map[string]any{"enable": false, "swagger": true, "enabled-unsafe-cors": true},
		"json-rpc":           map[string]any{"enable": true, "api": "eth,net,web3,debug", "allow-insecure-unlock": true},
		"evm":                map[string]any{"evm-chain-id": int64(123457)},
	}
	return cfg, app
}

func whats(f []Finding) string {
	var s []string
	for _, x := range f {
		s = append(s, x.Sev.String()+" "+x.What)
	}
	return strings.Join(s, "\n")
}

func TestValidatorRole(t *testing.T) {
	s := FromEnv(ParseEnv(env))
	cfg, app := docs()
	out := whats(Evaluate("v1", s.Expectations(Validator, "v0.7.2", true), cfg, app, []string{"start"}))
	for _, want := range []string{
		"FAIL mempool.type = flood, want \"app\"",
		"cors_allowed_origins", "enabled-unsafe-cors", "allow-insecure-unlock", "swagger", "without debug",
		"timeout_commit = 1s, want 200ms", "external_address", "--json-rpc.ws-origins = missing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("validator check lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "timeout_propose") {
		t.Errorf("400ms matches 400ms:\n%s", out)
	}
	// a v0.6 node can't run the app mempool: flood is right there
	out = whats(Evaluate("v1", s.Expectations(Validator, "v0.6.1", false), cfg, app, nil))
	if strings.Contains(out, "mempool") {
		t.Errorf("v0.6 with flood is correct:\n%s", out)
	}
}

func TestArchiveRole(t *testing.T) {
	s := FromEnv(ParseEnv(env))
	cfg, app := docs()
	out := whats(Evaluate("a1", s.Expectations(Archive, "v0.7.2", false), cfg, app, []string{"start"}))
	if !strings.Contains(out, "FAIL pruning = default") || !strings.Contains(out, "FAIL tx_index.indexer = null") {
		t.Fatalf("archive check:\n%s", out)
	}
	if strings.Contains(out, "swagger") || strings.Contains(out, "debug") {
		t.Errorf("an archive may serve swagger and debug:\n%s", out)
	}
	// --pruning nothing on the command line counts
	out = whats(Evaluate("a1", s.Expectations(Archive, "v0.7.2", false), cfg, app, []string{"start", "--pruning", "nothing"}))
	if strings.Contains(out, "pruning") {
		t.Errorf("pruning from the command:\n%s", out)
	}
}

func TestCmdFlag(t *testing.T) {
	cmd := []string{"start", "--pruning", "nothing", "--json-rpc.ws-origins=*"}
	if CmdFlag(cmd, "--pruning") != "nothing" || CmdFlag(cmd, "--json-rpc.ws-origins") != "*" || CmdFlag(cmd, "--x") != nil {
		t.Fatal("flag parsing")
	}
}
