package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func TestSetConfig(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, "config"), 0o755)
	cfg := "# node\nmoniker = \"a\"\n\n[mempool]\n# the type\ntype = \"flood\"\nsize = 5000\n"
	_ = os.WriteFile(filepath.Join(home, "config", "config.toml"), []byte(cfg), 0o600)
	var asked []string
	c := &toolkit.Context{Context: context.Background(), Profile: &config.Profile{Name: "n", Home: home},
		Approver: func(_ *toolkit.Context, p string, _ toolkit.Tier, _ map[string]any) (bool, error) {
			asked = append(asked, p)
			return true, nil
		}}
	c.SetHost(&host.Local{})
	res, err := setConfigTool{}.Run(c, toolkit.Args{"file": "config.toml", "key": "mempool.type", "value": "app"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(home, "config", "config.toml"))
	if !strings.Contains(string(got), "# the type\ntype = \"app\"\nsize = 5000") || !strings.Contains(res.Text, "was flood") {
		t.Fatalf("edit:\n%s\n%s", got, res.Text)
	}
	if fi, _ := os.Stat(filepath.Join(home, "config", "config.toml")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode changed: %v", fi.Mode())
	}
	if bak, _ := os.ReadFile(filepath.Join(home, "config", "config.toml.cometcli-bak")); string(bak) != cfg {
		t.Error("previous file not kept")
	}
	if len(asked) != 1 || !strings.Contains(asked[0], `mempool.type = "app" (was flood)`) {
		t.Errorf("approval: %v", asked)
	}
	// unchanged: no approval, no write
	if res, _ := (setConfigTool{}).Run(c, toolkit.Args{"file": "config.toml", "key": "mempool.type", "value": "app"}); !strings.Contains(res.Text, "nothing to change") || len(asked) != 1 {
		t.Errorf("no-op: %s", res.Text)
	}
	if _, err := (setConfigTool{}).Run(c, toolkit.Args{"file": "config.toml", "key": "mempool.size", "value": `["a" "b"]`}); err == nil {
		t.Error("a value that breaks the TOML must be refused")
	}
}
