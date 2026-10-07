package common

import (
	"fmt"
	"path"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// ReadNodeFile reads <home>/<rel> of the profile's node from its host,
// falling back to the container when the node runs in docker and its
// home isn't on the host. It also says where the file was read
// ("/home/x/.evmd/config/app.toml" or "container:/data/…").
func ReadNodeFile(c *toolkit.Context, h host.Host, rel string) ([]byte, string, error) {
	p := c.Profile
	if p.Home != "" {
		if b, err := h.ReadFile(c, path.Join(p.Home, rel)); err == nil {
			return b, path.Join(p.Home, rel), nil
		}
	}
	if p.Service.Type == "docker" && p.Service.Unit != "" {
		home := p.Signer.ContainerHome
		if home == "" {
			home = p.Home
		}
		if home != "" {
			out, code, err := h.Run(c, "docker exec "+ShellQ(p.Service.Unit)+" cat "+ShellQ(path.Join(home, rel)))
			if err == nil && code == 0 {
				return []byte(out), p.Service.Unit + ":" + path.Join(home, rel), nil
			}
		}
	}
	return nil, "", fmt.Errorf("cannot read %s", rel)
}

