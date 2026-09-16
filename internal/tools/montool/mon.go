// Package montool implements mon.* tools: watch TUI, one-shot snapshot,
// and the alerting watcher loop.
package montool

import (
	"fmt"
	"time"

	"github.com/abhijitkrm/cometcli/internal/monitor"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tui"
)

// Register adds all mon.* tools.
func Register(r *toolkit.Registry) {
	r.Register(watchTool{})
	r.Register(snapshotTool{})
	r.Register(alertsTool{})
}

type watchTool struct{}

func (watchTool) Name() string { return "mon.watch" }
func (watchTool) Desc() string {
	return "Live dashboard: height, signing, peers, disk, jail status"
}
func (watchTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"interval": toolkit.Int("poll interval seconds (default 5)"),
	})
}
func (watchTool) Tier() toolkit.Tier { return toolkit.TierObserve }
func (watchTool) LongRunning() bool  { return true }

func (watchTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	iv := time.Duration(a.Int("interval", 5)) * time.Second
	if err := tui.RunWatch(c, iv); err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: "watch exited"}, nil
}

type snapshotTool struct{}

func (snapshotTool) Name() string { return "mon.snapshot" }
func (snapshotTool) Desc() string {
	return "One-shot health snapshot (JSON-friendly)"
}
func (snapshotTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (snapshotTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (snapshotTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	s := monitor.Collect(c)
	var txt string
	if !s.Reachable {
		txt = "UNREACHABLE"
	} else {
		txt = fmt.Sprintf("h=%d peers=%d missed=%d/%d jailed=%v disk=%.0f%%",
			s.Height, s.Peers, s.Missed, s.Window, s.Jailed, s.DiskUsedPct)
	}
	return &toolkit.Result{Text: txt, Data: map[string]any{"snapshot": s}}, nil
}

type alertsTool struct{}

func (alertsTool) Name() string { return "mon.alerts" }
func (alertsTool) Desc() string {
	return "Run the alert watcher: rules fire webhooks (Slack/Discord/Telegram)"
}
func (alertsTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"interval":         toolkit.Int("poll interval seconds (default 10)"),
		"missed-threshold": toolkit.Int("alert when missed blocks >= N (default 50)"),
		"disk-pct":         toolkit.Int("alert when disk used >= N%% (default 85)"),
		"stall-secs":       toolkit.Int("alert when height stalls > N secs (default 120)"),
	})
}
func (alertsTool) Tier() toolkit.Tier { return toolkit.TierDiagnose }
func (alertsTool) LongRunning() bool  { return true }

func (alertsTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	iv := time.Duration(a.Int("interval", 10)) * time.Second
	sinks := monitor.Sinks(c.Profile.Alerts)
	w := &monitor.Watcher{
		Ctx:      c,
		Interval: iv,
		Rules:    monitor.DefaultRules(a.Int("missed-threshold", 50), float64(a.Int("disk-pct", 85)), int(a.Int("stall-secs", 120))),
		Sinks:    sinks,
		OnEvent:  func(msg string, _ bool) { fmt.Fprintln(c.Out, "ALERT:", msg) },
	}
	fmt.Fprintf(c.Out, "watching %s — %d rule(s), %d sink(s), every %s\n",
		c.Profile.Name, len(w.Rules), len(sinks), iv)
	w.Run()
	return &toolkit.Result{Text: "watcher stopped"}, nil
}
