package netspec

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPatchGenesis(t *testing.T) {
	raw, err := os.ReadFile("testdata/genesis.json")
	if err != nil {
		t.Fatal(err)
	}
	s := FromEnv(ParseEnv(env))
	s.Chain.DisplayDenom = "DEX"
	s.Genesis.EVM.Precompiles = []string{"slashing", "staking", "bank"} // out of order on purpose
	s.Genesis.EVM.MaxGasPerBlock = 30000000
	out, err := s.PatchGenesis(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p := os.Getenv("GENESIS_OUT"); p != "" {
		_ = os.WriteFile(p, out, 0o644) // for validating with the real evmd
	}
	var g map[string]any
	if err := json.Unmarshal(out, &g); err != nil {
		t.Fatal(err)
	}
	app := g["app_state"].(map[string]any)
	get := func(path ...string) any {
		var cur any = app
		for _, k := range path {
			cur = cur.(map[string]any)[k]
		}
		return cur
	}
	checks := map[string][2]any{
		"chain id":     {g["chain_id"], "primium-1"},
		"bond denom":   {get("staking", "params", "bond_denom"), "adex"},
		"evm denom":    {get("evm", "params", "evm_denom"), "adex"},
		"voting":       {get("gov", "params", "voting_period"), "600s"},
		"unbonding":    {get("staking", "params", "unbonding_time"), "600s"},
		"window":       {get("slashing", "params", "signed_blocks_window"), "10000"},
		"min signed":   {get("slashing", "params", "min_signed_per_window"), "0.500000000000000000"},
		"quorum":       {get("gov", "params", "quorum"), "0.334000000000000000"},
		"min deposit":  {get("gov", "params", "min_deposit").([]any)[0].(map[string]any)["amount"], "10000000"},
		"block gas":    {g["consensus"].(map[string]any)["params"].(map[string]any)["block"].(map[string]any)["max_gas"], "30000000"},
		"display unit": {len(get("bank", "denom_metadata").([]any)), 1},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}
	pcs := get("evm", "params", "active_static_precompiles").([]any)
	if len(pcs) != 3 || pcs[0] != "0x0000000000000000000000000000000000000800" || pcs[2] != "0x0000000000000000000000000000000000000806" {
		t.Errorf("precompiles must be addresses, sorted: %v", pcs)
	}
	s.Genesis.EVM.Precompiles = []string{"nope"}
	if _, err := s.PatchGenesis(raw); err == nil {
		t.Error("an unknown precompile must be refused")
	}
}
