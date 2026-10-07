package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func testCtx(t *testing.T, approve bool) (*toolkit.Context, *[]string) {
	t.Helper()
	var asked []string
	c := &toolkit.Context{
		Context: context.Background(),
		Profile: &config.Profile{Name: "t", Binary: "evmd"},
		Approver: func(_ *toolkit.Context, p string, _ toolkit.Tier, _ map[string]any) (bool, error) {
			asked = append(asked, p)
			return approve, nil
		},
		AutoApproveBelow: toolkit.TierLocalChange,
		Session:          toolkit.NewSession(t.TempDir()),
	}
	c.SetHost(&host.Local{})
	return c, &asked
}

func TestBashCwdPersistsAndReadsAutoRun(t *testing.T) {
	c, asked := testCtx(t, true)
	dir := c.Session.Cwd("")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (Bash{}).Run(c, toolkit.Args{"command": "cd sub"}); err != nil {
		t.Fatal(err)
	}
	res, err := (Bash{}).Run(c, toolkit.Args{"command": "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(dir, "sub"))
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(res.Text))
	if got != want {
		t.Fatalf("cwd = %q, want %q", res.Text, want)
	}
	if len(*asked) != 0 {
		t.Fatalf("read-only commands asked: %v", *asked)
	}
}

func TestBashChangeAsksAndDenialStops(t *testing.T) {
	c, asked := testCtx(t, false)
	_, err := (Bash{}).Run(c, toolkit.Args{"command": "touch created.txt"})
	if err == nil || len(*asked) != 1 {
		t.Fatalf("err=%v asked=%v", err, *asked)
	}
	if _, serr := os.Stat(filepath.Join(c.Session.Cwd(""), "created.txt")); serr == nil {
		t.Fatal("denied command ran")
	}
}

func TestBashForbiddenNeverAsks(t *testing.T) {
	c, asked := testCtx(t, true)
	_, err := (Bash{}).Run(c, toolkit.Args{"command": "cat config/priv_validator_key.json"})
	if err == nil || !strings.Contains(err.Error(), "refused") || len(*asked) != 0 {
		t.Fatalf("err=%v asked=%v", err, *asked)
	}
}

func TestBashExitCodeAndStderr(t *testing.T) {
	c, _ := testCtx(t, true)
	res, err := (Bash{}).Run(c, toolkit.Args{"command": "echo out; echo err >&2; false"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "out") || !strings.Contains(res.Text, "err") || !strings.Contains(res.Text, "[exit code 1]") {
		t.Fatalf("text = %q", res.Text)
	}
	if strings.Contains(res.Text, cwdMarker) {
		t.Fatal("cwd marker leaked")
	}
}

func TestBashTimeoutKillsProcessGroup(t *testing.T) {
	c, _ := testCtx(t, true)
	start := time.Now()
	res, err := (Bash{}).Run(c, toolkit.Args{"command": "sleep 30 | cat", "timeout": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second || !strings.Contains(res.Text, "killed after") {
		t.Fatalf("not killed promptly: %s %q", time.Since(start), res.Text)
	}
}

func TestBashScriptWithTxIsOnChain(t *testing.T) {
	c, asked := testCtx(t, false)
	dir := c.Session.Cwd("")
	script := filepath.Join(dir, "vote.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nevmd tx gov vote 1 yes --from v\n"), 0o755)
	c.AutoApproveBelow = toolkit.TierOnChain // bypass mode
	_, err := (Bash{}).Run(c, toolkit.Args{"command": "./vote.sh"})
	if err == nil || len(*asked) != 1 {
		t.Fatalf("script broadcasting a tx ran without asking in bypass: err=%v asked=%v", err, *asked)
	}
}

func TestSplitCwd(t *testing.T) {
	out, dir := splitCwd("hello\n\n" + cwdMarker + "/tmp/x\n")
	if out != "hello\n" || dir != "/tmp/x" {
		t.Fatalf("out=%q dir=%q", out, dir)
	}
	if out, dir := splitCwd("no marker"); out != "no marker" || dir != "" {
		t.Fatal("altered output without marker")
	}
}
