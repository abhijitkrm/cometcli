package govtool

import (
	"strings"
	"testing"

	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	upgradev1beta1 "cosmossdk.io/api/cosmos/upgrade/v1beta1"
)

func TestModuleAddress(t *testing.T) {
	// the live chain's gov module account
	if got, _ := ModuleAddress("gov", "cosmos"); got != "cosmos10d07y265gmmuvt4z0w9aw880jnsr700j6zn9kn" {
		t.Fatalf("gov = %s", got)
	}
}

func TestCosmosTypeURL(t *testing.T) {
	a, err := cosmosAny(&upgradev1beta1.MsgSoftwareUpgrade{Authority: "x"})
	if err != nil || a.TypeUrl != "/cosmos.upgrade.v1beta1.MsgSoftwareUpgrade" {
		t.Fatalf("%v %v", a.TypeUrl, err)
	}
}

func TestDepositMath(t *testing.T) {
	min := []*basev1beta1.Coin{{Denom: "stake", Amount: "10000000"}}
	if got := coinsString(scaled(min, "0.010000000000000000")); got != "100000stake" {
		t.Fatalf("scaled = %s", got)
	}
	if got := coinsString(missing(min, []*basev1beta1.Coin{{Denom: "stake", Amount: "200000"}})); got != "9800000stake" {
		t.Fatalf("missing = %s", got)
	}
	if below(min, min) || !below([]*basev1beta1.Coin{{Denom: "stake", Amount: "1"}}, min) {
		t.Fatal("below")
	}
	if !ratioGreater("0.01", "0.000") || ratioGreater("", "0.01") {
		t.Fatal("ratioGreater")
	}
}

func TestParseMessages(t *testing.T) {
	anys, err := parseMessages(`[{"@type":"/cosmos.bank.v1beta1.MsgSend","from_address":"a","to_address":"b","amount":[{"denom":"x","amount":"1"}]}]`)
	if err != nil || len(anys) != 1 || anys[0].TypeUrl != "/cosmos.bank.v1beta1.MsgSend" {
		t.Fatalf("%v %v", anys, err)
	}
	if _, err := parseMessages(`[{"@type":"/nope.Msg"}]`); err == nil || !strings.Contains(err.Error(), "@type") {
		t.Fatalf("unknown type: %v", err)
	}
	if _, err := parseCoins("10 stake"); err == nil {
		t.Fatal("bad coin accepted")
	}
}
