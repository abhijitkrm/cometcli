// Package networktool implements network.* tools over a network spec
// (package netspec): check a spec, the chain and every node against it.
package networktool

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	govv1 "cosmossdk.io/api/cosmos/gov/v1"
	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"github.com/BurntSushi/toml"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

// Register adds the network.* tools.
func Register(r *toolkit.Registry) { r.Register(checkTool{}) }

type checkTool struct{}

func (checkTool) Name() string { return "network.check" }
func (checkTool) Desc() string {
	return "Check a network against its spec (~/.cometcli/networks/<chain-id>.yaml): the spec's own sanity (and, with prod, " +
		"test-only values), the chain's live gov/staking/slashing params, and every node's config.toml / app.toml / container " +
		"command against its role (validator, archive, rpc) — mempool, db backend, gas price, CORS/unlock hardening, archive pruning."
}
func (checkTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"network": toolkit.Str("chain id or spec path (default: this profile's chain id)"),
		"prod":    toolkit.Bool("also flag values that are fine on a test network but not a public one"),
		"nodes":   toolkit.Str("comma-separated profiles to check (default: every profile of the chain)"),
	})
}
func (checkTool) Tier() toolkit.Tier                 { return toolkit.TierDiagnose }
func (checkTool) Timeout(toolkit.Args) time.Duration { return 5 * time.Minute }

func (checkTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	name := a.String("network", "")
	if name == "" && c.Profile != nil {
		name = c.Profile.ChainID
	}
	if name == "" {
		return nil, fmt.Errorf("which network? pass network (a chain id or spec file)")
	}
	spec, path, err := netspec.Load(name)
	if err != nil {
		return nil, err
	}
	prod := a.Bool("prod", false)
	findings := spec.Review(prod)

	// the chain itself, through any node of it
	profiles := chainProfiles(c, spec.Chain.ID, a.String("nodes", ""))
	if len(profiles) > 0 {
		pc := &toolkit.Context{Context: c.Context, Profile: profiles[0], Cfg: c.Cfg, Audit: c.Audit}
		findings = append(findings, ChainDrift(pc, spec)...)
		pc.Close()
	}
	// every node, in parallel
	var mu sync.Mutex
	var wg sync.WaitGroup
	checked := 0
	for _, p := range profiles {
		wg.Add(1)
		go func(p *config.Profile) {
			defer wg.Done()
			pc := &toolkit.Context{Context: c.Context, Profile: p, Cfg: c.Cfg, Audit: c.Audit}
			defer pc.Close()
			f, err := CheckNode(pc, spec, prod)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				findings = append(findings, netspec.Finding{Sev: netspec.Warn, Where: p.Name, What: "couldn't check: " + err.Error()})
				return
			}
			checked++
			findings = append(findings, f...)
		}(p)
	}
	wg.Wait()
	netspec.Sort(findings)

	var b strings.Builder
	fmt.Fprintf(&b, "network %s (%s) — %d node(s) checked%s\n", spec.Chain.ID, path, checked, map[bool]string{true: ", production rules", false: ""}[prod])
	counts := map[netspec.Severity]int{}
	// grouped: the spec, the chain, then each node
	groups := map[string][]netspec.Finding{}
	var order []string
	for _, f := range findings {
		counts[f.Sev]++
		g, _, _ := strings.Cut(f.Where, ":")
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], f)
	}
	sort.SliceStable(order, func(i, j int) bool { return rank(order[i]) < rank(order[j]) })
	for _, g := range order {
		fmt.Fprintf(&b, "\n%s\n", g)
		for _, f := range groups[g] {
			where := ""
			if _, file, ok := strings.Cut(f.Where, ":"); ok {
				where = file + " "
			}
			fmt.Fprintf(&b, "  %-4s %s%s", f.Sev, where, f.What)
			if f.Fix != "" {
				fmt.Fprintf(&b, " → %s", f.Fix)
			}
			b.WriteByte('\n')
		}
	}
	if len(findings) == 0 {
		b.WriteString("PASS everything matches the spec\n")
	}
	fmt.Fprintf(&b, "%d fail, %d warn, %d info\n", counts[netspec.Fail], counts[netspec.Warn], counts[netspec.Info])
	return &toolkit.Result{Text: b.String(), Data: map[string]any{
		"fail": counts[netspec.Fail], "warn": counts[netspec.Warn], "info": counts[netspec.Info], "nodes": checked,
	}}, nil
}

// chainProfiles are the profiles of a chain (or the ones named).
func chainProfiles(c *toolkit.Context, chainID, names string) []*config.Profile {
	var out []*config.Profile
	if c.Cfg == nil {
		if c.Profile != nil && c.Profile.ChainID == chainID {
			out = append(out, c.Profile)
		}
		return out
	}
	want := map[string]bool{}
	for _, n := range strings.Split(names, ",") {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	var keys []string
	for n := range c.Cfg.Profiles {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	for _, n := range keys {
		p := c.Cfg.Profiles[n]
		p.Name = n
		if (len(want) > 0 && want[n]) || (len(want) == 0 && p.ChainID == chainID) {
			out = append(out, p)
		}
	}
	return out
}

// --- one node -----------------------------------------------------------------

// CheckNode checks a node's config files and container command against
// its role under spec.
func CheckNode(c *toolkit.Context, spec *netspec.Spec, prod bool) ([]netspec.Finding, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	role := netspec.RoleOf(c.Profile.Role)
	var cfgDoc, appDoc map[string]any
	for _, f := range []struct {
		rel string
		dst *map[string]any
	}{{"config/config.toml", &cfgDoc}, {"config/app.toml", &appDoc}} {
		raw, _, err := common.ReadNodeFile(c, h, f.rel)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.rel, err)
		}
		if err := toml.Unmarshal(raw, f.dst); err != nil {
			return []netspec.Finding{{Sev: netspec.Fail, Where: c.Profile.Name + ":" + f.rel, What: "doesn't parse: " + err.Error()}}, nil
		}
	}
	version, cmd := containerInfo(c, h)
	f := netspec.Evaluate(c.Profile.Name, spec.Expectations(role, version, prod), cfgDoc, appDoc, cmd)
	if role == netspec.Validator && c.Profile.Role == "" {
		for i := range f {
			f[i].Where += " (role validator)"
		}
	}
	return f, nil
}

// containerInfo reads a docker node's image version and command.
func containerInfo(c *toolkit.Context, h host.Host) (version string, cmd []string) {
	p := c.Profile
	if p.Service.Type != "docker" || p.Service.Unit == "" {
		return "", nil
	}
	sub, cancel := toolkit.WithDeadline(c, 30*time.Second)
	defer cancel()
	res, err := host.Exec(sub, h, "docker inspect -f '{{json .Config.Cmd}}|{{index .Config.Labels \"org.opencontainers.image.version\"}}|{{.Config.Image}}' "+common.ShellQ(p.Service.Unit), 64<<10)
	if err != nil || res.Code != 0 {
		return "", nil
	}
	parts := strings.SplitN(strings.TrimSpace(res.Output), "|", 3)
	if len(parts) < 3 {
		return "", nil
	}
	_ = json.Unmarshal([]byte(parts[0]), &cmd)
	version = parts[1]
	if version == "" || version == "<no value>" {
		// primium-v0.7.2 or primium-evm:v0.7.2
		if _, tag, ok := strings.Cut(parts[2], ":"); ok {
			version = tag
		} else if i := strings.LastIndex(parts[2], "-v"); i >= 0 {
			version = parts[2][i+1:]
		}
	}
	if cmd == nil {
		cmd = []string{}
	}
	return version, cmd
}

// --- the chain ------------------------------------------------------------------

// ChainDrift compares the chain's live parameters with the spec's genesis
// values: a network whose params moved (by governance) or never matched.
func ChainDrift(c *toolkit.Context, spec *netspec.Spec) []netspec.Finding {
	g, err := c.GRPC()
	if err != nil {
		return []netspec.Finding{{Sev: netspec.Info, Where: "chain", What: "couldn't read chain params: " + err.Error()}}
	}
	var out []netspec.Finding
	diff := func(name string, spec, chain any) {
		s, ch := fmt.Sprint(spec), fmt.Sprint(chain)
		if s == "" || s == "0" || s == ch {
			return
		}
		out = append(out, netspec.Finding{Sev: netspec.Info, Where: "chain",
			What: fmt.Sprintf("%s: spec %s, chain %s", name, s, ch), Fix: "update the spec (or the chain, by governance)"})
	}
	gs := spec.Genesis
	if p, err := g.Staking.Params(c, &stakingv1beta1.QueryParamsRequest{}); err == nil && p.Params != nil {
		diff("staking.unbonding_time_s", gs.Staking.UnbondingTimeS, int64(p.Params.UnbondingTime.AsDuration().Seconds()))
		diff("staking.max_validators", gs.Staking.MaxValidators, p.Params.MaxValidators)
		if spec.Chain.Denom != "" && p.Params.BondDenom != spec.Chain.Denom {
			out = append(out, netspec.Finding{Sev: netspec.Fail, Where: "chain",
				What: fmt.Sprintf("bond denom is %s, the spec says %s — is this the right network?", p.Params.BondDenom, spec.Chain.Denom)})
		}
	}
	if p, err := g.Slashing.Params(c, &slashingv1beta1.QueryParamsRequest{}); err == nil && p.Params != nil {
		diff("slashing.signed_blocks_window", gs.Slashing.SignedBlocksWindow, p.Params.SignedBlocksWindow)
		diff("slashing.min_signed_per_window", fmtF(gs.Slashing.MinSignedPerWindow), common.DecFrac(string(p.Params.MinSignedPerWindow)))
		diff("slashing.downtime_jail_duration_s", gs.Slashing.DowntimeJailDurationS, int64(p.Params.DowntimeJailDuration.AsDuration().Seconds()))
	}
	if p, err := g.GovV1.Params(c, &govv1.QueryParamsRequest{ParamsType: "voting"}); err == nil && p.Params != nil {
		if p.Params.VotingPeriod != nil {
			diff("gov.voting_period_s", gs.Gov.VotingPeriodS, int64(p.Params.VotingPeriod.AsDuration().Seconds()))
		}
		if p.Params.ExpeditedVotingPeriod != nil {
			diff("gov.expedited_voting_period_s", gs.Gov.ExpeditedVotingPeriod, int64(p.Params.ExpeditedVotingPeriod.AsDuration().Seconds()))
		}
		diff("gov.quorum", fmtF(gs.Gov.Quorum), trimDec(p.Params.Quorum))
		diff("gov.threshold", fmtF(gs.Gov.Threshold), trimDec(p.Params.Threshold))
		for _, coin := range p.Params.MinDeposit {
			if coin.Denom == spec.Chain.Denom {
				diff("gov.min_deposit", gs.Gov.MinDeposit, coin.Amount)
			}
		}
	}
	return out
}

func fmtF(f float64) string {
	if f == 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// trimDec renders "0.500000000000000000" as "0.5".
func trimDec(s string) string {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func rank(group string) int {
	switch group {
	case "spec":
		return 0
	case "chain":
		return 1
	}
	return 2
}
