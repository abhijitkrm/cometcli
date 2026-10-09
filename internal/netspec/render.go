package netspec

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Patch sets one key in a node config file.
type Patch struct {
	File    string // config.toml | app.toml | client.toml
	Section string // "" = top level
	Key     string
	Value   string // a TOML literal: "\"x\"", "true", "[]", "0"
}

// NodeParams are the per-node values a render needs.
type NodeParams struct {
	Moniker         string
	ExternalAddress string // ip:port advertised to peers ("" = leave unset)
	PersistentPeers []string
	Prod            bool // public-network hardening
}

func q(s string) string { return strconv.Quote(s) }

func boolLit(b bool) string { return strconv.FormatBool(b) }

func ms(n int64) string { return q(fmt.Sprintf("%dms", n)) }

// Patches are the config edits that make a node of role match the spec —
// the same settings network check expects, so a rendered node passes it.
func (s *Spec) Patches(role Role, version string, n NodeParams) []Patch {
	if version == "" {
		version = s.Image.Tag
	}
	var p []Patch
	set := func(file, section, key, value string) {
		p = append(p, Patch{File: file, Section: section, Key: key, Value: value})
	}
	db := s.DBBackend
	if db == "" {
		db = "goleveldb"
	}
	sv := s.Services

	// config.toml
	if n.Moniker != "" {
		set("config.toml", "", "moniker", q(n.Moniker))
	}
	set("config.toml", "", "db_backend", q(db))
	set("config.toml", "rpc", "laddr", q("tcp://0.0.0.0:26657"))
	set("config.toml", "p2p", "laddr", q("tcp://0.0.0.0:26656"))
	if n.ExternalAddress != "" {
		set("config.toml", "p2p", "external_address", q(n.ExternalAddress))
	}
	if len(n.PersistentPeers) > 0 {
		set("config.toml", "p2p", "persistent_peers", q(strings.Join(n.PersistentPeers, ",")))
	}
	if AppMempool(version) {
		set("config.toml", "mempool", "type", q("app"))
	}
	set("config.toml", "instrumentation", "prometheus", boolLit(sv.Prometheus))
	set("config.toml", "tx_index", "indexer", q("kv"))
	if c := s.Consensus; role == Validator {
		for _, t := range []struct {
			key string
			v   int64
		}{{"timeout_propose", c.TimeoutProposeMS}, {"timeout_propose_delta", c.TimeoutProposeDeltaMS},
			{"timeout_prevote", c.TimeoutPrevoteMS}, {"timeout_prevote_delta", c.TimeoutPrevoteDeltaMS},
			{"timeout_precommit", c.TimeoutPrecommitMS}, {"timeout_precommit_delta", c.TimeoutPrecommitDeltaMS},
			{"timeout_commit", c.TimeoutCommitMS}, {"peer_gossip_sleep_duration", c.PeerGossipSleepMS}} {
			if t.v > 0 {
				set("config.toml", "consensus", t.key, ms(t.v))
			}
		}
	}
	open := !n.Prod && role != Validator // test networks keep archive/rpc open like node-setup does
	if n.Prod || role == Validator {
		set("config.toml", "rpc", "cors_allowed_origins", "[]")
	} else if open {
		set("config.toml", "rpc", "cors_allowed_origins", `["*"]`)
	}

	// app.toml
	if s.MinGasPrice != "" {
		set("app.toml", "", "minimum-gas-prices", q(s.MinGasPrice+s.GasDenom()))
	}
	set("app.toml", "", "app-db-backend", q(db))
	if role == Archive {
		set("app.toml", "", "pruning", q("nothing"))
	}
	set("app.toml", "api", "enable", boolLit(sv.API))
	set("app.toml", "api", "address", q(fmt.Sprintf("tcp://0.0.0.0:%d", def(sv.APIPort, 1317))))
	set("app.toml", "api", "swagger", boolLit(role != Validator))
	set("app.toml", "api", "enabled-unsafe-cors", boolLit(open))
	set("app.toml", "grpc", "enable", boolLit(sv.GRPC))
	set("app.toml", "grpc", "address", q(fmt.Sprintf("0.0.0.0:%d", def(sv.GRPCPort, 9090))))
	set("app.toml", "json-rpc", "enable", boolLit(sv.JSONRPC))
	set("app.toml", "json-rpc", "address", q(fmt.Sprintf("0.0.0.0:%d", def(sv.RPCPort, 8545))))
	set("app.toml", "json-rpc", "ws-address", q(fmt.Sprintf("0.0.0.0:%d", def(sv.WSPort, 8546))))
	api := "eth,net,web3,txpool"
	if role == Archive {
		api += ",debug,personal"
	}
	set("app.toml", "json-rpc", "api", q(api))
	set("app.toml", "json-rpc", "allow-insecure-unlock", boolLit(open))
	if AppMempool(version) {
		set("app.toml", "mempool", "max-txs", "0")
	}
	if s.Chain.EVMChainID != 0 {
		set("app.toml", "evm", "evm-chain-id", strconv.FormatUint(s.Chain.EVMChainID, 10))
	}

	// client.toml
	set("client.toml", "", "chain-id", q(s.Chain.ID))
	set("client.toml", "", "node", q("tcp://localhost:26657"))
	return p
}

func def(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

// Command is the container command for a node of role.
func (s *Spec) Command(role Role) []string {
	cmd := []string{"start"}
	if role == Archive {
		cmd = append(cmd, "--pruning", "nothing")
	}
	if s.Services.WS && s.Services.JSONRPC {
		o := s.Services.WSOrigins
		if o == "" {
			o = "*"
		}
		cmd = append(cmd, "--json-rpc.ws-origins="+o)
	}
	return cmd
}

// --- TOML editing ----------------------------------------------------------------

var sectionRe = regexp.MustCompile(`^\s*\[\s*([A-Za-z0-9_.\-]+)\s*\]\s*(#.*)?$`)

// SetTOML sets key = value in section of a TOML document, in place: the
// rest of the file — comments, order, other keys — is left as it was. A
// missing key is added at the end of its section; a missing section at
// the end of the file.
func SetTOML(doc, section, key, value string) string {
	lines := strings.Split(doc, "\n")
	keyRe := regexp.MustCompile(`^(\s*)` + regexp.QuoteMeta(key) + `\s*=`)
	cur := ""
	sectionSeen := section == ""
	insertAt := -1 // end of the target section
	if section == "" {
		insertAt = 0
		for i, l := range lines {
			if sectionRe.MatchString(l) {
				insertAt = i
				break
			}
			insertAt = i + 1
		}
	}
	for i, l := range lines {
		if m := sectionRe.FindStringSubmatch(l); m != nil {
			if cur == section && section != "" {
				insertAt = i
			}
			cur = m[1]
			if cur == section {
				sectionSeen = true
				insertAt = len(lines)
			}
			continue
		}
		if cur == section {
			if m := keyRe.FindStringSubmatch(l); m != nil {
				lines[i] = m[1] + key + " = " + value
				return strings.Join(lines, "\n")
			}
		}
	}
	line := key + " = " + value
	if !sectionSeen {
		return strings.TrimRight(doc, "\n") + "\n\n[" + section + "]\n" + line + "\n"
	}
	// back up over trailing blank lines so the key joins its section
	for insertAt > 0 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}
	out := append([]string{}, lines[:insertAt]...)
	out = append(out, line)
	out = append(out, lines[insertAt:]...)
	return strings.Join(out, "\n")
}

// Apply applies the patches for one file.
func Apply(doc, file string, patches []Patch) string {
	for _, p := range patches {
		if p.File == file {
			doc = SetTOML(doc, p.Section, p.Key, p.Value)
		}
	}
	return doc
}

// Compose is a docker-compose file for one node.
type Compose struct {
	Image, Container, HostHome string
	Command                    []string
	User                       string // uid:gid owning HostHome
	// PublicRPC publishes RPC/REST/gRPC/EVM on all interfaces; otherwise
	// only on 127.0.0.1 (cometcli reaches them through SSH). P2P is
	// always public.
	PublicRPC bool
	Network   string // an existing docker network to join ("" = compose default)
	Services  Services
	// PortOffset shifts every host port (several nodes on one machine).
	PortOffset int
}

// YAML renders the compose file.
func (c Compose) YAML() string {
	bind := "127.0.0.1:"
	if c.PublicRPC {
		bind = ""
	}
	pub := func(b string, port int) string { return fmt.Sprintf("%s%d:%d", b, port+c.PortOffset, port) }
	ports := []string{pub("", 26656), pub(bind, 26657), pub(bind, 26660)}
	add := func(on bool, port int) {
		if on {
			ports = append(ports, pub(bind, port))
		}
	}
	sv := c.Services
	add(sv.API, def(sv.APIPort, 1317))
	add(sv.GRPC, def(sv.GRPCPort, 9090))
	add(sv.JSONRPC, def(sv.RPCPort, 8545))
	add(sv.JSONRPC && sv.WS, def(sv.WSPort, 8546))
	var cmd []string
	for _, a := range c.Command {
		cmd = append(cmd, q(a))
	}
	var b strings.Builder
	b.WriteString("# written by cometcli node provision\nservices:\n  node:\n")
	fmt.Fprintf(&b, "    image: %s\n    container_name: %s\n    restart: unless-stopped\n", c.Image, c.Container)
	if c.User != "" {
		fmt.Fprintf(&b, "    user: %q\n", c.User)
	}
	b.WriteString("    stop_grace_period: 60s\n    working_dir: /data/node0/evmd\n    environment:\n      - ID=0\n      - HOME=/data/node0/evmd\n")
	fmt.Fprintf(&b, "    command: [%s]\n", strings.Join(cmd, ", "))
	b.WriteString("    volumes:\n")
	fmt.Fprintf(&b, "      - %s:/data/node0/evmd\n", c.HostHome)
	b.WriteString("    ports:\n")
	sort.Strings(ports[1:])
	for _, p := range ports {
		fmt.Fprintf(&b, "      - %q\n", p)
	}
	if c.Network != "" {
		fmt.Fprintf(&b, "    networks: [%s]\nnetworks:\n  %s:\n    external: true\n", c.Network, c.Network)
	}
	return b.String()
}
