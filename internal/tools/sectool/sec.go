// Package sectool implements sec.* tools: exposure audit, file permission
// checks, and double-sign protection.
package sectool

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Register adds all sec.* tools.
func Register(r *toolkit.Registry) {
	r.Register(exposureTool{})
	r.Register(permsTool{})
	r.Register(doubleSignTool{})
}

// sensitivePorts maps ports that must not be public on a validator.
var sensitivePorts = map[string]string{
	"8545":  "EVM JSON-RPC — NEVER public on validators",
	"8546":  "EVM JSON-RPC websocket",
	"9090":  "Cosmos gRPC",
	"1317":  "LCD/REST API",
	"26657": "CometBFT RPC (validators should bind localhost or sentries)",
	"6060":  "pprof — exposes process internals",
	"26660": "prometheus metrics",
	"26656": "p2p (must be reachable — info only)",
}

type exposureTool struct{}

func (exposureTool) Name() string { return "sec.exposure" }
func (exposureTool) Desc() string {
	return "Audit listening sockets vs validator exposure policy"
}
func (exposureTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (exposureTool) Tier() toolkit.Tier     { return toolkit.TierDiagnose }

func (exposureTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	var ls []listener
	source := ""
	// Containers: the published port map is authoritative — host sockets
	// show only the docker proxy, and on macOS live in a VM.
	if c.Profile.Service.Type == "docker" && c.Profile.Service.Unit != "" {
		cmd := "docker port " + c.Profile.Service.Unit
		out, code, err := h.Run(c, cmd)
		c.LogShell(cmd, code)
		if err == nil {
			ls, source = parseDockerPorts(out), "docker port "+c.Profile.Service.Unit
		}
	}
	if source == "" {
		cmd := "ss -tlnH 2>/dev/null || netstat -anp tcp 2>/dev/null | grep LISTEN || netstat -tln 2>/dev/null"
		out, code, err := h.Run(c, cmd)
		c.LogShell("ss -tln | netstat", code)
		if err != nil {
			return nil, fmt.Errorf("list sockets: %w", err)
		}
		ls, source = parseSockets(out), "host sockets"
	}
	if len(ls) == 0 {
		// Never report "clean" when nothing could be read.
		return nil, fmt.Errorf("could not enumerate listening sockets via %s — exposure unknown, not clean", source)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "source: %s\n", source)
	var findings []map[string]string
	crits := 0
	for _, l := range ls {
		note, sensitive := sensitivePorts[l.port]
		if !sensitive {
			continue
		}
		if l.port == "26656" {
			fmt.Fprintf(&b, "i  :%s  %-22s p2p reachable (%s)\n", l.port, l.bind, note)
			continue
		}
		if l.public {
			level := "warn"
			if c.Profile.IsValidator() {
				level = "critical"
				crits++
			}
			fmt.Fprintf(&b, "✗  :%s  %-22s PUBLIC — %s [%s]\n", l.port, l.bind, note, level)
			findings = append(findings, map[string]string{"port": l.port, "bind": l.bind, "level": level})
		} else {
			fmt.Fprintf(&b, "✓  :%s  %-22s localhost\n", l.port, l.bind)
		}
	}
	fmt.Fprintf(&b, "\n%d critical exposure(s)", crits)
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"findings": findings, "critical": crits, "source": source}}, nil
}

// listener is one listening socket: port classifies it (the service port —
// the container port for docker), bind is what's shown to the operator.
type listener struct {
	port, bind string
	public     bool
}

func isLoopbackHost(h string) bool {
	h = strings.Trim(h, "[]")
	return strings.HasPrefix(h, "127.") || h == "localhost" || h == "::1"
}

// parseDockerPorts reads `docker port <c>` lines: "26657/tcp -> 0.0.0.0:26677".
// IPv4/IPv6 duplicates of the same mapping collapse into one entry.
func parseDockerPorts(out string) []listener {
	var ls []listener
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		cport, hostAddr, ok := strings.Cut(strings.TrimSpace(line), " -> ")
		if !ok {
			continue
		}
		cport, _, _ = strings.Cut(cport, "/")
		i := strings.LastIndex(hostAddr, ":")
		if i < 0 {
			continue
		}
		host, hport := hostAddr[:i], hostAddr[i+1:]
		public := !isLoopbackHost(host)
		key := fmt.Sprintf("%s|%s|%v", cport, hport, public)
		if seen[key] {
			continue
		}
		seen[key] = true
		bind := hostAddr
		if hport != cport {
			bind += " (container :" + cport + ")"
		}
		ls = append(ls, listener{port: cport, bind: bind, public: public})
	}
	return ls
}

// parseSockets reads ss / Linux netstat ("0.0.0.0:26657") and BSD/macOS
// netstat ("*.26657", "127.0.0.1.6060") listing lines.
func parseSockets(out string) []listener {
	var ls []listener
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for _, col := range f[min(1, len(f)):] {
			host, port, ok := splitListenAddr(col)
			if !ok {
				continue
			}
			public := host == "*" || !isLoopbackHost(host)
			if key := port + "|" + fmt.Sprint(public); !seen[key] {
				seen[key] = true
				ls = append(ls, listener{port: port, bind: col, public: public})
			}
			break // first address column is the local one
		}
	}
	return ls
}

func splitListenAddr(col string) (host, port string, ok bool) {
	sep := strings.LastIndex(col, ":")
	if sep < 0 {
		sep = strings.LastIndex(col, ".") // BSD netstat: host.port
	}
	if sep <= 0 || sep == len(col)-1 {
		return "", "", false
	}
	port = col[sep+1:]
	for _, r := range port {
		if r < '0' || r > '9' {
			return "", "", false
		}
	}
	return col[:sep], port, true
}

type permsTool struct{}

func (permsTool) Name() string { return "sec.perms" }
func (permsTool) Desc() string {
	return "Audit permissions on key material and config files"
}
func (permsTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (permsTool) Tier() toolkit.Tier     { return toolkit.TierDiagnose }

func (permsTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	home := c.Profile.Home
	targets := []struct {
		path string
		want uint32
		desc string
	}{
		{home + "/config/priv_validator_key.json", 0o600, "consensus key"},
		{home + "/data/priv_validator_state.json", 0o600, "signing state"},
		{home + "/config/node_key.json", 0o600, "p2p node key"},
		{home + "/config", 0o700, "config dir"},
		{home + "/data", 0o700, "data dir"},
	}
	var b strings.Builder
	var findings []map[string]any
	bad := 0
	for _, t := range targets {
		fi, err := h.Stat(c, t.path)
		if err != nil {
			fmt.Fprintf(&b, "?  %-50s %s (missing?)\n", t.path, t.desc)
			continue
		}
		perm := uint32(fi.Mode().Perm())
		ok := perm&0o077 == 0 // group/other must be zero
		icon := "✓"
		if !ok {
			icon = "✗"
			bad++
		}
		fmt.Fprintf(&b, "%s  %-50s %04o  %s\n", icon, t.path, perm, t.desc)
		findings = append(findings, map[string]any{"path": t.path, "perm": perm, "ok": ok})
	}
	fmt.Fprintf(&b, "\n%d permission problem(s)", bad)
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"findings": findings, "bad": bad}}, nil
}

type doubleSignTool struct{}

func (doubleSignTool) Name() string { return "sec.doublesign" }
func (doubleSignTool) Desc() string {
	return "Inspect priv_validator_state HRS + detect risky signing configuration"
}
func (doubleSignTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (doubleSignTool) Tier() toolkit.Tier     { return toolkit.TierDiagnose }

func (doubleSignTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	raw, err := h.ReadFile(c, c.Profile.Home+"/data/priv_validator_state.json")
	if err != nil {
		return nil, fmt.Errorf("read priv_validator_state: %w", err)
	}
	var st struct {
		Height    string `json:"height"`
		Round     int    `json:"round"`
		Step      int    `json:"step"`
		Signature string `json:"signature"`
		SignBytes string `json:"signbytes"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse priv_validator_state: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "last signed: height=%s round=%d step=%d\n", st.Height, st.Round, st.Step)
	fmt.Fprintf(&b, "signature present: %v\n", st.Signature != "")
	// Check for a second signer risk: is the service running while we might migrate?
	if c.Profile.Service.Unit != "" {
		out, code, _ := h.Run(c, fmt.Sprintf("systemctl is-active %s 2>/dev/null || echo unknown", c.Profile.Service.Unit))
		c.LogShell("is-active "+c.Profile.Service.Unit, code)
		fmt.Fprintf(&b, "service %s: %s\n", c.Profile.Service.Unit, strings.TrimSpace(out))
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{
		"height": st.Height, "round": st.Round, "step": st.Step,
	}}, nil
}
