package fs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func ctx(t *testing.T) (*toolkit.Context, string, *int) {
	t.Helper()
	dir := t.TempDir()
	n := 0
	c := &toolkit.Context{
		Context: context.Background(),
		Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) {
			n++
			return true, nil
		},
		AutoApproveBelow: toolkit.TierLocalChange,
		Session:          toolkit.NewSession(dir),
		WorkRoot:         dir,
	}
	c.SetHost(&host.Local{})
	return c, dir, &n
}

func TestReadEditRoundTrip(t *testing.T) {
	c, dir, asked := ctx(t)
	p := filepath.Join(dir, "app.toml")
	os.WriteFile(p, []byte("[api]\nenable = true\nswagger = true\n"), 0o640)

	if _, err := (Edit{}).Run(c, toolkit.Args{"file_path": "app.toml", "old_string": "swagger = true", "new_string": "swagger = false"}); err == nil {
		t.Fatal("edit without read allowed")
	}
	res, err := (Read{}).Run(c, toolkit.Args{"file_path": "app.toml"})
	if err != nil || !strings.Contains(res.Text, "     2\tenable = true") {
		t.Fatalf("read = %q %v", res, err)
	}
	if _, err := (Edit{}).Run(c, toolkit.Args{"file_path": "app.toml", "old_string": "true", "new_string": "false"}); err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("ambiguous edit: %v", err)
	}
	res, err = (Edit{}).Run(c, toolkit.Args{"file_path": "app.toml", "old_string": "swagger = true", "new_string": "swagger = false"})
	if err != nil || *asked != 1 || !strings.Contains(res.Text, "+ swagger = false") {
		t.Fatalf("edit: %v asked=%d %q", err, *asked, res)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "[api]\nenable = true\nswagger = false\n" {
		t.Fatalf("content = %q", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %v", fi.Mode().Perm())
	}
	// an outside change invalidates the read
	os.WriteFile(p, []byte("changed\n"), 0o640)
	if _, err := (Edit{}).Run(c, toolkit.Args{"file_path": p, "old_string": "changed", "new_string": "x"}); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("stale edit: %v", err)
	}
}

func TestWriteNewFileAndAcceptEdits(t *testing.T) {
	c, dir, asked := ctx(t)
	c.AcceptEdits = true
	if _, err := (Write{}).Run(c, toolkit.Args{"file_path": "sub/new.conf", "content": "a=1\n"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sub", "new.conf")); string(b) != "a=1\n" || *asked != 0 {
		t.Fatalf("content=%q asked=%d", b, *asked)
	}
	other := filepath.Join(t.TempDir(), "x.conf")
	if _, err := (Write{}).Run(c, toolkit.Args{"file_path": other, "content": "b"}); err != nil || *asked != 1 {
		t.Fatalf("outside root must ask: %v asked=%d", err, *asked)
	}
}

func TestKeyMaterialRefused(t *testing.T) {
	c, dir, asked := ctx(t)
	os.MkdirAll(filepath.Join(dir, "config"), 0o700)
	key := filepath.Join(dir, "config", "priv_validator_key.json")
	os.WriteFile(key, []byte(`{"priv_key":"secret"}`), 0o600)
	for _, tc := range []struct {
		tool toolkit.Tool
		args toolkit.Args
	}{
		{Read{}, toolkit.Args{"file_path": key}},
		{Write{}, toolkit.Args{"file_path": key, "content": "x"}},
		{Edit{}, toolkit.Args{"file_path": key, "old_string": "secret", "new_string": "x"}},
		{Read{}, toolkit.Args{"file_path": filepath.Join(dir, ".mnemonics", "mnemonics.txt")}},
	} {
		if _, err := tc.tool.Run(c, tc.args); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s %v: err = %v", tc.tool.Name(), tc.args["file_path"], err)
		}
	}
	if *asked != 0 {
		t.Fatal("asked about key material instead of refusing")
	}
}

func TestGlobAndGrep(t *testing.T) {
	c, dir, _ := ctx(t)
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "config.toml"), []byte("moniker = \"val\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a", "b", "app.toml"), []byte("pruning = \"nothing\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".git", "x.toml"), []byte("moniker\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a", "node_key.json"), []byte("moniker secret\n"), 0o600)

	res, err := (Glob{}).Run(c, toolkit.Args{"pattern": "**/*.toml"})
	if err != nil || strings.Count(res.Text, "\n") != 1 || strings.Contains(res.Text, ".git") {
		t.Fatalf("glob = %q %v", res.Text, err)
	}
	res, err = (Grep{}).Run(c, toolkit.Args{"pattern": "moniker", "output_mode": "content"})
	if err != nil || !strings.Contains(res.Text, "config.toml:1:moniker") {
		t.Fatalf("grep = %q %v", res.Text, err)
	}
	if strings.Contains(res.Text, "node_key") || strings.Contains(res.Text, ".git") {
		t.Fatalf("grep searched excluded files: %q", res.Text)
	}
	res, _ = (Grep{}).Run(c, toolkit.Args{"pattern": "nomatch-xyz"})
	if !strings.HasPrefix(res.Text, "no matches") {
		t.Fatalf("no-match = %q", res.Text)
	}
}
