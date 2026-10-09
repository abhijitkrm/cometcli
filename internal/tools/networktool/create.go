package networktool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/keys"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// CreateOpts are the choices genesis create (and a drill network) make.
type CreateOpts struct {
	HomeFor   func(p *config.Profile) string // node home on its host
	Keyring   KeyringOpts
	Extra     []Account
	DockerNet string // every node on this docker network (one machine)
	PortBase  int    // host port offset of the first validator
	PortStep  int    // added per further validator
	Progress  func(string)
	// Mnemonic gets each new operator mnemonic; nil drops it (throwaway
	// networks).
	Mnemonic func(p *config.Profile, account, mnemonic string) error
}

func (o CreateOpts) say(format string, args ...any) {
	if o.Progress != nil {
		o.Progress(fmt.Sprintf(format, args...))
	}
}

// CreateNetwork runs the distributed genesis workflow for the spec (saved
// under its chain id) on the validators' hosts: keys made where they stay,
// a base genesis, a gentx signed on each host, collect and strict
// validation, then each node's genesis, configs and start. Each profile
// is updated to describe its node, signer and operator included.
func CreateNetwork(ctxFor func(*config.Profile) *toolkit.Context, spec *netspec.Spec, profiles []*config.Profile, o CreateOpts) ([]*GenesisNode, error) {
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no genesis validators")
	}
	var ctxs []*toolkit.Context
	defer func() {
		for _, c := range ctxs {
			c.Close()
		}
	}()
	if o.DockerNet != "" {
		c := ctxFor(profiles[0])
		ctxs = append(ctxs, c)
		h, err := c.Host()
		if err != nil {
			return nil, err
		}
		_, _, _ = h.Run(c, "docker network inspect "+o.DockerNet+" >/dev/null 2>&1 || docker network create "+o.DockerNet+" >/dev/null")
	}

	// 1. each validator, on its own host
	var nodes []*GenesisNode
	for _, p := range profiles {
		c := ctxFor(p)
		ctxs = append(ctxs, c)
		n, mnemonic, err := PrepareValidator(c, spec, o.HomeFor(p), o.Keyring)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
		o.say("✓ %s: node %s, operator %s", p.Name, n.NodeID[:12], n.Account)
		if o.Mnemonic != nil {
			if err := o.Mnemonic(p, n.Account, mnemonic); err != nil {
				return nil, err
			}
		}
		// the profile signs as this validator, with the key in the node's
		// own keyring (provision keeps these when it updates the profile)
		if p.Metadata == nil {
			p.Metadata = map[string]string{}
		}
		if v, err := keys.ValAddress(n.Account); err == nil {
			p.Metadata["valoper"] = v
		}
		if p.Signer.ContainerKey == "" {
			p.Signer.ContainerKey, p.Signer.ContainerKeyring = n.Moniker, o.Keyring.Backend
		}
	}
	// 2. base genesis, on the first validator's host
	base, err := BuildBase(nodes[0].Ctx, spec, nodes, o.Extra)
	if err != nil {
		return nil, err
	}
	o.say("✓ base genesis: %d validators funded, %d extra accounts", len(nodes), len(o.Extra))
	// 3. gentxs, each on its own host
	gentxs := map[string][]byte{}
	for _, n := range nodes {
		g, err := Gentx(n, spec, base, o.Keyring)
		if err != nil {
			return nil, err
		}
		gentxs[n.Moniker] = g
	}
	o.say("✓ %d gentxs signed on their hosts", len(gentxs))
	// 4. collect + strict validation
	final, err := Collect(nodes[0].Ctx, spec, gentxs)
	if err != nil {
		return nil, err
	}
	gp, err := GenesisPath(spec.Chain.ID)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(gp, final, 0o600); err != nil {
		return nil, err
	}
	o.say("✓ final genesis valid (%d bytes) → %s", len(final), gp)
	// 5. every node: the final genesis, its configs, start
	addrOf := func(i int) string {
		if o.DockerNet != "" {
			if u := profiles[i].Service.Unit; u != "" {
				return u
			}
			return "primium-validator"
		}
		if p := profiles[i]; p.Transport.Type == "ssh" && p.Transport.Host != "" {
			return p.Transport.Host
		}
		return "local"
	}
	portOf := func(i int) int {
		if o.DockerNet != "" {
			return 26656 // peers dial the container port
		}
		return 26656 + o.PortBase + i*o.PortStep
	}
	for i, n := range nodes {
		h, _ := n.Ctx.Host()
		if err := h.WriteFile(n.Ctx, filepath.Join(n.Home, "config", "genesis.json"), final, 0o644); err != nil {
			return nil, err
		}
		var peers []string
		for j, m := range nodes {
			if j != i {
				peers = append(peers, fmt.Sprintf("%s@%s:%d", m.NodeID, addrOf(j), portOf(j)))
			}
		}
		pargs := toolkit.Args{"role": "validator", "network": spec.Chain.ID, "home": n.Home, "reconfigure": true,
			"peers": strings.Join(peers, ","), "port_offset": float64(o.PortBase + i*o.PortStep)}
		if o.DockerNet != "" {
			pargs["docker_network"] = o.DockerNet
		}
		if _, err := (provisionTool{}).Run(n.Ctx, pargs); err != nil {
			return nil, fmt.Errorf("%s: %w", n.Moniker, err)
		}
		o.say("✓ %s started", n.Moniker)
	}
	return nodes, nil
}
