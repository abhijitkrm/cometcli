// Package nettool implements net.* tools: persistent peer management.
package nettool

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

// Register adds net.* tools.
func Register(r *toolkit.Registry) {
	r.Register(addPeerTool{})
	r.Register(rmPeerTool{})
}

type addPeerTool struct{}

func (addPeerTool) Name() string { return "net.add-peer" }
func (addPeerTool) Desc() string {
	return "Add a persistent peer to config.toml"
}
func (addPeerTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"peer": toolkit.Str("node_id@host:port"),
	}, "peer")
}
func (addPeerTool) Tier() toolkit.Tier { return toolkit.TierLocalChange }

func (addPeerTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	return editPeers(c, a.String("peer", ""), true)
}

type rmPeerTool struct{}

func (rmPeerTool) Name() string { return "net.rm-peer" }
func (rmPeerTool) Desc() string {
	return "Remove a persistent peer from config.toml"
}
func (rmPeerTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"peer": toolkit.Str("node_id@host:port"),
	}, "peer")
}
func (rmPeerTool) Tier() toolkit.Tier { return toolkit.TierLocalChange }

func (rmPeerTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	return editPeers(c, a.String("peer", ""), false)
}

// peerRe is node_id@host:port (a 40-hex-char CometBFT node id).
var (
	peerRe      = regexp.MustCompile(`^[0-9a-f]{40}@[^@:\s]+:[0-9]{1,5}$`)
	peersLineRe = regexp.MustCompile(`(?m)^(\s*persistent_peers\s*=\s*")([^"]*)(".*)$`)
)

func editPeers(c *toolkit.Context, peerArg string, add bool) (*toolkit.Result, error) {
	var peers []string
	for _, p := range strings.Split(peerArg, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if !peerRe.MatchString(p) {
			return nil, fmt.Errorf("peer %q: want <40-hex node id>@host:port", p)
		}
		peers = append(peers, p)
	}
	if len(peers) == 0 {
		return nil, fmt.Errorf("no peer given (node_id@host:port, comma-separated for several)")
	}
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	raw, where, err := common.ReadNodeFile(c, h, "config/config.toml")
	if err != nil {
		return nil, err
	}
	cfg := string(raw)
	loc := peersLineRe.FindStringSubmatchIndex(cfg)
	if loc == nil {
		return nil, fmt.Errorf("no persistent_peers = \"…\" line in %s", where)
	}
	var clean []string
	have := map[string]bool{}
	for _, p := range strings.Split(cfg[loc[4]:loc[5]], ",") {
		if p = strings.TrimSpace(p); p != "" {
			clean = append(clean, p)
			have[p] = true
		}
	}
	verb, done := "add", "added"
	var changed []string
	if add {
		for _, p := range peers {
			// the same node id at another address replaces nothing: skip known ids
			id, _, _ := strings.Cut(p, "@")
			known := false
			for _, q := range clean {
				if strings.HasPrefix(q, id+"@") {
					known = true
				}
			}
			if !known {
				clean = append(clean, p)
				changed = append(changed, p)
			}
		}
	} else {
		verb, done = "remove", "removed"
		drop := map[string]bool{}
		for _, p := range peers {
			if have[p] {
				drop[p] = true
				changed = append(changed, p)
			}
		}
		var keep []string
		for _, p := range clean {
			if !drop[p] {
				keep = append(keep, p)
			}
		}
		clean = keep
	}
	if len(changed) == 0 {
		return &toolkit.Result{Text: "nothing to change — " + map[bool]string{true: "already present", false: "not present"}[add], Data: map[string]any{"peers": clean}}, nil
	}
	updated := cfg[:loc[4]] + strings.Join(clean, ",") + cfg[loc[5]:]
	if err := c.Approve(fmt.Sprintf("%s persistent peer(s) %s", verb, strings.Join(changed, ", ")), toolkit.TierLocalChange,
		map[string]any{"peers_after": clean, "diff": toolkit.Diff(where, cfg, updated)}); err != nil {
		return nil, err
	}
	if _, err := common.WriteNodeFile(c, h, "config/config.toml", []byte(updated), 0o644); err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: fmt.Sprintf("%s %s — %d persistent peer(s); restart the node to apply", done, strings.Join(changed, ", "), len(clean)),
		Data: map[string]any{"peers": clean}}, nil
}
