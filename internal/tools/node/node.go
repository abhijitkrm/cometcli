// Package node implements node.* tools: status, health, logs, peers,
// service control, and config inspection.
package node

import (
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

// Register adds all node.* tools to the registry.
func Register(r *toolkit.Registry) {
	r.Register(statusTool{})
	r.Register(healthTool{})
	r.Register(logsTool{})
	r.Register(peersTool{})
	r.Register(serviceTool{})
	r.Register(configShowTool{})
	r.Register(versionCheckTool{})
	r.Register(consensusTool{})
}

type statusTool struct{}

func (statusTool) Name() string { return "node.status" }
func (statusTool) Desc() string {
	return "Node sync status, height, version, peers, and validator info"
}
func (statusTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (statusTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (statusTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	cc, err := c.Comet()
	if err != nil {
		return nil, err
	}
	st, err := cc.Status(c)
	if err != nil {
		return nil, fmt.Errorf("comet status: %w", err)
	}
	s := st.SyncInfo
	ni := st.NodeInfo
	vi := st.ValidatorInfo
	var b strings.Builder
	fmt.Fprintf(&b, "node:    %s (%s)\n", ni.Moniker, ni.Network)
	fmt.Fprintf(&b, "version: cometbft %s\n", ni.Version)
	fmt.Fprintf(&b, "height:  %d  (latest block %s)\n", s.LatestBlockHeight, s.LatestBlockTime.Format("15:04:05Z"))
	fmt.Fprintf(&b, "syncing: %v\n", s.CatchingUp)
	fmt.Fprintf(&b, "valaddr: %x  voting power: %d\n", vi.Address, vi.VotingPower)
	return &toolkit.Result{Text: b.String(), Data: map[string]any{
		"moniker": ni.Moniker, "network": ni.Network, "version": ni.Version,
		"height": s.LatestBlockHeight, "catching_up": s.CatchingUp,
		"earliest_height": s.EarliestBlockHeight,
		"voting_power":    vi.VotingPower, "val_address": fmt.Sprintf("%x", vi.Address),
	}}, nil
}

type healthTool struct{}

func (healthTool) Name() string { return "node.health" }
func (healthTool) Desc() string {
	return "CometBFT /health + process/service liveness"
}
func (healthTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (healthTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (healthTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	var b strings.Builder
	healthy := true
	if cc, err := c.Comet(); err == nil {
		if err := cc.Health(c); err != nil {
			healthy = false
			fmt.Fprintf(&b, "rpc health: FAIL (%v)\n", err)
		} else {
			fmt.Fprintln(&b, "rpc health: OK")
		}
	} else {
		healthy = false
		fmt.Fprintf(&b, "rpc health: UNREACHABLE (%v)\n", err)
	}
	if h, err := c.Host(); err == nil && c.Profile.Service.Unit != "" {
		out, code, _ := svcRun(c, h, "status")
		fmt.Fprintf(&b, "service %s: %s (exit %d)\n", c.Profile.Service.Unit, common.OneLine(out), code)
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"healthy": healthy}}, nil
}

type logsTool struct{}

func (logsTool) Name() string { return "node.logs" }
func (logsTool) Desc() string {
	return "Tail node logs (journalctl/docker), optional grep filter"
}
func (logsTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"lines": toolkit.Int("number of lines (default 80)"),
		"grep":  toolkit.Str("substring filter"),
		"level": toolkit.Enum("log level filter", "error", "info", "debug"),
	})
}
func (logsTool) Tier() toolkit.Tier { return toolkit.TierDiagnose }

func (logsTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	n := a.Int("lines", 80)
	cmd := logCmd(c, int(n))
	if g := a.String("grep", ""); g != "" {
		cmd += " | grep -- " + common.ShellQ(g)
	}
	out, code, err := h.Run(c, cmd)
	c.LogShell(cmd, code)
	if err != nil && out == "" {
		// grep exit 1 = no matches — report cleanly, don't error the step.
		if a.String("grep", "") != "" && code == 1 {
			return &toolkit.Result{Text: "(no matches)", Data: map[string]any{"lines": ""}}, nil
		}
		return nil, err
	}
	return &toolkit.Result{Text: out, Data: map[string]any{"lines": out}}, nil
}

func logCmd(c *toolkit.Context, lines int) string {
	switch c.Profile.Service.Type {
	case "docker":
		return fmt.Sprintf("docker logs --tail %d %s 2>&1", lines, common.ShellQ(c.Profile.Service.Unit))
	case "launchd":
		return fmt.Sprintf("tail -n %d %s/*.log 2>&1", lines, c.Profile.Home)
	default: // systemd
		return fmt.Sprintf("journalctl -u %s -n %d --no-pager 2>&1", common.ShellQ(c.Profile.Service.Unit), lines)
	}
}

type peersTool struct{}

func (peersTool) Name() string { return "node.peers" }
func (peersTool) Desc() string {
	return "Peer list, count, and connectivity quality"
}
func (peersTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (peersTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (peersTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	cc, err := c.Comet()
	if err != nil {
		return nil, err
	}
	ni, err := cc.NetInfo(c)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "peers: %d (listening: %v)\n", ni.NPeers, ni.Listening)
	var list []map[string]any
	for _, p := range ni.Peers {
		fmt.Fprintf(&b, "  %s@%s  %s\n", p.NodeInfo.ID(), p.RemoteIP, p.NodeInfo.Moniker)
		list = append(list, map[string]any{"id": string(p.NodeInfo.ID()), "ip": p.RemoteIP, "moniker": p.NodeInfo.Moniker})
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"count": ni.NPeers, "listening": ni.Listening, "peers": list}}, nil
}

type serviceTool struct{}

func (serviceTool) Name() string { return "node.service" }
func (serviceTool) Desc() string {
	return "Control the node service: status|start|stop|restart"
}
func (serviceTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"action": toolkit.Enum("service action", "status", "start", "stop", "restart"),
	}, "action")
}
func (serviceTool) Tier() toolkit.Tier { return toolkit.TierLocalChange }

func (serviceTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	action := a.String("action", "status")
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	if action != "status" {
		if err := c.Approve(fmt.Sprintf("%s service %s on %s", action, c.Profile.Service.Unit, h), toolkit.TierLocalChange,
			map[string]any{"action": action, "unit": c.Profile.Service.Unit, "host": h.String()}); err != nil {
			return nil, err
		}
	}
	out, code, err := svcRun(c, h, action)
	c.LogShell("service "+action+" "+c.Profile.Service.Unit, code)
	if err != nil {
		return &toolkit.Result{Text: out, Data: map[string]any{"action": action, "exit": code, "output": out}}, nil
	}
	return &toolkit.Result{Text: common.OneLine(out), Data: map[string]any{"action": action, "exit": code, "output": out}}, nil
}

func svcRun(c *toolkit.Context, h host.Host, action string) (string, int, error) {
	unit := c.Profile.Service.Unit
	var cmd string
	switch c.Profile.Service.Type {
	case "docker":
		if action == "status" {
			cmd = "docker ps -a --filter name=" + common.ShellQ(unit)
		} else {
			cmd = "docker " + action + " " + common.ShellQ(unit)
		}
	case "launchd":
		if action == "status" {
			cmd = "launchctl list | grep -i " + common.ShellQ(unit)
		} else {
			cmd = "launchctl kickstart -k gui/$(id -u)/" + common.ShellQ(unit)
		}
	default: // systemd
		cmd = fmt.Sprintf("systemctl %s %s 2>&1", action, common.ShellQ(unit))
		if action == "status" {
			cmd += " | head -20"
		}
	}
	return h.Run(c, cmd)
}

type configShowTool struct{}

func (configShowTool) Name() string { return "node.config" }
func (configShowTool) Desc() string {
	return "Show or lint config.toml/app.toml; 'diff' compares to hardened baseline"
}
func (configShowTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"file":   toolkit.Enum("which config", "config.toml", "app.toml"),
		"action": toolkit.Enum("show | lint", "show", "lint"),
	})
}
func (configShowTool) Tier() toolkit.Tier { return toolkit.TierDiagnose }

func (configShowTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	file := a.String("file", "config.toml")
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("%s/config/%s", c.Profile.Home, file)
	raw, err := h.ReadFile(c, path)
	if err != nil {
		return nil, err
	}
	if a.String("action", "show") == "lint" {
		return lintConfig(c, file, string(raw)), nil
	}
	return &toolkit.Result{Text: string(raw), Data: map[string]any{"file": file}}, nil
}

type versionCheckTool struct{}

func (versionCheckTool) Name() string { return "node.version-check" }
func (versionCheckTool) Desc() string {
	return "Compare running binary version to the upstream git release"
}
func (versionCheckTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"repo": toolkit.Str("github repo, e.g. cosmos/evm (default: profile metadata.repo)"),
	})
}
func (versionCheckTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (versionCheckTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	cc, err := c.Comet()
	if err != nil {
		return nil, err
	}
	st, err := cc.Status(c)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "running cometbft: %s\n", st.NodeInfo.Version)
	repo := a.String("repo", c.Profile.Metadata["repo"])
	if repo != "" {
		latest, err := common.LatestRelease(c, repo)
		if err != nil {
			fmt.Fprintf(&b, "latest release: error: %v\n", err)
		} else {
			fmt.Fprintf(&b, "latest %s: %s\n", repo, latest)
		}
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"cometbft_version": st.NodeInfo.Version}}, nil
}
