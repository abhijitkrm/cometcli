package netspec

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Role is what a node does in the network.
type Role string

const (
	Validator Role = "validator" // signs blocks (genesis or joined later)
	Archive   Role = "archive"   // full history, every query API, never signs
	RPC       Role = "rpc"       // public queries, pruned, never signs
)

// RoleOf maps a profile role to a node role.
func RoleOf(profileRole string) Role {
	switch strings.ToLower(profileRole) {
	case "archive":
		return Archive
	case "rpc", "sentry":
		return RPC
	}
	return Validator
}

// Expect is one setting a node of a role should have.
type Expect struct {
	File string   // config.toml | app.toml | cmd (the container command)
	Key  []string // TOML path; for cmd, the flag
	Sev  Severity // when it doesn't hold
	// Check reports whether the actual value is right; got is nil when
	// the key is missing.
	Check func(got any) bool
	Want  string // what it should be, for the report
	Why   string
}

func eq(want any) func(any) bool {
	return func(got any) bool { return got != nil && fmt.Sprint(got) == fmt.Sprint(want) }
}

func emptyList(got any) bool {
	l, ok := got.([]any)
	return ok && len(l) == 0
}

func isFalse(got any) bool { return got == false }


var durRe = regexp.MustCompile(`^(\d+)(ms|s)$`)

// durMS checks a "400ms"/"1s" duration against milliseconds.
func durMS(ms int64) func(any) bool {
	return func(got any) bool {
		m := durRe.FindStringSubmatch(fmt.Sprint(got))
		if m == nil {
			return false
		}
		n, _ := strconv.ParseInt(m[1], 10, 64)
		if m[2] == "s" {
			n *= 1000
		}
		return n == ms
	}
}

// AppMempool reports whether a cosmos/evm version runs the EVM app-side
// mempool (v0.7+, CometBFT 0.39): config.toml mempool.type must be "app"
// and app.toml mempool.max-txs >= 0. Earlier versions (CometBFT 0.38)
// don't know "app" at all.
func AppMempool(version string) bool {
	m := regexp.MustCompile(`v?(\d+)\.(\d+)`).FindStringSubmatch(version)
	if m == nil {
		return true
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > 0 || minor >= 7
}

// Expectations lists what a node of role should have under spec, for a
// node running version (the cosmos/evm tag; "" = the spec's image tag).
// prod adds the public-network hardening.
func (s *Spec) Expectations(role Role, version string, prod bool) []Expect {
	if version == "" {
		version = s.Image.Tag
	}
	var e []Expect
	add := func(x Expect) { e = append(e, x) }
	db := s.DBBackend
	if db == "" {
		db = "goleveldb"
	}
	add(Expect{File: "config.toml", Key: []string{"db_backend"}, Sev: Fail, Check: eq(db), Want: db,
		Why: "must match app.toml app-db-backend and the data on disk, or the node fails to open its database (EOF)"})
	add(Expect{File: "app.toml", Key: []string{"app-db-backend"}, Sev: Fail,
		// empty means "use config.toml's db_backend"
		Check: func(g any) bool { return g == nil || g == "" || fmt.Sprint(g) == db }, Want: db + " (or empty)",
		Why: "must match config.toml db_backend"})
	if AppMempool(version) {
		add(Expect{File: "config.toml", Key: []string{"mempool", "type"}, Sev: Fail, Check: eq("app"), Want: `"app"`,
			Why: version + " runs the EVM app-side mempool; evmd's cross-config check panics otherwise"})
		// max-txs >= 0 turns the app-side mempool on, which needs "app"
		// above; -1 (off) is valid with "app" too, so it isn't checked
	} else {
		add(Expect{File: "config.toml", Key: []string{"mempool", "type"}, Sev: Fail,
			Check: func(g any) bool { return fmt.Sprint(g) != "app" }, Want: "flood (not app)",
			Why: version + " pins CometBFT 0.38, which doesn't support mempool type \"app\""})
	}
	if s.MinGasPrice != "" && s.GasDenom() != "" {
		want := s.MinGasPrice + s.GasDenom()
		add(Expect{File: "app.toml", Key: []string{"minimum-gas-prices"}, Sev: Warn, Check: eq(want), Want: want,
			Why: "the fee the mempool accepts; nodes that disagree reject each other's txs"})
	}
	if s.Chain.EVMChainID != 0 {
		add(Expect{File: "app.toml", Key: []string{"evm", "evm-chain-id"}, Sev: Fail,
			Check: func(g any) bool { return fmt.Sprint(g) == strconv.FormatUint(s.Chain.EVMChainID, 10) },
			Want:  strconv.FormatUint(s.Chain.EVMChainID, 10), Why: "EIP-155 chain id: wallets sign for this id"})
	}
	add(Expect{File: "config.toml", Key: []string{"instrumentation", "prometheus"}, Sev: Info, Check: eq(s.Services.Prometheus),
		Want: fmt.Sprint(s.Services.Prometheus), Why: "metrics for monitoring"})
	svc := []struct {
		key []string
		on  bool
	}{{[]string{"api", "enable"}, s.Services.API}, {[]string{"grpc", "enable"}, s.Services.GRPC}, {[]string{"json-rpc", "enable"}, s.Services.JSONRPC}}
	for _, x := range svc {
		add(Expect{File: "app.toml", Key: x.key, Sev: Warn, Check: eq(x.on), Want: fmt.Sprint(x.on), Why: "as the network spec says"})
	}
	if s.Services.WS && s.Services.JSONRPC {
		add(Expect{File: "cmd", Key: []string{"--json-rpc.ws-origins"}, Sev: Warn,
			Check: func(g any) bool { return g != nil }, Want: "passed as a CLI flag",
			Why: "cosmos-evm mangles ws-origins read from app.toml into \"[*]\" (viper/pflag bug); only the CLI flag works"})
	}

	switch role {
	case Validator:
		c := s.Consensus
		for _, t := range []struct {
			key string
			ms  int64
		}{{"timeout_propose", c.TimeoutProposeMS}, {"timeout_prevote", c.TimeoutPrevoteMS}, {"timeout_precommit", c.TimeoutPrecommitMS}, {"timeout_commit", c.TimeoutCommitMS}} {
			if t.ms > 0 {
				add(Expect{File: "config.toml", Key: []string{"consensus", t.key}, Sev: Warn, Check: durMS(t.ms),
					Want: fmt.Sprintf("%dms", t.ms), Why: "validators should share the spec's consensus timing"})
			}
		}
		add(Expect{File: "config.toml", Key: []string{"p2p", "external_address"}, Sev: Warn,
			Check: func(g any) bool { return g != nil && fmt.Sprint(g) != "" }, Want: "<public ip>:26656",
			Why: "peers can't dial back a validator that doesn't advertise its address"})
		if prod {
			hardenRPC(add, "validator")
			add(Expect{File: "app.toml", Key: []string{"api", "swagger"}, Sev: Warn, Check: isFalse, Want: "false",
				Why: "smaller attack surface on a validator"})
			add(Expect{File: "app.toml", Key: []string{"json-rpc", "api"}, Sev: Warn,
				Check: func(g any) bool {
					v := fmt.Sprint(g)
					return !strings.Contains(v, "debug") && !strings.Contains(v, "personal")
				},
				Want: "without debug, personal", Why: "debug and personal namespaces don't belong on a validator"})
		}
	case Archive:
		add(Expect{File: "app.toml", Key: []string{"pruning"}, Sev: Fail,
			Check: eq("nothing"), Want: `"nothing"`, Why: "an archive keeps all historical state"})
		add(Expect{File: "config.toml", Key: []string{"tx_index", "indexer"}, Sev: Fail, Check: eq("kv"), Want: `"kv"`,
			Why: "an archive indexes every transaction"})
		if prod {
			hardenRPC(add, "archive")
		}
	case RPC:
		add(Expect{File: "config.toml", Key: []string{"tx_index", "indexer"}, Sev: Warn, Check: eq("kv"), Want: `"kv"`,
			Why: "tx queries need the indexer"})
		if prod {
			hardenRPC(add, "RPC node")
		}
	}
	return e
}

// hardenRPC is the public-network CORS/unlock hardening every role needs.
func hardenRPC(add func(Expect), who string) {
	add(Expect{File: "config.toml", Key: []string{"rpc", "cors_allowed_origins"}, Sev: Warn, Check: emptyList, Want: "[]",
		Why: "no cross-origin RPC on a " + who + " — put a reverse proxy in front if a browser needs it"})
	add(Expect{File: "app.toml", Key: []string{"api", "enabled-unsafe-cors"}, Sev: Warn, Check: isFalse, Want: "false",
		Why: "REST CORS open to any origin"})
	add(Expect{File: "app.toml", Key: []string{"json-rpc", "allow-insecure-unlock"}, Sev: Warn, Check: isFalse, Want: "false",
		Why: "account unlock over JSON-RPC"})
}

// Lookup reads a TOML path from a decoded document.
func Lookup(doc map[string]any, path []string) any {
	var cur any = doc
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// CmdFlag finds a flag in a container command ("--x=v" or "--x v");
// nil when absent.
func CmdFlag(cmd []string, flag string) any {
	for i, a := range cmd {
		if a == flag {
			if i+1 < len(cmd) {
				return cmd[i+1]
			}
			return ""
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"=")
		}
	}
	return nil
}

// Evaluate runs expectations against a node's decoded config files and
// container command, returning what doesn't hold.
func Evaluate(node string, exp []Expect, config, app map[string]any, cmd []string) []Finding {
	var out []Finding
	for _, x := range exp {
		var got any
		switch x.File {
		case "config.toml":
			if config == nil {
				continue
			}
			got = Lookup(config, x.Key)
		case "app.toml":
			if app == nil {
				continue
			}
			got = Lookup(app, x.Key)
		case "cmd":
			if cmd == nil {
				continue
			}
			got = CmdFlag(cmd, x.Key[0])
		}
		// archive pruning may come from the command instead of app.toml
		if x.File == "app.toml" && len(x.Key) == 1 && x.Key[0] == "pruning" && cmd != nil {
			if v := CmdFlag(cmd, "--pruning"); v != nil {
				got = v
			}
		}
		if x.Check(got) {
			continue
		}
		have := "missing"
		if got != nil {
			have = fmt.Sprint(got)
		}
		out = append(out, Finding{Sev: x.Sev, Where: node + ":" + x.File,
			What: fmt.Sprintf("%s = %s, want %s — %s", strings.Join(x.Key, "."), have, x.Want, x.Why)})
	}
	return out
}
