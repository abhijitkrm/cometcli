// Package fleet implements fleet.* tools: multi-profile health fan-out.
package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/monitor"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Register adds fleet.* tools.
func Register(r *toolkit.Registry) {
	r.Register(statusTool{})
	r.Register(execTool{reg: r})
	r.Register(shellTool{})
}

type statusTool struct{}

func (statusTool) Name() string { return "fleet.status" }
func (statusTool) Desc() string {
	return "Health matrix across all configured profiles"
}
func (statusTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"timeout": toolkit.Int("per-node timeout seconds (default 8)"),
	})
}
func (statusTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (statusTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	to := time.Duration(a.Int("timeout", 8)) * time.Second

	type row struct {
		name string
		snap *monitor.Snapshot
		err  string
	}
	var mu sync.Mutex
	var rows []row
	var wg sync.WaitGroup
	for name, p := range c.Cfg.Profiles {
		wg.Add(1)
		go func(name string, p *config.Profile) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(c, to)
			defer cancel()
			sub := &toolkit.Context{
				Context: ctx, Profile: p, Cfg: c.Cfg, Out: c.Out, Audit: c.Audit,
			}
			s := monitor.Collect(sub)
			mu.Lock()
			rows = append(rows, row{name, s, strings.Join(s.Errors, "; ")})
			mu.Unlock()
		}(name, p)
	}
	wg.Wait()

	var b strings.Builder
	fmt.Fprintf(&b, "%-18s %-6s %-8s %-6s %-14s %-6s %s\n",
		"PROFILE", "ROLE", "HEIGHT", "PEERS", "SIGNING", "DISK", "STATUS")
	for _, r := range rows {
		s := r.snap
		role := ""
		if c.Cfg.Profiles[r.name] != nil {
			role = c.Cfg.Profiles[r.name].Role
		}
		signing := "-"
		if s.Window > 0 {
			signing = fmt.Sprintf("%d/%d missed", s.Missed, s.Window)
		}
		status := "ok"
		switch {
		case !s.Reachable:
			status = "UNREACHABLE"
		case s.Tombstoned:
			status = "TOMBSTONED"
		case s.Jailed:
			status = "JAILED"
		case s.CatchingUp:
			status = "catching-up"
		case s.Peers == 0:
			status = "no peers"
		}
		fmt.Fprintf(&b, "%-18s %-6s %-8d %-6d %-14s %-5.0f%% %s\n",
			r.name, role, s.Height, s.Peers, signing, s.DiskUsedPct, status)
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"count": len(rows)}}, nil
}

// profilesSelected returns the subset of configured profiles matching the
// comma-separated --profiles filter (empty = all), in sorted order.
func profilesSelected(c *toolkit.Context, a toolkit.Args) []string {
	want := map[string]bool{}
	for _, s := range strings.Split(a.String("profiles", ""), ",") {
		if s = strings.TrimSpace(s); s != "" {
			want[s] = true
		}
	}
	var names []string
	for name := range c.Cfg.Profiles {
		if len(want) == 0 || want[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// execTool fans out a registry tool (observe/diagnose tier only) across profiles.
type execTool struct{ reg *toolkit.Registry }

func (execTool) Name() string { return "fleet.exec" }
func (execTool) Desc() string {
	return "Run a read-only tool across profiles: fleet exec --tool node.status [--profiles a,b] [--args '{\"lines\":50}']"
}
func (execTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"tool":     toolkit.Str("registry tool name, e.g. node.status (observe/diagnose only)"),
		"args":     toolkit.Str("tool args as JSON object, e.g. {\"lines\":50}"),
		"profiles": toolkit.Str("comma-separated profile filter (default: all)"),
		"timeout":  toolkit.Int("per-node timeout seconds (default 30)"),
	}, "tool")
}
func (execTool) Tier() toolkit.Tier { return toolkit.TierDiagnose }

func (t execTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	name := a.String("tool", "")
	tool, ok := t.reg.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	if tool.Tier() > toolkit.TierDiagnose {
		return nil, fmt.Errorf("refusing to fan out %q — tier %s mutates state; run it per-profile", name, tool.Tier())
	}
	var targs toolkit.Args
	if raw := a.String("args", ""); raw != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("bad --args JSON: %w", err)
		}
		targs = m
	}
	to := time.Duration(a.Int("timeout", 30)) * time.Second
	names := profilesSelected(c, a)
	if len(names) == 0 {
		return nil, fmt.Errorf("no profiles configured")
	}

	type row struct {
		name string
		text string
		err  string
	}
	var mu sync.Mutex
	var rows []row
	var wg sync.WaitGroup
	for _, pname := range names {
		p := c.Cfg.Profiles[pname]
		wg.Add(1)
		go func(pname string, p *config.Profile) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(c, to)
			defer cancel()
			sub := &toolkit.Context{
				Context: ctx, Profile: p, Cfg: c.Cfg, Out: c.Out, Audit: c.Audit,
			}
			res, err := tool.Run(sub, targs)
			r := row{name: pname}
			if err != nil {
				r.err = err.Error()
			} else if res != nil {
				r.text = strings.TrimRight(res.Text, "\n")
			}
			mu.Lock()
			rows = append(rows, r)
			mu.Unlock()
		}(pname, p)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "=== %s ===\n", r.name)
		if r.err != "" {
			fmt.Fprintf(&b, "error: %s\n", r.err)
		} else {
			fmt.Fprintf(&b, "%s\n", r.text)
		}
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"tool": name, "count": len(rows)}}, nil
}

// shellTool runs a shell command on each profile's host (local-change tier).
type shellTool struct{}

func (shellTool) Name() string { return "fleet.shell" }
func (shellTool) Desc() string {
	return "Run a shell command on every profile's host (prompts once)"
}
func (shellTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"cmd":      toolkit.Str("shell command to run on each host"),
		"profiles": toolkit.Str("comma-separated profile filter (default: all)"),
		"timeout":  toolkit.Int("per-node timeout seconds (default 30)"),
	}, "cmd")
}
func (shellTool) Tier() toolkit.Tier { return toolkit.TierLocalChange }

func (shellTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	cmd := a.String("cmd", "")
	names := profilesSelected(c, a)
	if len(names) == 0 {
		return nil, fmt.Errorf("no profiles configured")
	}
	if err := c.Approve(fmt.Sprintf("run on %d host(s): %s", len(names), cmd),
		toolkit.TierLocalChange, map[string]any{"cmd": cmd, "profiles": names}); err != nil {
		return nil, err
	}
	to := time.Duration(a.Int("timeout", 30)) * time.Second

	type row struct {
		name, out, err string
	}
	var mu sync.Mutex
	var rows []row
	var wg sync.WaitGroup
	for _, pname := range names {
		p := c.Cfg.Profiles[pname]
		wg.Add(1)
		go func(pname string, p *config.Profile) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(c, to)
			defer cancel()
			sub := &toolkit.Context{
				Context: ctx, Profile: p, Cfg: c.Cfg, Out: c.Out, Audit: c.Audit,
			}
			r := row{name: pname}
			h, err := sub.Host()
			if err != nil {
				r.err = err.Error()
			} else {
				out, code, err := h.Run(ctx, cmd)
				switch {
				case err != nil:
					r.err = err.Error()
				case code != 0:
					r.err = fmt.Sprintf("exit %d: %s", code, strings.TrimSpace(out))
				default:
					r.out = strings.TrimRight(out, "\n")
				}
			}
			mu.Lock()
			rows = append(rows, r)
			mu.Unlock()
		}(pname, p)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "=== %s ===\n", r.name)
		if r.err != "" {
			fmt.Fprintf(&b, "error: %s\n", r.err)
		} else {
			fmt.Fprintf(&b, "%s\n", r.out)
		}
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"cmd": cmd, "count": len(rows)}}, nil
}
