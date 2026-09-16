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
	out, code, err := h.Run(c, "ss -tlnH 2>/dev/null || netstat -tln 2>/dev/null | tail -n +3")
	c.LogShell("ss -tln", code)
	if err != nil {
		return nil, fmt.Errorf("list sockets: %w", err)
	}
	var b strings.Builder
	var findings []map[string]string
	crits := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		// local address column position differs between ss/netstat; find host:port
		var laddr string
		for _, col := range f {
			if strings.Count(col, ":") >= 1 {
				laddr = col
				break
			}
		}
		if laddr == "" {
			continue
		}
		port := laddr[strings.LastIndex(laddr, ":")+1:]
		note, sensitive := sensitivePorts[port]
		if !sensitive {
			continue
		}
		hostPart := laddr[:strings.LastIndex(laddr, ":")]
		public := hostPart == "*" || hostPart == "0.0.0.0" || hostPart == "::" ||
			(!strings.HasPrefix(hostPart, "127.") && hostPart != "localhost" && hostPart != "[::1]" && hostPart != "::1")
		if port == "26656" {
			fmt.Fprintf(&b, "i  :%s  %-14s p2p reachable (%s)\n", port, laddr, note)
			continue
		}
		if public {
			level := "warn"
			if c.Profile.IsValidator() {
				level = "critical"
				crits++
			}
			fmt.Fprintf(&b, "✗  :%s  %-14s PUBLIC — %s [%s]\n", port, laddr, note, level)
			findings = append(findings, map[string]string{"port": port, "bind": laddr, "level": level})
		} else {
			fmt.Fprintf(&b, "✓  :%s  %-14s localhost\n", port, laddr)
		}
	}
	fmt.Fprintf(&b, "\n%d critical exposure(s)", crits)
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"findings": findings, "critical": crits}}, nil
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
