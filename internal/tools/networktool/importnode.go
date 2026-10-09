package networktool

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	govv1 "cosmossdk.io/api/cosmos/gov/v1"
	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"github.com/BurntSushi/toml"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

var durationRe = regexp.MustCompile(`^(\d+)(ms|s)$`)

func durationMS(v any) int64 {
	m := durationRe.FindStringSubmatch(fmt.Sprint(v))
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	if m[2] == "s" {
		n *= 1000
	}
	return n
}

func boolOf(v any) bool { return v == true }

func intOf(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	n, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
	return n
}

func portOf(addr any) int {
	s := fmt.Sprint(addr)
	if i := strings.LastIndex(s, ":"); i >= 0 {
		n, _ := strconv.Atoi(s[i+1:])
		return n
	}
	return 0
}

// FromNode describes a running network from one of its nodes: identity
// and live params from the chain, timing and services from the node's
// config, the image from its container — plus the genesis, byte for byte.
func FromNode(c *toolkit.Context) (*netspec.Spec, []byte, error) {
	cc, err := c.Comet()
	if err != nil {
		return nil, nil, err
	}
	st, err := cc.Status(c)
	if err != nil {
		return nil, nil, err
	}
	s := &netspec.Spec{}
	s.Chain.ID = st.NodeInfo.Network
	s.Chain.Bech32Prefix = c.Profile.Bech32Prefix
	if g, err := c.GRPC(); err == nil {
		if p, err := g.Staking.Params(c, &stakingv1beta1.QueryParamsRequest{}); err == nil && p.Params != nil {
			s.Chain.Denom = p.Params.BondDenom
			s.Genesis.Staking.UnbondingTimeS = int64(p.Params.UnbondingTime.AsDuration().Seconds())
			s.Genesis.Staking.MaxValidators = int64(p.Params.MaxValidators)
			s.Genesis.Staking.MinCommissionRate, _ = strconv.ParseFloat(common.DecFrac(p.Params.MinCommissionRate), 64)
		}
		if p, err := g.Slashing.Params(c, &slashingv1beta1.QueryParamsRequest{}); err == nil && p.Params != nil {
			sl := &s.Genesis.Slashing
			sl.SignedBlocksWindow = p.Params.SignedBlocksWindow
			sl.MinSignedPerWindow, _ = strconv.ParseFloat(common.DecFrac(string(p.Params.MinSignedPerWindow)), 64)
			sl.DowntimeJailDurationS = int64(p.Params.DowntimeJailDuration.AsDuration().Seconds())
			sl.SlashFractionDouble, _ = strconv.ParseFloat(common.DecFrac(string(p.Params.SlashFractionDoubleSign)), 64)
			sl.SlashFractionDowntime, _ = strconv.ParseFloat(common.DecFrac(string(p.Params.SlashFractionDowntime)), 64)
		}
		if p, err := g.GovV1.Params(c, &govv1.QueryParamsRequest{ParamsType: "voting"}); err == nil && p.Params != nil {
			gv := &s.Genesis.Gov
			if p.Params.VotingPeriod != nil {
				gv.VotingPeriodS = int64(p.Params.VotingPeriod.AsDuration().Seconds())
			}
			if p.Params.ExpeditedVotingPeriod != nil {
				gv.ExpeditedVotingPeriod = int64(p.Params.ExpeditedVotingPeriod.AsDuration().Seconds())
			}
			if p.Params.MaxDepositPeriod != nil {
				gv.MaxDepositPeriodS = int64(p.Params.MaxDepositPeriod.AsDuration().Seconds())
			}
			gv.Quorum, _ = strconv.ParseFloat(p.Params.Quorum, 64)
			gv.Threshold, _ = strconv.ParseFloat(p.Params.Threshold, 64)
			gv.VetoThreshold, _ = strconv.ParseFloat(p.Params.VetoThreshold, 64)
			gv.ExpeditedThreshold, _ = strconv.ParseFloat(p.Params.ExpeditedThreshold, 64)
			for _, coin := range p.Params.MinDeposit {
				gv.MinDeposit = coin.Amount
			}
			for _, coin := range p.Params.ExpeditedMinDeposit {
				gv.ExpeditedMinDeposit = coin.Amount
			}
		}
	}
	// the node's own config: timing, services, fees, db
	h, err := c.Host()
	if err != nil {
		return nil, nil, err
	}
	var cfg, app map[string]any
	if raw, _, err := common.ReadNodeFile(c, h, "config/config.toml"); err == nil {
		_ = toml.Unmarshal(raw, &cfg)
	}
	if raw, _, err := common.ReadNodeFile(c, h, "config/app.toml"); err == nil {
		_ = toml.Unmarshal(raw, &app)
	}
	get := func(doc map[string]any, keys ...string) any { return netspec.Lookup(doc, keys) }
	if cfg != nil {
		s.DBBackend = fmt.Sprint(get(cfg, "db_backend"))
		cs := &s.Consensus
		cs.TimeoutProposeMS = durationMS(get(cfg, "consensus", "timeout_propose"))
		cs.TimeoutProposeDeltaMS = durationMS(get(cfg, "consensus", "timeout_propose_delta"))
		cs.TimeoutPrevoteMS = durationMS(get(cfg, "consensus", "timeout_prevote"))
		cs.TimeoutPrevoteDeltaMS = durationMS(get(cfg, "consensus", "timeout_prevote_delta"))
		cs.TimeoutPrecommitMS = durationMS(get(cfg, "consensus", "timeout_precommit"))
		cs.TimeoutPrecommitDeltaMS = durationMS(get(cfg, "consensus", "timeout_precommit_delta"))
		cs.TimeoutCommitMS = durationMS(get(cfg, "consensus", "timeout_commit"))
		cs.PeerGossipSleepMS = durationMS(get(cfg, "consensus", "peer_gossip_sleep_duration"))
		cs.SkipTimeoutCommit = boolOf(get(cfg, "consensus", "skip_timeout_commit"))
		s.Services.Prometheus = boolOf(get(cfg, "instrumentation", "prometheus"))
	}
	if app != nil {
		sv := &s.Services
		sv.API, sv.GRPC, sv.JSONRPC = boolOf(get(app, "api", "enable")), boolOf(get(app, "grpc", "enable")), boolOf(get(app, "json-rpc", "enable"))
		sv.WS = sv.JSONRPC && fmt.Sprint(get(app, "json-rpc", "ws-address")) != ""
		sv.APIPort, sv.GRPCPort = portOf(get(app, "api", "address")), portOf(get(app, "grpc", "address"))
		sv.RPCPort, sv.WSPort = portOf(get(app, "json-rpc", "address")), portOf(get(app, "json-rpc", "ws-address"))
		if id := intOf(get(app, "evm", "evm-chain-id")); id > 0 {
			s.Chain.EVMChainID = uint64(id)
		}
		gp := fmt.Sprint(get(app, "minimum-gas-prices"))
		if amount, denom := toolkit.ParseMinGasPrices("minimum-gas-prices = \"" + gp + "\""); denom != "" {
			s.MinGasPrice = amount
			if denom != s.Chain.Denom {
				s.FeeDenom = denom // EVM chains often pay fees in another denom than they bond
			}
		}
		if db := fmt.Sprint(get(app, "app-db-backend")); db != "" && db != "<nil>" && s.DBBackend == "" {
			s.DBBackend = db
		}
	}
	// the image, from the container
	if c.Profile.Service.Type == "docker" && c.Profile.Service.Unit != "" {
		sub, cancel := toolkit.WithDeadline(c, 30*time.Second)
		res, err := hostExec(sub, h, "docker inspect -f '{{.Config.Image}}|{{index .Config.Labels \"org.opencontainers.image.source\"}}|{{json .Config.Cmd}}' "+common.ShellQ(c.Profile.Service.Unit))
		cancel()
		if err == nil {
			parts := strings.SplitN(strings.TrimSpace(res), "|", 3)
			if len(parts) == 3 {
				name, tag, _ := strings.Cut(parts[0], ":")
				s.Image.Name, s.Image.Tag = name, tag
				if _, repo, ok := strings.Cut(parts[1], "github.com/"); ok {
					s.Image.Repo = strings.TrimSuffix(repo, "/")
				}
				if m := regexp.MustCompile(`--json-rpc\.ws-origins=([^"]+)`).FindStringSubmatch(parts[2]); m != nil {
					s.Services.WSOrigins = m[1]
				}
			}
		}
	}
	gen, err := FetchGenesis(c)
	if err != nil {
		return s, nil, err
	}
	return s, gen, nil
}

func hostExec(c *toolkit.Context, h host.Host, script string) (string, error) {
	res, err := host.Exec(c, h, script, 1<<20)
	if err == nil && res.Code != 0 {
		err = fmt.Errorf("exit %d", res.Code)
	}
	return res.Output, err
}
