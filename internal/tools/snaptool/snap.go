// Package snaptool implements snap.* tools: state-sync bootstrap and
// snapshot inventory.
package snaptool

import (
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/client/comet"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

// Register adds all snap.* tools.
func Register(r *toolkit.Registry) {
	r.Register(stateSyncTool{})
	r.Register(listTool{})
	r.Register(pruneTool{})
}

type stateSyncTool struct{}

func (stateSyncTool) Name() string { return "snap.statesync" }
func (stateSyncTool) Desc() string {
	return "Fetch a trust height/hash from a trusted RPC and write [statesync] config"
}
func (stateSyncTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"rpc":          toolkit.Str("trusted RPCs, comma-separated — must be reachable FROM THE NODE (docker: host.docker.internal or peer container names); extras via metadata.statesync_rpcs"),
		"height":       toolkit.Int("trust height (default: latest - 2000)"),
		"trust-period": toolkit.Str("trust period (default 168h)"),
		"apply":        toolkit.Bool("write to config.toml on the host (else print only)"),
	})
}
func (stateSyncTool) Tier() toolkit.Tier { return toolkit.TierLocalChange }

func (stateSyncTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	trustPeriod := a.String("trust-period", "168h")
	rpcs := a.String("rpc", c.Profile.Endpoints.Comet)
	if rpcs == "" {
		rpcs = "tcp://127.0.0.1:26657"
	}
	if extra := c.Profile.Metadata["statesync_rpcs"]; extra != "" {
		rpcs = rpcs + "," + extra
	}
	// fetch the trust block from the trusted RPC — the profile's own node
	// is usually the thing being rebuilt (its RPC may be down)
	cc, err := comet.New(strings.Split(rpcs, ",")[0])
	if err != nil {
		return nil, err
	}
	st, err := cc.Status(c)
	if err != nil {
		return nil, err
	}
	height := a.Int("height", 0)
	if height == 0 {
		height = st.SyncInfo.LatestBlockHeight - 2000
	}
	blk, err := cc.Block(c, &height)
	if err != nil {
		return nil, fmt.Errorf("fetch trust block %d: %w", height, err)
	}
	hexHash := fmt.Sprintf("%X", blk.BlockID.Hash)

	// CometBFT rejects single-server statesync configs at startup
	if len(strings.Split(rpcs, ",")) < 2 {
		return nil, fmt.Errorf("statesync needs at least 2 rpc_servers for light-client cross-checks — pass --rpc \"a,b\" or set metadata.statesync_rpcs")
	}
	cfg := fmt.Sprintf(`[statesync]
enable = true
rpc_servers = "%s"
trust_height = %d
trust_hash = "%s"
trust_period = "%s"
`, rpcs, height, hexHash, trustPeriod)

	if !a.Bool("apply", false) {
		return &toolkit.Result{Text: "proposed config:\n\n" + cfg, Data: map[string]any{
			"trust_height": height, "trust_hash": hexHash, "rpc_servers": rpcs,
		}}, nil
	}
	if err := c.Approve(fmt.Sprintf("write [statesync] into %s/config/config.toml (trust_height=%d)", c.Profile.Home, height),
		toolkit.TierLocalChange, map[string]any{"height": height, "hash": hexHash}); err != nil {
		return nil, err
	}
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	path := c.Profile.Home + "/config/config.toml"
	raw, err := h.ReadFile(c, path)
	if err != nil {
		return nil, err
	}
	updated := patchStatesync(string(raw), rpcs, height, hexHash, trustPeriod)
	if err := h.WriteFile(c, path, []byte(updated), 0o644); err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: fmt.Sprintf("statesync config applied (trust_height=%d)\n\n%s", height, cfg),
		Data: map[string]any{"applied": true, "trust_height": height}}, nil
}

// patchStatesync rewrites the [statesync] section of config.toml in place.
func patchStatesync(cfg, rpcs string, height int64, hash, trustPeriod string) string {
	lines := strings.Split(cfg, "\n")
	var out []string
	inSS := false
	setKeys := map[string]bool{}
	put := func(key, val string) {
		out = append(out, fmt.Sprintf("%s = %s", key, val))
		setKeys[key] = true
	}
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			if inSS {
				// flush unset keys before leaving the section
				if !setKeys["rpc_servers"] {
					put("rpc_servers", `"`+rpcs+`"`)
				}
				if !setKeys["trust_height"] {
					put("trust_height", fmt.Sprint(height))
				}
				if !setKeys["trust_hash"] {
					put("trust_hash", `"`+hash+`"`)
				}
				if !setKeys["trust_period"] {
					put("trust_period", `"`+trustPeriod+`"`)
				}
			}
			inSS = t == "[statesync]"
			out = append(out, ln)
			continue
		}
		if inSS {
			switch {
			case strings.HasPrefix(t, "enable"):
				out = append(out, "enable = true")
				setKeys["enable"] = true
				continue
			case strings.HasPrefix(t, "rpc_servers"):
				out = append(out, fmt.Sprintf(`rpc_servers = "%s"`, rpcs))
				setKeys["rpc_servers"] = true
				continue
			case strings.HasPrefix(t, "trust_height"):
				out = append(out, fmt.Sprintf("trust_height = %d", height))
				setKeys["trust_height"] = true
				continue
			case strings.HasPrefix(t, "trust_hash"):
				out = append(out, fmt.Sprintf(`trust_hash = "%s"`, hash))
				setKeys["trust_hash"] = true
				continue
			case strings.HasPrefix(t, "trust_period"):
				out = append(out, fmt.Sprintf(`trust_period = "%s"`, trustPeriod))
				setKeys["trust_period"] = true
				continue
			}
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

type listTool struct{}

func (listTool) Name() string { return "snap.list" }
func (listTool) Desc() string {
	return "List local snapshots under <home>/data/snapshots"
}
func (listTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (listTool) Tier() toolkit.Tier     { return toolkit.TierDiagnose }

func (listTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	out, code, err := h.Run(c, fmt.Sprintf("ls -la %s/data/snapshots/ 2>/dev/null; du -sh %s/data 2>/dev/null",
		common.ShellQ(c.Profile.Home), common.ShellQ(c.Profile.Home)))
	c.LogShell("snap.list", code)
	if err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: out, Data: map[string]any{"raw": out}}, nil
}

type pruneTool struct{}

func (pruneTool) Name() string { return "snap.prune" }
func (pruneTool) Desc() string {
	return "Advise on pruning strategy from data-dir growth and disk pressure"
}
func (pruneTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (pruneTool) Tier() toolkit.Tier     { return toolkit.TierDiagnose }

func (pruneTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	out, code, err := h.Run(c, fmt.Sprintf(`du -sb %s/data 2>/dev/null | awk '{print $1}'; df -P %s | tail -1 | awk '{print $5}'`,
		common.ShellQ(c.Profile.Home), common.ShellQ(c.Profile.Home)))
	c.LogShell("snap.prune", code)
	if err != nil {
		return nil, err
	}
	f := strings.Fields(out)
	var b strings.Builder
	var dataBytes int64
	if len(f) >= 1 {
		fmt.Sscan(f[0], &dataBytes)
		fmt.Fprintf(&b, "data dir: %.1f GiB\n", float64(dataBytes)/(1<<30))
	}
	if len(f) >= 2 {
		fmt.Fprintf(&b, "disk used: %s\n", f[1])
	}
	fmt.Fprintln(&b, `
pruning guidance (app.toml):
  pruning = "custom"
  pruning-keep-recent = "100"      # keep ~100 recent states
  pruning-interval = "19"          # prune every 19 blocks
  min-retain-blocks = "362880"     # ~state-sync snapshot cadence
snapshots (app.toml):
  snapshot-interval = "1500"       # serve statesync snapshots
  snapshot-keep-recent = "2"`)
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"data_bytes": dataBytes}}, nil
}
