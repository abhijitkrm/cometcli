package toolkit

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
)

func TestFeeDefaultsFromTheNodesAppToml(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, "config"), 0o755)
	_ = os.WriteFile(filepath.Join(home, "config", "app.toml"), []byte("minimum-gas-prices = \"10000000000adex\"\n"), 0o644)
	c := &Context{Context: context.Background(), Profile: &config.Profile{Name: "v", Home: home}}
	c.SetHost(&host.Local{})
	c.FeeDefaults()
	if c.Profile.Metadata["fee_denom"] != "adex" || c.Profile.Metadata["gas_price"] != "10000000000" {
		t.Fatalf("metadata = %v", c.Profile.Metadata)
	}
	// set values are kept
	c.Profile.Metadata = map[string]string{"fee_denom": "uatom", "gas_price": "0.1"}
	c.FeeDefaults()
	if c.Profile.Metadata["fee_denom"] != "uatom" {
		t.Fatal("an explicit fee denom was overwritten")
	}
}
