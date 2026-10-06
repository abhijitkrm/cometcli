package keystool

import (
	"context"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func TestKeyToolsRoundTripAndOperatorOnly(t *testing.T) {
	t.Setenv("COMETCLI_HOME", t.TempDir())
	t.Setenv("COMETCLI_KEYRING_PASSWORD", "pw")
	reg := toolkit.NewRegistry()
	Register(reg)
	c := &toolkit.Context{Context: context.Background(), Profile: &config.Profile{Name: "t", Bech32Prefix: "cosmos", Signer: config.Signer{Backend: "file"}},
		AutoApproveBelow: toolkit.TierOnChain}
	add, _ := reg.Get("keys.add")
	rm, _ := reg.Get("keys.rm")
	if !toolkit.IsOperatorOnly(add) || !toolkit.IsOperatorOnly(rm) {
		t.Fatal("key management must be operator-only")
	}
	for _, n := range []string{"keys.list", "keys.show", "keys.convert"} {
		if tl, _ := reg.Get(n); toolkit.IsOperatorOnly(tl) {
			t.Fatalf("%s should stay agent-callable", n)
		}
	}
	res, err := add.Run(c, toolkit.Args{"name": "ops", "privkey-hex": "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"})
	if err != nil {
		t.Fatal(err)
	}
	addr := res.Data["address"].(string)
	show, _ := reg.Get("keys.show")
	sr, err := show.Run(c, toolkit.Args{"name": "ops"})
	if err != nil || !strings.Contains(sr.Text, addr) || !strings.Contains(sr.Text, "cosmosvaloper") {
		t.Fatalf("show: %v %v", sr, err)
	}
	conv, _ := reg.Get("keys.convert")
	cr, err := conv.Run(c, toolkit.Args{"address": addr})
	if err != nil {
		t.Fatal(err)
	}
	hex := strings.Fields(strings.Split(cr.Text, "\n")[1])[1]
	back, err := conv.Run(c, toolkit.Args{"address": hex})
	if err != nil || !strings.Contains(back.Text, addr) {
		t.Fatalf("hex→bech32 round trip: %v %v", back, err)
	}
	for _, bad := range []string{"0xzz", "0x1234"} {
		if _, err := conv.Run(c, toolkit.Args{"address": bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := rm.Run(c, toolkit.Args{"name": "ops"}); err != nil {
		t.Fatal(err)
	}
	if _, err := show.Run(c, toolkit.Args{"name": "ops"}); err == nil {
		t.Fatal("key still present after rm")
	}
}
