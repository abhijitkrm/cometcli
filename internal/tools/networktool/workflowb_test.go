package networktool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/keys"
	"github.com/abhijitkrm/cometcli/internal/netspec"
)

const testSpec = `chain:
  id: wb-1
  denom: adex
  bech32_prefix: cosmos
image:
  name: primium-evm
  tag: v0.7.2
  naming: primium-{tag}
genesis:
  initial_stake: '1000'
  initial_balance: '2000'
`

const (
	accA = "cosmos1gk5aka9mc8rw2uxm3xmzrtne526vj9t2z8rzay"
	idA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idB  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func kitFixture(t *testing.T) (dir string, k *Kit, id string) {
	t.Helper()
	d := t.TempDir()
	sp := filepath.Join(d, "spec-src.yaml")
	_ = os.WriteFile(sp, []byte(testSpec), 0o644)
	k = &Kit{Version: FormatVersion, ChainID: "wb-1", Denom: "adex", Stake: "1000", Image: "primium-v0.7.2",
		Validators: []KitValidator{
			{Moniker: "val-a", Account: accA, NodeID: idA, ConsKey: "Q09OU0E=", Address: "10.0.0.1:26656"},
			{Moniker: "val-b", Account: "cosmos1xtk0ehth60ls99rvnayxkwhqcsv608md0px450", NodeID: idB, Address: "10.0.0.2:26756"},
		}}
	dir = filepath.Join(d, "kit")
	id, err := WriteKit(dir, k, []byte(`{"chain_id":"wb-1"}`), sp)
	if err != nil {
		t.Fatal(err)
	}
	return dir, k, id
}

func TestKitRoundTripAndTamper(t *testing.T) {
	dir, _, id := kitFixture(t)
	k, base, spec, id2, err := ReadKit(dir)
	if err != nil || id2 != id || k.ChainID != "wb-1" || spec.Chain.ID != "wb-1" || !strings.Contains(string(base), "wb-1") {
		t.Fatalf("read kit: %v %v", err, k)
	}
	// a changed base genesis is caught before anything uses it
	_ = os.WriteFile(filepath.Join(dir, "base-genesis.json"), []byte(`{"chain_id":"wb-1","x":1}`), 0o644)
	if _, _, _, _, err := ReadKit(dir); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered kit accepted: %v", err)
	}
}

func gentxFor(t *testing.T, moniker, account, consKey, stake, memo string, signed bool) json.RawMessage {
	t.Helper()
	valoper, err := keys.ValAddress(account)
	if err != nil {
		t.Fatal(err)
	}
	sigs := []string{}
	if signed {
		sigs = []string{"c2ln"}
	}
	g := map[string]any{"body": map[string]any{"memo": memo, "messages": []any{map[string]any{
		"@type": "/cosmos.staking.v1beta1.MsgCreateValidator", "description": map[string]any{"moniker": moniker},
		"delegator_address": "", "validator_address": valoper, "pubkey": map[string]any{"key": consKey},
		"value": map[string]any{"denom": "adex", "amount": stake}}}}, "signatures": sigs}
	raw, _ := json.Marshal(g)
	return raw
}

func TestCollectChecksEveryGentx(t *testing.T) {
	_, k, id := kitFixture(t)
	good := func() *GentxReturn {
		return &GentxReturn{Version: FormatVersion, ChainID: "wb-1", KitID: id, Moniker: "val-a", Account: accA, NodeID: idA,
			Gentx: gentxFor(t, "val-a", accA, "Q09OU0E=", "1000", idA+"@10.0.0.1:26656", true)}
	}
	if err := CheckReturn(k, id, good()); err != nil {
		t.Fatalf("a good gentx was rejected: %v", err)
	}
	for name, mutate := range map[string]func(r *GentxReturn){
		"other kit":   func(r *GentxReturn) { r.KitID = "deadbeef" },
		"not in kit":  func(r *GentxReturn) { r.Moniker = "val-z" },
		"wrong stake": func(r *GentxReturn) { r.Gentx = gentxFor(t, "val-a", accA, "Q09OU0E=", "999999", idA+"@x:1", true) },
		"other account": func(r *GentxReturn) {
			r.Gentx = gentxFor(t, "val-a", k.Validators[1].Account, "Q09OU0E=", "1000", idA+"@x:1", true)
		},
		"other cons key":  func(r *GentxReturn) { r.Gentx = gentxFor(t, "val-a", accA, "T1RIRVI=", "1000", idA+"@x:1", true) },
		"other node":      func(r *GentxReturn) { r.Gentx = gentxFor(t, "val-a", accA, "Q09OU0E=", "1000", idB+"@x:1", true) },
		"moniker swapped": func(r *GentxReturn) { r.Gentx = gentxFor(t, "val-b", accA, "Q09OU0E=", "1000", idA+"@x:1", true) },
		"unsigned":        func(r *GentxReturn) { r.Gentx = gentxFor(t, "val-a", accA, "Q09OU0E=", "1000", idA+"@x:1", false) },
		"garbage":         func(r *GentxReturn) { r.Gentx = json.RawMessage(`{"body":{}}`) },
	} {
		r := good()
		mutate(r)
		if err := CheckReturn(k, id, r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReadJoinValidates(t *testing.T) {
	d := t.TempDir()
	spec := &netspec.Spec{Chain: netspec.Chain{ID: "wb-1", Bech32Prefix: "cosmos"}}
	write := func(j Join) string {
		p := filepath.Join(d, "j.json")
		raw, _ := json.Marshal(j)
		_ = os.WriteFile(p, raw, 0o644)
		return p
	}
	ok := Join{Version: FormatVersion, ChainID: "wb-1", Moniker: "val-a", Account: accA, NodeID: idA, ConsKey: "Q09OU0E=", IP: "1.2.3.4", P2PPort: 26656}
	if _, err := ReadJoin(write(ok), spec); err != nil {
		t.Fatal(err)
	}
	for name, j := range map[string]Join{
		"chain":   func() Join { x := ok; x.ChainID = "other-1"; return x }(),
		"moniker": func() Join { x := ok; x.Moniker = "bad name!"; return x }(),
		"account": func() Join { x := ok; x.Account = "osmo1abc"; return x }(),
		"node id": func() Join { x := ok; x.NodeID = "xyz"; return x }(),
		"conskey": func() Join { x := ok; x.ConsKey = ""; return x }(),
	} {
		if _, err := ReadJoin(write(j), spec); err == nil {
			t.Errorf("join with a bad %s accepted", name)
		}
	}
}

func TestPackagesPeerWithEveryoneElse(t *testing.T) {
	dir, k, _ := kitFixture(t)
	out := filepath.Join(t.TempDir(), "pkgs")
	pk, err := WritePackages(out, k, []byte(`{"chain_id":"wb-1","final":true}`), filepath.Join(dir, "spec.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, gen, _, err := ReadPackage(pk["val-a"])
	if err != nil || !strings.Contains(string(gen), "final") {
		t.Fatal(err)
	}
	if len(p.Peers) != 1 || p.Peers[0] != idB+"@10.0.0.2:26756" {
		t.Fatalf("val-a peers = %v", p.Peers)
	}
	_ = os.WriteFile(filepath.Join(pk["val-b"], "genesis.json"), []byte(`{}`), 0o644)
	if _, _, _, err := ReadPackage(pk["val-b"]); err == nil {
		t.Fatal("a tampered package was accepted")
	}
}
