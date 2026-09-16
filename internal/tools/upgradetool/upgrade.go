// Package upgradetool implements upgrade.* tools: check plans, stage
// binaries into cosmovisor, and watch upgrade heights.
package upgradetool

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	upgradev1beta1 "cosmossdk.io/api/cosmos/upgrade/v1beta1"

	"github.com/abhijitkrm/cometcli/internal/tools/common"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Register adds all upgrade.* tools.
func Register(r *toolkit.Registry) {
	r.Register(checkTool{})
	r.Register(prepareTool{})
	r.Register(watchTool{})
}

type checkTool struct{}

func (checkTool) Name() string { return "upgrade.check" }
func (checkTool) Desc() string {
	return "On-chain upgrade plan + local binary version + latest release"
}
func (checkTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"repo": toolkit.Str("github repo for releases (default: metadata.repo)"),
	})
}
func (checkTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (checkTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	var b strings.Builder
	data := map[string]any{}
	if g, err := c.GRPC(); err == nil {
		if res, err := g.Upgrade.CurrentPlan(c, &upgradev1beta1.QueryCurrentPlanRequest{}); err == nil {
			if res.Plan != nil {
				fmt.Fprintf(&b, "on-chain plan: %s at height %d\n", res.Plan.Name, res.Plan.Height)
				data["plan"] = map[string]any{"name": res.Plan.Name, "height": res.Plan.Height}
			} else {
				fmt.Fprintln(&b, "on-chain plan: none")
			}
		}
	}
	if h, err := c.Host(); err == nil && c.Profile.Binary != "" {
		if out, code, err := h.Run(c, c.Profile.Binary+" version 2>/dev/null || "+c.Profile.Binary+" version --long 2>/dev/null | head -5"); err == nil {
			fmt.Fprintf(&b, "local binary:   %s\n", common.OneLine(out))
			data["local_version"] = strings.TrimSpace(out)
			c.LogShell("binary version", code)
		}
	}
	repo := a.String("repo", c.Profile.Metadata["repo"])
	if repo != "" {
		if rel, err := common.LatestRelease(c, repo); err == nil {
			fmt.Fprintf(&b, "latest release: %s (%s)\n", rel, repo)
			data["latest_release"] = rel
		}
	}
	return &toolkit.Result{Text: b.String(), Data: data}, nil
}

type prepareTool struct{}

func (prepareTool) Name() string { return "upgrade.prepare" }
func (prepareTool) Desc() string {
	return "Download + checksum-verify a release binary and stage it for cosmovisor"
}
func (prepareTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"name":       toolkit.Str("upgrade name (from the plan), e.g. v3"),
		"url":        toolkit.Str("release asset URL (default: repo release for this platform)"),
		"repo":       toolkit.Str("github repo (default: metadata.repo)"),
		"tag":        toolkit.Str("release tag (default: latest)"),
		"checksum":   toolkit.Str("expected sha256 hex — verify before staging"),
	}, "name")
}
func (prepareTool) Tier() toolkit.Tier { return toolkit.TierLocalChange }

func (prepareTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	name := a.String("name", "")
	binary := c.Profile.Binary
	if binary == "" {
		return nil, fmt.Errorf("profile %q has no binary configured", c.Profile.Name)
	}
	url := a.String("url", "")
	if url == "" {
		repo := a.String("repo", c.Profile.Metadata["repo"])
		if repo == "" {
			return nil, fmt.Errorf("pass --url or set metadata.repo")
		}
		tag := a.String("tag", "")
		if tag == "" {
			var err error
			tag, err = common.LatestRelease(c, repo)
			if err != nil {
				return nil, err
			}
		}
		// conventional asset name: <binary>_<tag>_<os>_<arch>.tar.gz
		url = fmt.Sprintf("https://github.com/%s/releases/download/%s/%s_%s_%s_%s.tar.gz",
			repo, tag, binary, strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
	}
	detail := map[string]any{"url": url, "upgrade": name, "binary": binary}
	if err := c.Approve(fmt.Sprintf("download %s and stage for cosmovisor upgrade %q", url, name),
		toolkit.TierLocalChange, detail); err != nil {
		return nil, err
	}
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	// download on the host, extract, verify checksum, stage into cosmovisor
	dir := fmt.Sprintf("%s/cosmovisor/upgrades/%s/bin", c.Profile.Home, name)
	cmd := fmt.Sprintf(`set -e
mkdir -p %s
cd "$(mktemp -d)"
curl -fsSL %s -o asset.tar.gz
tar xzf asset.tar.gz
`, common.ShellQ(dir), common.ShellQ(url))
	if cs := a.String("checksum", ""); cs != "" {
		cmd += fmt.Sprintf("echo '%s  ./%s' | sha256sum -c -\n", cs, binary)
	}
	cmd += fmt.Sprintf("install -m 0755 ./%s %s/\n", common.ShellQ(binary), common.ShellQ(dir))
	out, code, err := h.Run(c, cmd)
	c.LogShell("upgrade.prepare "+name, code)
	if err != nil {
		return &toolkit.Result{Text: "FAILED: " + err.Error() + "\n" + out}, err
	}
	// verify sha256 of staged binary for the audit trail
	sum, _, _ := h.Run(c, fmt.Sprintf("shasum -a 256 %s/%s 2>/dev/null || sha256sum %s/%s",
		common.ShellQ(dir), binary, common.ShellQ(dir), binary))
	return &toolkit.Result{
		Text: fmt.Sprintf("staged %s → %s\nsha256: %s", url, dir+"/"+binary, strings.TrimSpace(sum)),
		Data: map[string]any{"dir": dir, "sha256": sha256Str(sum)},
	}, nil
}

func sha256Str(s string) string {
	f := strings.Fields(s)
	if len(f) > 0 {
		return f[0]
	}
	return s
}

type watchTool struct{}

func (watchTool) Name() string { return "upgrade.watch" }
func (watchTool) Desc() string {
	return "Watch block height until the upgrade height, then alert"
}
func (watchTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"height":   toolkit.Int("upgrade height (default: on-chain plan)"),
		"interval": toolkit.Int("poll seconds (default 10)"),
	})
}
func (watchTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (watchTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	var target int64 = a.Int("height", 0)
	if target == 0 {
		g, err := c.GRPC()
		if err != nil {
			return nil, err
		}
		res, err := g.Upgrade.CurrentPlan(c, &upgradev1beta1.QueryCurrentPlanRequest{})
		if err != nil || res.Plan == nil {
			return nil, fmt.Errorf("no on-chain plan — pass --height")
		}
		target = res.Plan.Height
	}
	cc, err := c.Comet()
	if err != nil {
		return nil, err
	}
	iv := time.Duration(a.Int("interval", 10)) * time.Second
	fmt.Fprintf(c.Out, "watching for upgrade height %d (every %s)\n", target, iv)
	for {
		st, err := cc.Status(c)
		if err == nil {
			h := st.SyncInfo.LatestBlockHeight
			remain := target - h
			fmt.Fprintf(c.Out, "\rheight %d  — %d blocks to go   ", h, remain)
			if h >= target {
				fmt.Fprintf(c.Out, "\n✓ upgrade height %d reached\n", target)
				return &toolkit.Result{Text: "upgrade height reached", Data: map[string]any{"height": h, "target": target}}, nil
			}
		}
		select {
		case <-c.Done():
			return nil, c.Err()
		case <-time.After(iv):
		}
	}
}

