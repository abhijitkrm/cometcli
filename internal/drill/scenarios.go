package drill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/triage"
)

// Env is what a scenario works with.
type Env struct {
	Net     *Net
	Cfg     *config.Config
	Reg     *toolkit.Registry
	Node    int
	Profile *config.Profile
	backup  map[string][]byte
	note    string
}

// Signals collects the named signal families for the scenario's node.
func (e *Env) Signals(ctx context.Context, names ...string) kb.Signals {
	c := &toolkit.Context{Context: ctx, Profile: e.Profile, Cfg: e.Cfg}
	defer c.Close()
	return triage.CollectFor(c, 5*time.Minute, names).Signals
}

// waitFor polls cond until it holds or timeout passes.
func waitFor(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
	return cond()
}

func (e *Env) file(rel string) string { return filepath.Join(e.Net.home(e.Node), rel) }

// save backs up a node file before a scenario changes it.
func (e *Env) save(rel string) error {
	raw, err := os.ReadFile(e.file(rel))
	if err != nil {
		return err
	}
	if e.backup == nil {
		e.backup = map[string][]byte{}
	}
	e.backup[rel] = raw
	return nil
}

func (e *Env) restoreFiles() {
	for rel, raw := range e.backup {
		_ = os.WriteFile(e.file(rel), raw, 0o644)
	}
}

func (e *Env) container() string { return Container(e.Node) }

// inConsensus: running, unjailed, bonded and signing recent blocks.
func (e *Env) inConsensus(ctx context.Context) (bool, string) {
	s := e.Signals(ctx, "node.", "val.", "proc.")
	ok := s["proc.running"] == true && s["val.jailed"] == false && s["node.catching_up"] == false
	if pct, has := s["val.signed_recent_pct"].(float64); !has || pct < 80 {
		ok = false
	}
	return ok, fmt.Sprintf("running=%v jailed=%v catching_up=%v signed=%v%%", s["proc.running"], s["val.jailed"], s["node.catching_up"], s["val.signed_recent_pct"])
}

// Scenario is one drill.
type Scenario struct {
	Name   string
	Desc   string
	Prompt string   // how an operator would describe it
	Expect []string // root cases that count as the right diagnosis
	Inject func(ctx context.Context, e *Env) error
	Ready  func(ctx context.Context, e *Env) bool // the fault is visible
	Verify func(ctx context.Context, e *Env) (bool, string)
}

var haltRe = regexp.MustCompile(`(?m)^halt-height = .*$`)

// Scenarios are the built-in drills, run against the last validator.
var Scenarios = []Scenario{
	{
		Name: "jail-downtime", Desc: "the node is stopped until it's jailed for downtime",
		Prompt: "%s stopped signing", Expect: []string{"val-jailed-downtime", "node-process-down", "node-rpc-down", "node-killed-sigkill"},
		Inject: func(ctx context.Context, e *Env) error { _, err := docker(ctx, "stop", e.container()); return err },
		Ready: func(ctx context.Context, e *Env) bool {
			return waitFor(ctx, 3*time.Minute, func() bool { return e.Signals(ctx, "val.")["val.jailed"] == true })
		},
		Verify: func(ctx context.Context, e *Env) (bool, string) { return e.inConsensus(ctx) },
	},
	{
		Name: "config-crash", Desc: "config.toml is broken and the node crash-loops",
		Prompt: "%s keeps restarting", Expect: []string{"cfg-parse-error", "node-exit-config-error"},
		Inject: func(ctx context.Context, e *Env) error {
			if err := e.save("config/config.toml"); err != nil {
				return err
			}
			if err := os.WriteFile(e.file("config/config.toml"), append(e.backup["config/config.toml"], []byte("\n[consensus\n")...), 0o644); err != nil {
				return err
			}
			_, err := docker(ctx, "restart", e.container())
			return err
		},
		Ready: func(ctx context.Context, e *Env) bool {
			return waitFor(ctx, 2*time.Minute, func() bool { return e.Signals(ctx, "config.")["config.parse_error"] == true })
		},
		Verify: func(ctx context.Context, e *Env) (bool, string) {
			if e.Signals(ctx, "config.")["config.parse_error"] == true {
				return false, "config.toml still doesn't parse"
			}
			return e.inConsensus(ctx)
		},
	},
	{
		Name: "halt-height", Desc: "app.toml halt-height stops the node a few blocks ahead",
		Prompt: "%s stopped producing blocks", Expect: []string{"upgrade-halt-height-set"},
		Inject: func(ctx context.Context, e *Env) error {
			if err := e.save("config/app.toml"); err != nil {
				return err
			}
			h := e.Net.Height(ctx, e.Node)
			raw := haltRe.ReplaceAll(e.backup["config/app.toml"], []byte(fmt.Sprintf("halt-height = %d", h+8)))
			if err := os.WriteFile(e.file("config/app.toml"), raw, 0o644); err != nil {
				return err
			}
			_, err := docker(ctx, "restart", e.container())
			return err
		},
		Ready: func(ctx context.Context, e *Env) bool {
			return waitFor(ctx, 2*time.Minute, func() bool {
				s := e.Signals(ctx, "node.", "app.")
				age, _ := s["node.block_age_s"].(float64)
				return age > 30 && s["app.halt_height"] != nil
			})
		},
		Verify: func(ctx context.Context, e *Env) (bool, string) {
			if hh, _ := e.Signals(ctx, "app.")["app.halt_height"].(float64); hh > 0 {
				return false, fmt.Sprintf("halt-height still %d", int64(hh))
			}
			return e.inConsensus(ctx)
		},
	},
	{
		Name: "oom", Desc: "the container's memory limit is too low and evmd is OOM-killed",
		Prompt: "%s restarts every few seconds, nothing in the logs", Expect: []string{"container-memory-limit", "host-oom-killed"},
		Inject: func(ctx context.Context, e *Env) error {
			if _, err := docker(ctx, "update", "--memory", "120m", "--memory-swap", "120m", e.container()); err != nil {
				return err
			}
			_, err := docker(ctx, "restart", e.container())
			return err
		},
		Ready: func(ctx context.Context, e *Env) bool {
			return waitFor(ctx, 2*time.Minute, func() bool {
				r, _ := e.Signals(ctx, "proc.")["proc.restarts"].(float64)
				return r >= 2
			})
		},
		Verify: func(ctx context.Context, e *Env) (bool, string) {
			if lim, ok := e.Signals(ctx, "proc.")["proc.mem_limit_mb"].(float64); ok && lim < 1024 {
				return false, fmt.Sprintf("memory limit still %.0f MB", lim)
			}
			return e.inConsensus(ctx)
		},
	},
	{
		Name: "partition", Desc: "the container is disconnected from the network",
		Prompt: "%s RPC times out", Expect: []string{"node-network-detached", "node-rpc-path-broken"},
		Inject: func(ctx context.Context, e *Env) error {
			_, err := docker(ctx, "network", "disconnect", project, e.container())
			return err
		},
		Ready: func(ctx context.Context, e *Env) bool {
			return waitFor(ctx, time.Minute, func() bool { return e.Signals(ctx, "proc.")["proc.networks"] == 0.0 })
		},
		Verify: func(ctx context.Context, e *Env) (bool, string) {
			if n, _ := e.Signals(ctx, "proc.")["proc.networks"].(float64); n < 1 {
				return false, "still detached"
			}
			return e.inConsensus(ctx)
		},
	},
}

// Recover puts the node back to health after a scenario, whatever the
// agent did: files restored, limits lifted, network reattached, started,
// synced and unjailed.
func (e *Env) Recover(ctx context.Context, unjail func(ctx context.Context) error) error {
	e.restoreFiles()
	_, _ = docker(ctx, "update", "--memory", "4g", "--memory-swap", "4g", e.container())
	if out, _ := docker(ctx, "inspect", "-f", "{{len .NetworkSettings.Networks}}", e.container()); strings.TrimSpace(out) == "0" {
		_, _ = docker(ctx, "network", "connect", "--ip", fmt.Sprintf("%s.%d", e.Net.Subnet, e.Node+2), project, e.container())
	}
	if _, err := docker(ctx, "restart", e.container()); err != nil {
		return err
	}
	if !waitFor(ctx, 3*time.Minute, func() bool { return e.Signals(ctx, "node.")["node.catching_up"] == false }) {
		return fmt.Errorf("%s didn't sync after recovery", e.Profile.Name)
	}
	if e.Signals(ctx, "val.")["val.jailed"] == true {
		waitFor(ctx, time.Minute, func() bool { r, _ := e.Signals(ctx, "val.")["val.jail_remaining_s"].(float64); return r == 0 })
		if err := unjail(ctx); err != nil {
			return fmt.Errorf("unjail during recovery: %w", err)
		}
	}
	if !waitFor(ctx, 2*time.Minute, func() bool { ok, _ := e.inConsensus(ctx); return ok }) {
		_, why := e.inConsensus(ctx)
		return fmt.Errorf("%s not back in consensus: %s", e.Profile.Name, why)
	}
	return nil
}
