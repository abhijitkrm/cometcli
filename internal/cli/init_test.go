package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMinGasPriceDenom(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, "config"), 0o755)
	for app, want := range map[string][2]string{
		"minimum-gas-prices = \"1000000000atest\"\n":       {"1000000000", "atest"},
		"minimum-gas-prices = \"0.025uatom,0.001stake\"\n": {"0.025", "uatom"},
		"pruning = \"default\"\n":                          {"", ""},
	} {
		_ = os.WriteFile(filepath.Join(home, "config", "app.toml"), []byte(app), 0o644)
		if a, d := minGasPriceDenom(home); a != want[0] || d != want[1] {
			t.Errorf("%q → %q %q, want %v", app, a, d, want)
		}
	}
	if a, d := minGasPriceDenom(t.TempDir()); a != "" || d != "" {
		t.Error("missing app.toml should yield nothing")
	}
}
