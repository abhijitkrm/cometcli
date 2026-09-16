package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Finding is one lint result.
type Finding struct {
	Level   string `json:"level"` // ok | warn | critical
	Check   string `json:"check"`
	Message string `json:"message"`
}

// lintConfig audits config.toml / app.toml against hardened validator
// baselines from the cosmos-evm docs.
func lintConfig(c *toolkit.Context, file, raw string) *toolkit.Result {
	var m map[string]any
	if err := toml.Unmarshal([]byte(raw), &m); err != nil {
		return &toolkit.Result{Text: "unparseable: " + err.Error()}
	}
	var f []Finding
	isVal := c.Profile.IsValidator()

	get := func(path ...string) any {
		var cur any = m
		for _, p := range path {
			mm, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = mm[p]
		}
		return cur
	}
	bind := func(v any) string { // extract host part of host:port laddr
		s, _ := v.(string)
		s = strings.TrimPrefix(s, "tcp://")
		s = strings.TrimPrefix(s, "http://")
		h, _, err := net.SplitHostPort(s)
		if err != nil {
			return s
		}
		return h
	}
	public := func(v any) bool {
		h := bind(v)
		return h != "127.0.0.1" && h != "localhost" && h != "::1" && h != ""
	}
	boolOf := func(v any) bool { b, _ := v.(bool); return b }
	check := func(level, name, msg string, bad bool) {
		if bad {
			f = append(f, Finding{level, name, msg})
		} else {
			f = append(f, Finding{"ok", name, msg})
		}
	}

	switch file {
	case "app.toml":
		api := get("api", "enable")
		check("critical", "api.enable",
			fmt.Sprintf("REST API enabled=%v — must be false on validators", api),
			isVal && boolOf(api))
		grpcEn := get("grpc", "enable")
		grpcAddr := get("grpc", "address")
		check("critical", "grpc.public",
			fmt.Sprintf("gRPC enabled=%v at %v — bind localhost only on validators", grpcEn, grpcAddr),
			isVal && boolOf(grpcEn) && public(grpcAddr))
		jsonrpc := get("json-rpc", "enable")
		jsonrpcAddr := get("json-rpc", "address")
		check("critical", "json-rpc.enable",
			fmt.Sprintf("EVM JSON-RPC enabled=%v at %v — NEVER enable on validators", jsonrpc, jsonrpcAddr),
			isVal && boolOf(jsonrpc))
		if v := get("minimum-gas-prices"); v != nil {
			check("warn", "minimum-gas-prices", fmt.Sprintf("minimum-gas-prices=%v", v), fmt.Sprint(v) == "0" || v == "")
		}
		prune := get("pruning", "strategy")
		check("warn", "pruning", fmt.Sprintf("pruning strategy=%v", prune), prune == "nothing")

	case "config.toml":
		laddr := get("laddr")
		check("warn", "rpc.laddr",
			fmt.Sprintf("comet RPC laddr=%v — keep private; sentries serve public traffic", laddr),
			isVal && public(laddr))
		pprof := get("pprof_laddr")
		check("critical", "pprof_laddr",
			fmt.Sprintf("pprof laddr=%v — must be localhost (exposes internals)", pprof),
			public(pprof))
		dsc := get("double_sign_check_height")
		check("warn", "double_sign_check_height",
			fmt.Sprintf("double_sign_check_height=%v", dsc),
			isVal && fmt.Sprint(dsc) == "0")
		pex := get("pex")
		check("warn", "pex",
			fmt.Sprintf("pex=%v — validators often disable and use persistent_peers", pex), false)
		prom := get("instrumentation", "prometheus")
		check("ok", "prometheus", fmt.Sprintf("prometheus metrics=%v", prom), false)
	}

	var b strings.Builder
	crits := 0
	for _, x := range f {
		if x.Level == "critical" {
			crits++
		}
		icon := map[string]string{"ok": "✓", "warn": "!", "critical": "✗"}[x.Level]
		fmt.Fprintf(&b, "%s  %-28s %s\n", icon, x.Check, x.Message)
	}
	fmt.Fprintf(&b, "\n%d critical finding(s)", crits)
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"findings": f, "critical": crits}}
}
