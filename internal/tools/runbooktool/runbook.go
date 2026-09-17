// Package runbooktool exposes the runbook engine as runbook.* tools.
package runbooktool

import (
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/runbook"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Register adds runbook.* tools.
func Register(r *toolkit.Registry) {
	r.Register(listTool{})
	r.Register(showTool{})
	r.Register(runTool{reg: r})
}

type listTool struct{}

func (listTool) Name() string { return "runbook.list" }
func (listTool) Desc() string {
	return "List available runbooks (playbooks)"
}
func (listTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (listTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (listTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	var b strings.Builder
	var out []map[string]string
	for _, rb := range runbook.All() {
		fmt.Fprintf(&b, "%-22s %s\n", rb.Name, rb.Desc)
		out = append(out, map[string]string{"name": rb.Name, "desc": rb.Desc})
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"runbooks": out}}, nil
}

type showTool struct{}

func (showTool) Name() string { return "runbook.show" }
func (showTool) Desc() string {
	return "Print a runbook's steps without executing"
}
func (showTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"name": toolkit.Enum("runbook name", runbook.AllNames()...),
	}, "name")
}
func (showTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (showTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	rb, err := runbook.GetAll(a.String("name", ""))
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n\n", rb.Name, rb.Desc)
	for i, s := range rb.Steps {
		fmt.Fprintf(&b, "%2d. %s\n", i+1, s.Name)
		switch {
		case s.Manual != "":
			fmt.Fprintf(&b, "    manual: %s\n", s.Manual)
		case s.Tool != "":
			fmt.Fprintf(&b, "    tool: %s", s.Tool)
			if len(s.Args) > 0 {
				fmt.Fprintf(&b, " %v", s.Args)
			}
			if s.Optional {
				b.WriteString("  (optional)")
			}
			b.WriteString("\n")
		}
		if s.Note != "" {
			fmt.Fprintf(&b, "    note: %s\n", s.Note)
		}
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"runbook": rb.Name, "steps": len(rb.Steps)}}, nil
}

type runTool struct{ reg *toolkit.Registry }

func (runTool) Name() string { return "runbook.run" }
func (runTool) Desc() string {
	return "Execute a runbook: jail-recovery | halt-recovery | host-migration | statesync-bootstrap | coordinated-upgrade"
}
func (t runTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"name": toolkit.Enum("runbook name", runbook.AllNames()...),
	}, "name")
}
func (runTool) Tier() toolkit.Tier { return toolkit.TierLocalChange }

func (t runTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	rb, err := runbook.GetAll(a.String("name", ""))
	if err != nil {
		return nil, err
	}
	if err := c.Approve(fmt.Sprintf("execute runbook %q (%d steps)", rb.Name, len(rb.Steps)),
		toolkit.TierLocalChange, map[string]any{"runbook": rb.Name}); err != nil {
		return nil, err
	}
	var b strings.Builder
	if err := runbook.Run(rb, c, t.reg, &b); err != nil {
		return &toolkit.Result{Text: b.String() + "\nABORTED: " + err.Error()}, err
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"runbook": rb.Name, "done": true}}, nil
}
