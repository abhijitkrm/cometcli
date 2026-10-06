package runbook

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

type stub struct {
	name string
	err  error
	ran  *[]string
	op   bool
}

func (s stub) Name() string           { return s.name }
func (s stub) Desc() string           { return s.name }
func (s stub) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (s stub) Tier() toolkit.Tier     { return toolkit.TierObserve }
func (s stub) OperatorOnly() bool     { return s.op }
func (s stub) Run(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
	*s.ran = append(*s.ran, s.name)
	return &toolkit.Result{Text: s.name + " ok"}, s.err
}

func setup() (*toolkit.Registry, *[]string) {
	var ran []string
	reg := toolkit.NewRegistry()
	reg.Register(stub{name: "a.ok", ran: &ran})
	reg.Register(stub{name: "a.fail", ran: &ran, err: errors.New("boom")})
	reg.Register(stub{name: "keys.add", ran: &ran, op: true})
	return reg, &ran
}

func TestRunStepsOptionalAndManual(t *testing.T) {
	reg, ran := setup()
	asked := 0
	c := &toolkit.Context{Context: context.Background(), Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { asked++; return true, nil }}
	rb := &Runbook{Name: "r", Steps: []Step{
		{Name: "one", Tool: "a.ok"},
		{Name: "may fail", Tool: "a.fail", Optional: true},
		{Name: "human", Manual: "restart the sentry"},
		{Name: "note only", Note: "just a note"},
		{Name: "two", Tool: "a.ok"},
	}}
	var out bytes.Buffer
	if err := Run(rb, c, reg, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*ran, ",") != "a.ok,a.fail,a.ok" || asked != 1 || !strings.Contains(out.String(), "✓ runbook complete") {
		t.Fatalf("ran=%v asked=%d out=%s", *ran, asked, out.String())
	}
	*ran = nil
	rb.Steps[1].Optional = false
	if err := Run(rb, c, reg, &out); err == nil || !strings.Contains(err.Error(), `step "may fail" failed`) || len(*ran) != 2 {
		t.Fatalf("required failure: %v ran=%v", err, *ran)
	}
}

func TestRunValidatesBeforeRunningAnything(t *testing.T) {
	reg, ran := setup()
	c := &toolkit.Context{Context: context.Background()}
	rb := &Runbook{Name: "r", Steps: []Step{{Name: "one", Tool: "a.ok"}, {Name: "typo", Tool: "a.okk"}}}
	if err := Run(rb, c, reg, &bytes.Buffer{}); err == nil || len(*ran) != 0 {
		t.Fatalf("unknown tool must fail before step 1 runs: %v ran=%v", err, *ran)
	}
	// key management steps: fine for the operator, refused for the agent
	rb = &Runbook{Name: "keys", Steps: []Step{{Name: "add", Tool: "keys.add"}}}
	if err := Run(rb, c, reg, &bytes.Buffer{}); err != nil {
		t.Fatalf("operator run: %v", err)
	}
	c.ToolName = "runbook.run"
	*ran = nil
	if err := Run(rb, c, reg, &bytes.Buffer{}); err == nil || len(*ran) != 0 {
		t.Fatalf("agent ran a key-management step: %v", err)
	}
}

func TestCustomRunbooksLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMETCLI_HOME", home)
	os.MkdirAll(filepath.Join(home, "runbooks"), 0o700)
	os.WriteFile(filepath.Join(home, "runbooks", "mine.yaml"), []byte("name: my-check\ndesc: checks\nsteps:\n  - name: status\n    tool: node.status\n  - name: human\n    manual: do it\n"), 0o600)
	rb, err := GetAll("my-check")
	if err != nil || len(rb.Steps) != 2 || rb.Steps[1].Manual != "do it" {
		t.Fatalf("custom = %+v %v", rb, err)
	}
	if len(Builtins()) == 0 {
		t.Fatal("no builtin runbooks")
	}
	for _, b := range Builtins() {
		if _, err := GetAll(b.Name); err != nil {
			t.Errorf("builtin %s: %v", b.Name, err)
		}
	}
}
