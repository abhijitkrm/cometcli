// Package nettool implements net.* tools: persistent peer management.
package nettool

import (
	"fmt"
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

func editPeers(c *toolkit.Context, peer string, add bool) (*toolkit.Result, error) {
	if !strings.Contains(peer, "@") {
		return nil, fmt.Errorf("peer must be node_id@host:port")
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
	// find persistent_peers = "a,b,c" line
	idx := strings.Index(cfg, "persistent_peers")
	if idx < 0 {
		return nil, fmt.Errorf("no persistent_peers key in %s", path)
	}
	lineEnd := strings.IndexByte(cfg[idx:], '\n')
	if lineEnd < 0 {
		return nil, fmt.Errorf("unterminated persistent_peers line")
	}
	line := cfg[idx : idx+lineEnd]
	qi := strings.IndexByte(line, '"')
	qj := strings.LastIndexByte(line, '"')
	if qi < 0 || qj <= qi {
		return nil, fmt.Errorf("persistent_peers not a quoted string: %s", line)
	}
	peers := strings.Split(line[qi+1:qj], ",")
	var clean []string
	for _, p := range peers {
		if p = strings.TrimSpace(p); p != "" {
			clean = append(clean, p)
		}
	}
	verb, done := "add", "added"
	if add {
		for _, p := range clean {
			if p == peer {
				return &toolkit.Result{Text: "peer already present"}, nil
			}
		}
		clean = append(clean, peer)
	} else {
		verb, done = "remove", "removed"
		var keep []string
		for _, p := range clean {
			if p != peer {
				keep = append(keep, p)
			}
		}
		clean = keep
	}
	newLine := line[:qi+1] + strings.Join(clean, ",") + line[qj:]
	if err := c.Approve(fmt.Sprintf("%s persistent peer %s", verb, peer), toolkit.TierLocalChange,
		map[string]any{"peer": peer, "peers_after": clean}); err != nil {
		return nil, err
	}
	cfg = cfg[:idx] + newLine + cfg[idx+lineEnd:]
	if err := h.WriteFile(c, path, []byte(cfg), 0o644); err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: fmt.Sprintf("%s %s — %d persistent peer(s)", done, peer, len(clean)),
		Data: map[string]any{"peers": clean}}, nil
}
