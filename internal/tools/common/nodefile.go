package common

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// containerHomeOf is where the node home sits inside its container.
func containerHomeOf(c *toolkit.Context) string {
	if h := c.Profile.Signer.ContainerHome; h != "" {
		return h
	}
	return c.Profile.Home
}

// HostHome is the node home on its host: the profile's, else — for a
// docker node — the bind mount behind its container home, read from the
// container even when it's stopped (a crashed node still needs its
// config fixed).
func HostHome(c *toolkit.Context, h host.Host) string {
	p := c.Profile
	if p.Home != "" {
		if _, err := h.Stat(c, path.Join(p.Home, "config")); err == nil {
			return p.Home
		}
	}
	if p.Service.Type != "docker" || p.Service.Unit == "" {
		return p.Home
	}
	home := containerHomeOf(c)
	out, code, err := h.Run(c, "docker inspect -f '{{range .Mounts}}{{.Destination}}|{{.Source}}{{\"\\n\"}}{{end}}' "+ShellQ(p.Service.Unit))
	if err != nil || code != 0 {
		return ""
	}
	best, bestDst := "", ""
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		dst, src, ok := strings.Cut(l, "|")
		if !ok || src == "" {
			continue
		}
		// the mount the home is in (the longest matching destination)
		if home != "" && (home == dst || strings.HasPrefix(home, dst+"/")) && len(dst) > len(bestDst) {
			best, bestDst = path.Join(src, strings.TrimPrefix(home, dst)), dst
		}
	}
	if best == "" {
		return ""
	}
	if _, err := h.Stat(c, path.Join(best, "config")); err != nil {
		return "" // not readable from the host (owned by another user)
	}
	return best
}

// ReadNodeFile reads <home>/<rel> of the profile's node from its host
// (the profile's home, or the container's bind mount), falling back to
// the running container. It also says where it read the file.
func ReadNodeFile(c *toolkit.Context, h host.Host, rel string) ([]byte, string, error) {
	if home := HostHome(c, h); home != "" {
		if b, err := h.ReadFile(c, path.Join(home, rel)); err == nil {
			return b, path.Join(home, rel), nil
		}
	}
	p := c.Profile
	if p.Service.Type == "docker" && p.Service.Unit != "" {
		if home := containerHomeOf(c); home != "" {
			out, code, err := h.Run(c, "docker exec "+ShellQ(p.Service.Unit)+" cat "+ShellQ(path.Join(home, rel)))
			if err == nil && code == 0 {
				return []byte(out), p.Service.Unit + ":" + path.Join(home, rel), nil
			}
		}
	}
	return nil, "", fmt.Errorf("cannot read %s", rel)
}

// WriteNodeFile writes <home>/<rel> the same way ReadNodeFile reads it,
// keeping the previous content as <rel>.cometcli-bak.
func WriteNodeFile(c *toolkit.Context, h host.Host, rel string, data []byte, perm os.FileMode) (string, error) {
	if home := HostHome(c, h); home != "" {
		fp := path.Join(home, rel)
		if fi, err := h.Stat(c, fp); err == nil {
			perm = fi.Mode().Perm() // keep the file's mode
		}
		if old, err := h.ReadFile(c, fp); err == nil {
			_ = h.WriteFile(c, fp+".cometcli-bak", old, perm)
		}
		return fp, h.WriteFile(c, fp, data, perm)
	}
	p := c.Profile
	if p.Service.Type == "docker" && p.Service.Unit != "" {
		if home := containerHomeOf(c); home != "" {
			fp := path.Join(home, rel)
			script := fmt.Sprintf("docker exec -i %s sh -c %s", ShellQ(p.Service.Unit),
				ShellQ(fmt.Sprintf("cp %s %s.cometcli-bak 2>/dev/null; cat > %s.cometcli-tmp && mv %s.cometcli-tmp %s", fp, fp, fp, fp, fp)))
			res, err := host.ExecIn(c, h, script, data, 64<<10)
			if err == nil && res.Code == 0 {
				return p.Service.Unit + ":" + fp, nil
			}
			return "", fmt.Errorf("write %s in %s: %v %s", fp, p.Service.Unit, err, strings.TrimSpace(res.Output))
		}
	}
	return "", fmt.Errorf("cannot write %s: no node home on the host and no running container", rel)
}

// ListNodeDir lists the entries of <home>/<rel>.
func ListNodeDir(c *toolkit.Context, h host.Host, rel string) ([]string, error) {
	var out string
	var code int
	var err error
	if home := HostHome(c, h); home != "" {
		out, code, err = h.Run(c, "ls -1 "+ShellQ(path.Join(home, rel))+" 2>/dev/null | head -200")
	} else if c.Profile.Service.Type == "docker" && containerHomeOf(c) != "" {
		out, code, err = h.Run(c, "docker exec "+ShellQ(c.Profile.Service.Unit)+" ls -1 "+ShellQ(path.Join(containerHomeOf(c), rel))+" 2>/dev/null | head -200")
	} else {
		return nil, fmt.Errorf("no node home")
	}
	if err != nil || code != 0 {
		return nil, fmt.Errorf("cannot list %s", rel)
	}
	var names []string
	for _, l := range bytes.Split([]byte(strings.TrimSpace(out)), []byte("\n")) {
		if s := strings.TrimSpace(string(l)); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// DataBackend tells from the files in data/application.db which database
// backend wrote it: goleveldb leaves *.ldb, rocksdb OPTIONS-* files.
func DataBackend(names []string) string {
	ldb, rocks := false, false
	for _, n := range names {
		switch {
		case strings.HasSuffix(n, ".ldb"):
			ldb = true
		case strings.HasPrefix(n, "OPTIONS-"):
			rocks = true
		}
	}
	switch {
	case rocks && !ldb:
		return "rocksdb"
	case ldb && !rocks:
		return "goleveldb"
	}
	return ""
}
