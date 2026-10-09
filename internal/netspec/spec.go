// Package netspec is the declarative description of a network: chain
// identity, genesis parameters, consensus timing, node services and how
// images are built and run. One file per network
// (~/.cometcli/networks/<chain-id>.yaml) is the source every node's
// configuration is checked against — and, later, rendered from.
//
// It imports the node-setup scripts' network-config.env, so an existing
// network can move to cometcli without retyping anything.
package netspec

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// Spec describes one network.
type Spec struct {
	Chain     Chain     `yaml:"chain"`
	Image     Image     `yaml:"image"`
	Genesis   Genesis   `yaml:"genesis"`
	Consensus Consensus `yaml:"consensus"`
	Services  Services  `yaml:"services"`
	// MinGasPrice is the node's minimum-gas-prices amount, in FeeDenom
	// (the bond denom when that's empty — EVM chains often differ).
	MinGasPrice string `yaml:"min_gas_price"`
	FeeDenom    string `yaml:"fee_denom,omitempty"`
	DBBackend   string `yaml:"db_backend"` // goleveldb | rocksdb
	HostHome    string `yaml:"host_home,omitempty"`
	// Extra keeps imported settings with no field yet, so nothing is lost.
	Extra map[string]string `yaml:"extra,omitempty"`
}

type Chain struct {
	ID           string `yaml:"id"`
	EVMChainID   uint64 `yaml:"evm_chain_id,omitempty"`
	Denom        string `yaml:"denom"`
	DisplayDenom string `yaml:"display_denom,omitempty"`
	Bech32Prefix string `yaml:"bech32_prefix,omitempty"`
}

type Image struct {
	Name   string `yaml:"name"`             // e.g. primium-evm
	Tag    string `yaml:"tag"`              // e.g. v0.7.2
	Repo   string `yaml:"repo,omitempty"`   // official source, e.g. cosmos/evm
	Naming string `yaml:"naming,omitempty"` // e.g. primium-{tag}
}

type Genesis struct {
	Validators     int       `yaml:"validators,omitempty"`
	InitialStake   string    `yaml:"initial_stake,omitempty"`
	InitialBalance string    `yaml:"initial_balance,omitempty"`
	Gov            Gov       `yaml:"gov"`
	Staking        Staking   `yaml:"staking"`
	Slashing       Slashing  `yaml:"slashing"`
	Feemarket      Feemarket `yaml:"feemarket"`
	EVM            EVM       `yaml:"evm"`
	Mint           Mint      `yaml:"mint"`
	Distribution   Distrib   `yaml:"distribution"`
}

type Gov struct {
	MinDeposit            string  `yaml:"min_deposit,omitempty"`
	ExpeditedMinDeposit   string  `yaml:"expedited_min_deposit,omitempty"`
	MaxDepositPeriodS     int64   `yaml:"max_deposit_period_s,omitempty"`
	VotingPeriodS         int64   `yaml:"voting_period_s,omitempty"`
	ExpeditedVotingPeriod int64   `yaml:"expedited_voting_period_s,omitempty"`
	Quorum                float64 `yaml:"quorum,omitempty"`
	Threshold             float64 `yaml:"threshold,omitempty"`
	VetoThreshold         float64 `yaml:"veto_threshold,omitempty"`
	ExpeditedThreshold    float64 `yaml:"expedited_threshold,omitempty"`
}

type Staking struct {
	UnbondingTimeS    int64   `yaml:"unbonding_time_s,omitempty"`
	MaxValidators     int64   `yaml:"max_validators,omitempty"`
	MinCommissionRate float64 `yaml:"min_commission_rate"`
}

type Slashing struct {
	SignedBlocksWindow    int64   `yaml:"signed_blocks_window,omitempty"`
	MinSignedPerWindow    float64 `yaml:"min_signed_per_window,omitempty"`
	DowntimeJailDurationS int64   `yaml:"downtime_jail_duration_s,omitempty"`
	SlashFractionDouble   float64 `yaml:"slash_fraction_double_sign,omitempty"`
	SlashFractionDowntime float64 `yaml:"slash_fraction_downtime,omitempty"`
}

type Feemarket struct {
	NoBaseFee                bool   `yaml:"no_base_fee"`
	BaseFee                  string `yaml:"base_fee,omitempty"`
	MinGasPrice              string `yaml:"min_gas_price,omitempty"`
	BaseFeeChangeDenominator int64  `yaml:"base_fee_change_denominator,omitempty"`
	ElasticityMultiplier     int64  `yaml:"elasticity_multiplier,omitempty"`
}

type EVM struct {
	Precompiles        []string `yaml:"precompiles,omitempty"`
	AccessCreate       string   `yaml:"access_create,omitempty"`
	AccessCall         string   `yaml:"access_call,omitempty"`
	HistoryServeWindow int64    `yaml:"history_serve_window,omitempty"`
	MaxGasPerBlock     int64    `yaml:"max_gas_per_block,omitempty"`
	ERC20              bool     `yaml:"erc20"`
}

type Mint struct {
	InflationRateChange float64 `yaml:"inflation_rate_change,omitempty"`
	InflationMax        float64 `yaml:"inflation_max,omitempty"`
	InflationMin        float64 `yaml:"inflation_min,omitempty"`
	GoalBonded          float64 `yaml:"goal_bonded,omitempty"`
	BlocksPerYear       int64   `yaml:"blocks_per_year,omitempty"`
}

type Distrib struct {
	CommunityTax        float64 `yaml:"community_tax,omitempty"`
	WithdrawAddrEnabled bool    `yaml:"withdraw_addr_enabled"`
}

// Consensus is config.toml's [consensus] timing, in milliseconds.
type Consensus struct {
	TimeoutProposeMS        int64 `yaml:"timeout_propose_ms,omitempty"`
	TimeoutProposeDeltaMS   int64 `yaml:"timeout_propose_delta_ms,omitempty"`
	TimeoutPrevoteMS        int64 `yaml:"timeout_prevote_ms,omitempty"`
	TimeoutPrevoteDeltaMS   int64 `yaml:"timeout_prevote_delta_ms,omitempty"`
	TimeoutPrecommitMS      int64 `yaml:"timeout_precommit_ms,omitempty"`
	TimeoutPrecommitDeltaMS int64 `yaml:"timeout_precommit_delta_ms,omitempty"`
	TimeoutCommitMS         int64 `yaml:"timeout_commit_ms,omitempty"`
	SkipTimeoutCommit       bool  `yaml:"skip_timeout_commit"`
	PeerGossipSleepMS       int64 `yaml:"peer_gossip_sleep_ms,omitempty"`
}

// Services are what a node exposes.
type Services struct {
	API        bool   `yaml:"api"`
	GRPC       bool   `yaml:"grpc"`
	JSONRPC    bool   `yaml:"jsonrpc"`
	WS         bool   `yaml:"ws"`
	Prometheus bool   `yaml:"prometheus"`
	APIPort    int    `yaml:"api_port,omitempty"`
	GRPCPort   int    `yaml:"grpc_port,omitempty"`
	RPCPort    int    `yaml:"jsonrpc_port,omitempty"`
	WSPort     int    `yaml:"ws_port,omitempty"`
	WSOrigins  string `yaml:"ws_origins,omitempty"`
}

// --- storage -------------------------------------------------------------------

var idRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Path is where a network's spec lives.
func Path(chainID string) (string, error) {
	if !idRe.MatchString(chainID) {
		return "", fmt.Errorf("network %q: want a chain id (letters, digits, . _ -)", chainID)
	}
	return config.Path("networks", chainID+".yaml")
}

// Load reads a spec by chain id or file path.
func Load(nameOrPath string) (*Spec, string, error) {
	p := nameOrPath
	if !strings.ContainsAny(nameOrPath, "/\\") && !strings.HasSuffix(nameOrPath, ".yaml") {
		var err error
		if p, err = Path(nameOrPath); err != nil {
			return nil, "", err
		}
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, "", fmt.Errorf("no network spec %s — import one: cometcli network import <network-config.env>", nameOrPath)
	}
	var s Spec
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, "", fmt.Errorf("%s: %w", p, err)
	}
	return &s, p, nil
}

// Save writes the spec under its chain id.
func (s *Spec) Save() (string, error) {
	p, err := Path(s.Chain.ID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	raw, err := yaml.Marshal(s)
	if err != nil {
		return "", err
	}
	return p, os.WriteFile(p, raw, 0o600)
}

// --- import ----------------------------------------------------------------------

// ParseEnv reads a shell env file (KEY="value" # comment).
func ParseEnv(raw string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "export ")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !envKeyRe.MatchString(k) {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case strings.HasPrefix(v, `"`):
			if i := strings.Index(v[1:], `"`); i >= 0 {
				v = v[1 : i+1]
			}
		case strings.HasPrefix(v, "'"):
			if i := strings.Index(v[1:], "'"); i >= 0 {
				v = v[1 : i+1]
			}
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = v[:i]
			}
			v = strings.TrimSpace(v)
		}
		out[k] = v
	}
	return out
}

var envKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// FromEnv builds a spec from node-setup's network-config.env keys. Keys it
// doesn't map are kept in Extra.
func FromEnv(env map[string]string) *Spec {
	used := map[string]bool{}
	str := func(k string) string { used[k] = true; return env[k] }
	i64 := func(k string) int64 { used[k] = true; n, _ := strconv.ParseInt(env[k], 10, 64); return n }
	f64 := func(k string) float64 { used[k] = true; f, _ := strconv.ParseFloat(env[k], 64); return f }
	b := func(k string) bool { used[k] = true; return strings.EqualFold(env[k], "true") }
	s := &Spec{}
	s.Chain = Chain{ID: str("COSMOS_CHAIN_ID"), Denom: str("CHAIN_DENOM"), DisplayDenom: str("CHAIN_DISPLAY_DENOM"), Bech32Prefix: "cosmos"}
	s.Chain.EVMChainID = uint64(i64("EVM_CHAIN_ID"))
	s.Image = Image{Name: str("DOCKER_IMAGE"), Tag: str("DOCKER_TAG"), Repo: "cosmos/evm"}
	s.Genesis.Validators = int(i64("NUM_VALIDATORS"))
	s.Genesis.InitialStake = str("VALIDATOR_INITIAL_STAKE")
	s.Genesis.InitialBalance = str("VALIDATOR_INITIAL_BALANCE")
	s.Genesis.Gov = Gov{
		MinDeposit: str("MIN_DEPOSIT"), ExpeditedMinDeposit: str("EXPEDITED_MIN_DEPOSIT"),
		MaxDepositPeriodS: i64("MAX_DEPOSIT_PERIOD"), VotingPeriodS: i64("VOTING_PERIOD"),
		ExpeditedVotingPeriod: i64("EXPEDITED_VOTING_PERIOD"), Quorum: f64("QUORUM"),
		Threshold: f64("THRESHOLD"), VetoThreshold: f64("VETO_THRESHOLD"), ExpeditedThreshold: f64("EXPEDITED_THRESHOLD"),
	}
	s.Genesis.Staking = Staking{UnbondingTimeS: i64("UNBONDING_TIME"), MaxValidators: i64("MAX_VALIDATORS"), MinCommissionRate: f64("MIN_COMMISSION_RATE")}
	s.Genesis.Slashing = Slashing{
		SignedBlocksWindow: i64("SIGNED_BLOCKS_WINDOW"), MinSignedPerWindow: f64("MIN_SIGNED_PER_WINDOW"),
		DowntimeJailDurationS: i64("DOWNTIME_JAIL_DURATION"), SlashFractionDouble: f64("SLASH_FRACTION_DOUBLE_SIGN"),
		SlashFractionDowntime: f64("SLASH_FRACTION_DOWNTIME"),
	}
	s.Genesis.Feemarket = Feemarket{
		NoBaseFee: b("FEEMARKET_NO_BASE_FEE"), BaseFee: str("FEEMARKET_BASE_FEE"), MinGasPrice: str("FEEMARKET_MIN_GAS_PRICE"),
		BaseFeeChangeDenominator: i64("FEEMARKET_BASE_FEE_CHANGE_DENOMINATOR"), ElasticityMultiplier: i64("FEEMARKET_ELASTICITY_MULTIPLIER"),
	}
	s.Genesis.EVM = EVM{
		AccessCreate: str("EVM_ACCESS_CONTROL_CREATE"), AccessCall: str("EVM_ACCESS_CONTROL_CALL"),
		HistoryServeWindow: i64("HISTORY_SERVE_WINDOW"), MaxGasPerBlock: i64("MAX_GAS_PER_BLOCK"), ERC20: b("ENABLE_ERC20"),
	}
	if b("ENABLE_PRECOMPILES") {
		for _, p := range strings.Split(str("PRECOMPILES_LIST"), ",") {
			if p = strings.TrimSpace(p); p != "" {
				s.Genesis.EVM.Precompiles = append(s.Genesis.EVM.Precompiles, p)
			}
		}
	}
	s.Genesis.Mint = Mint{InflationRateChange: f64("INFLATION_RATE_CHANGE"), InflationMax: f64("INFLATION_MAX"),
		InflationMin: f64("INFLATION_MIN"), GoalBonded: f64("GOAL_BONDED"), BlocksPerYear: i64("BLOCKS_PER_YEAR")}
	s.Genesis.Distribution = Distrib{CommunityTax: f64("COMMUNITY_TAX"), WithdrawAddrEnabled: b("WITHDRAW_ADDR_ENABLED")}
	s.Consensus = Consensus{
		TimeoutProposeMS: i64("TIMEOUT_PROPOSE"), TimeoutProposeDeltaMS: i64("TIMEOUT_PROPOSE_DELTA"),
		TimeoutPrevoteMS: i64("TIMEOUT_PREVOTE"), TimeoutPrevoteDeltaMS: i64("TIMEOUT_PREVOTE_DELTA"),
		TimeoutPrecommitMS: i64("TIMEOUT_PRECOMMIT"), TimeoutPrecommitDeltaMS: i64("TIMEOUT_PRECOMMIT_DELTA"),
		TimeoutCommitMS: i64("TIMEOUT_COMMIT"), SkipTimeoutCommit: b("SKIP_TIMEOUT_COMMIT"),
		PeerGossipSleepMS: i64("PEER_GOSSIP_SLEEP_DURATION"),
	}
	s.Services = Services{
		API: b("ENABLE_API"), GRPC: b("ENABLE_GRPC"), JSONRPC: b("ENABLE_JSONRPC"), WS: b("ENABLE_WS"), Prometheus: b("ENABLE_PROMETHEUS"),
		APIPort: int(i64("API_PORT")), GRPCPort: int(i64("GRPC_PORT")), RPCPort: int(i64("JSONRPC_PORT")), WSPort: int(i64("WS_PORT")),
		WSOrigins: str("WS_ORIGINS"),
	}
	s.MinGasPrice = str("MIN_GAS_PRICE")
	s.DBBackend = str("DB_BACKEND")
	s.HostHome = str("PRIMIUM_HOST_HOME")
	for k, v := range env {
		if !used[k] {
			if s.Extra == nil {
				s.Extra = map[string]string{}
			}
			s.Extra[k] = v
		}
	}
	return s
}

// Merge fills s's unset fields from o (importing several env files: the
// genesis one is the most complete; validator/archive ones add host homes).
func (s *Spec) Merge(o *Spec) {
	a, _ := yaml.Marshal(o)
	var base, over map[string]any
	_ = yaml.Unmarshal(a, &over)
	b, _ := yaml.Marshal(s)
	_ = yaml.Unmarshal(b, &base)
	fill(base, over)
	merged, _ := yaml.Marshal(base)
	_ = yaml.Unmarshal(merged, s)
}

func fill(dst, src map[string]any) {
	for k, v := range src {
		cur, ok := dst[k]
		if !ok || cur == nil || cur == "" || cur == 0 {
			dst[k] = v
			continue
		}
		if dm, ok := cur.(map[string]any); ok {
			if sm, ok := v.(map[string]any); ok {
				fill(dm, sm)
			}
		}
	}
}

// --- review --------------------------------------------------------------------

// Severity of a finding.
type Severity int

const (
	Info Severity = iota
	Warn
	Fail
)

func (s Severity) String() string { return [...]string{"INFO", "WARN", "FAIL"}[s] }

// Finding is one review result.
type Finding struct {
	Sev   Severity
	Where string // "spec", "chain", or node:file
	What  string
	Fix   string
}

// knownPrecompiles are cosmos/evm's static precompile names.
var knownPrecompiles = map[string]bool{"p256": true, "bech32": true, "staking": true, "distribution": true, "ics20": true,
	"ics02": true, "vesting": true, "bank": true, "gov": true, "slashing": true, "evidence": true, "erc20": true, "werc20": true, "callbacks": true}

// Review checks the spec for mistakes, and with prod also for values that
// are fine on a test network but not on a public one.
func (s *Spec) Review(prod bool) []Finding {
	var out []Finding
	add := func(sev Severity, what, fix string) {
		out = append(out, Finding{Sev: sev, Where: "spec", What: what, Fix: fix})
	}
	g := s.Genesis
	if s.Chain.ID == "" || s.Chain.Denom == "" {
		add(Fail, "chain id or denom is empty", "set chain.id and chain.denom")
	}
	if s.DBBackend != "" && s.DBBackend != "goleveldb" && s.DBBackend != "rocksdb" {
		add(Fail, "db_backend "+s.DBBackend+" isn't goleveldb or rocksdb", "")
	}
	if g.Gov.ExpeditedVotingPeriod > 0 && g.Gov.VotingPeriodS > 0 && g.Gov.ExpeditedVotingPeriod >= g.Gov.VotingPeriodS {
		add(Fail, fmt.Sprintf("expedited voting period (%ds) must be shorter than the voting period (%ds)", g.Gov.ExpeditedVotingPeriod, g.Gov.VotingPeriodS), "")
	}
	if g.Gov.ExpeditedThreshold > 0 && g.Gov.ExpeditedThreshold <= g.Gov.Threshold {
		add(Fail, "expedited threshold must be above the threshold", "")
	}
	for name, v := range map[string]float64{"gov.quorum": g.Gov.Quorum, "gov.threshold": g.Gov.Threshold, "gov.veto_threshold": g.Gov.VetoThreshold,
		"slashing.min_signed_per_window": g.Slashing.MinSignedPerWindow, "staking.min_commission_rate": g.Staking.MinCommissionRate} {
		if v < 0 || v > 1 {
			add(Fail, fmt.Sprintf("%s = %v is outside 0..1", name, v), "")
		}
	}
	for _, p := range g.EVM.Precompiles {
		if !knownPrecompiles[p] {
			add(Warn, "unknown precompile "+p, "check the name against the cosmos/evm release")
		}
	}
	if g.Slashing.SignedBlocksWindow > 0 && g.Slashing.SignedBlocksWindow < 100 {
		add(Warn, fmt.Sprintf("signed_blocks_window %d is very short: a brief restart jails a validator", g.Slashing.SignedBlocksWindow), "")
	}
	if !prod {
		return out
	}
	day := int64(86400)
	if g.Gov.VotingPeriodS > 0 && g.Gov.VotingPeriodS < 2*day {
		add(Warn, fmt.Sprintf("voting period is %s", human(g.Gov.VotingPeriodS)), "172800 (2 days) for a public network")
	}
	if g.Gov.ExpeditedVotingPeriod > 0 && g.Gov.ExpeditedVotingPeriod < day {
		add(Warn, fmt.Sprintf("expedited voting period is %s", human(g.Gov.ExpeditedVotingPeriod)), "86400 (1 day)")
	}
	if g.Staking.UnbondingTimeS > 0 && g.Staking.UnbondingTimeS < 21*day {
		add(Warn, fmt.Sprintf("unbonding time is %s", human(g.Staking.UnbondingTimeS)), "1814400 (21 days)")
	}
	if c := s.Consensus; c.TimeoutProposeMS > 0 && c.TimeoutProposeMS < 1000 {
		add(Warn, fmt.Sprintf("consensus timeouts are tuned for a LAN (propose %dms, commit %dms)", c.TimeoutProposeMS, c.TimeoutCommitMS), "about 2000ms / 5000ms for public P2P")
	}
	if s.Services.WS && (s.Services.WSOrigins == "" || s.Services.WSOrigins == "*") {
		add(Warn, "WebSocket accepts any origin (ws_origins *)", "your dApp's origins, e.g. https://app.example.com")
	}
	if g.Gov.MinDeposit == "10000000" {
		add(Info, "min_deposit is the test default (10000000"+s.Chain.Denom+")", "raise it to match the token's value")
	}
	return out
}

func human(sec int64) string {
	switch {
	case sec%86400 == 0:
		return fmt.Sprintf("%dd", sec/86400)
	case sec%3600 == 0:
		return fmt.Sprintf("%dh", sec/3600)
	case sec%60 == 0:
		return fmt.Sprintf("%dm", sec/60)
	}
	return fmt.Sprintf("%ds", sec)
}

// Sort orders findings worst first.
func Sort(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool { return f[i].Sev > f[j].Sev })
}

// GasDenom is what fees are paid in.
func (s *Spec) GasDenom() string {
	if s.FeeDenom != "" {
		return s.FeeDenom
	}
	return s.Chain.Denom
}
