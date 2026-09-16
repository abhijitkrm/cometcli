// Package fleet implements fleet.* tools: multi-profile health fan-out.
package fleet

import (
	"context"
	"fmt"
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
