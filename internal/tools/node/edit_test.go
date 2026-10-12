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

func TestRepairTOML(t *testing.T) {
	good := "moniker = \"a\"\n\n[consensus]\ntimeout_commit = \"1s\"\n"
	cases := []struct {
		name, doc string
		disabled  int
		fail      bool
	}{
		{"parses", good, 0, false},
		{"a stray table header (the drill)", good + "\n[consensus\n", 1, false},
		{"a key with no value", good + "size =\n", 1, false},
		{"a duplicate key", good + "timeout_commit = \"2s\"\n", 1, false},
		{"too much broken", good + "a =\nb =\nc =\nd =\n", 0, true},
	}
	for _, x := range cases {
		out, disabled, err := RepairTOML(x.doc, maxRepairLines)
		if (err != nil) != x.fail || len(disabled) != x.disabled {
			t.Errorf("%s: disabled %v, err %v", x.name, disabled, err)
			continue
		}
		if !x.fail && (parses(out) != nil || !strings.Contains(out, `timeout_commit = "1s"`)) {
			t.Errorf("%s: result:\n%s", x.name, out)
		}
	}
}

func TestRepairConfig(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, "config"), 0o755)
	p := filepath.Join(home, "config", "config.toml")
	broken := "moniker = \"a\"\n\n[consensus]\ntimeout_commit = \"1s\"\n\n[consensus\n"
	_ = os.WriteFile(p, []byte(broken), 0o644)
	var asked []map[string]any
	c := &toolkit.Context{Context: context.Background(), Profile: &config.Profile{Name: "n", Home: home},
		Approver: func(_ *toolkit.Context, _ string, _ toolkit.Tier, d map[string]any) (bool, error) {
			asked = append(asked, d)
			return true, nil
		}}
	c.SetHost(&host.Local{})
	res, err := repairConfigTool{}.Run(c, toolkit.Args{"file": "config.toml"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if parses(string(got)) != nil || !strings.Contains(string(got), "# cometcli: disabled, didn't parse: [consensus") || !strings.Contains(res.Text, "line 6") {
		t.Fatalf("repair:\n%s\n%s", got, res.Text)
	}
	if bak, _ := os.ReadFile(p + ".cometcli-bak"); string(bak) != broken {
		t.Error("the broken file must be kept")
	}
	if len(asked) != 1 || !strings.Contains(asked[0]["diff"].(string), "[consensus") {
		t.Errorf("approval: %v", asked)
	}
	if res, _ := (repairConfigTool{}).Run(c, toolkit.Args{"file": "config.toml"}); !strings.Contains(res.Text, "nothing to repair") || len(asked) != 1 {
		t.Errorf("second run: %s", res.Text)
	}
	// beyond a patch: the copy cometcli kept at its last edit
	_ = os.WriteFile(p+".cometcli-bak", []byte("moniker = \"good\"\n"), 0o644)
	_ = os.WriteFile(p, []byte("a =\nb =\nc =\nd =\n"), 0o644)
	if res, err := (repairConfigTool{}).Run(c, toolkit.Args{"file": "config.toml"}); err != nil || !strings.Contains(res.Text, "restore the copy") {
		t.Fatalf("backup: %v %v", res, err)
	}
	if got, _ := os.ReadFile(p); string(got) != "moniker = \"good\"\n" {
		t.Errorf("restored: %s", got)
	}
	// nothing to fall back on: refused, nothing written
	_ = os.WriteFile(p, []byte("a =\nb =\nc =\nd =\n"), 0o644)
	_ = os.WriteFile(p+".cometcli-bak", []byte("x =\n"), 0o644)
	if _, err := (repairConfigTool{}).Run(c, toolkit.Args{"file": "config.toml"}); err == nil {
		t.Error("an unrepairable file must be refused")
	}
}
