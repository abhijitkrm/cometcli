// Package nettool implements net.* tools: persistent peer management.
package nettool

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
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

func editPeers(c *toolkit.Context, peer string, add bool) (*toolkit.Result, error) {
	peer = strings.TrimSpace(peer)
	if !peerRe.MatchString(peer) {
		return nil, fmt.Errorf("peer %q: want <40-hex node id>@host:port", peer)
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
	cfg := string(raw)
	loc := peersLineRe.FindStringSubmatchIndex(cfg)
	if loc == nil {
		return nil, fmt.Errorf("no persistent_peers = \"…\" line in %s", path)
	}
	var clean []string
	for _, p := range strings.Split(cfg[loc[4]:loc[5]], ",") {
		if p = strings.TrimSpace(p); p != "" {
			clean = append(clean, p)
		}
	}
	present := false
	for _, p := range clean {
		if p == peer {
			present = true
		}
	}
	verb, done := "add", "added"
	switch {
	case add && present:
		return &toolkit.Result{Text: "peer already present — nothing to change", Data: map[string]any{"peers": clean}}, nil
	case !add && !present:
		return &toolkit.Result{Text: "peer not present — nothing to change", Data: map[string]any{"peers": clean}}, nil
	case add:
		clean = append(clean, peer)
	default:
		verb, done = "remove", "removed"
		var keep []string
		for _, p := range clean {
			if p != peer {
				keep = append(keep, p)
			}
		}
		clean = keep
	}
	updated := cfg[:loc[4]] + strings.Join(clean, ",") + cfg[loc[5]:]
	if err := c.Approve(fmt.Sprintf("%s persistent peer %s", verb, peer), toolkit.TierLocalChange,
		map[string]any{"peer": peer, "peers_after": clean, "diff": toolkit.Diff(path, cfg, updated)}); err != nil {
		return nil, err
	}
	mode := os.FileMode(0o600)
	if fi, err := h.Stat(c, path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := h.WriteFile(c, path, []byte(updated), mode); err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: fmt.Sprintf("%s %s — %d persistent peer(s); restart the node to apply", done, peer, len(clean)),
		Data: map[string]any{"peers": clean}}, nil
}
