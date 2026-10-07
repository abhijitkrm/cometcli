// Package triage implements node.triage: one deterministic sweep that
// collects ~50 flat signals about the node, validator, host, logs and
// configs in parallel, then matches them against the knowledge base so
// the agent starts from a ranked list of known cases instead of guessing.
package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	govv1 "cosmossdk.io/api/cosmos/gov/v1"
	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	upgradev1beta1 "cosmossdk.io/api/cosmos/upgrade/v1beta1"
	"github.com/BurntSushi/toml"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/keys"
	"github.com/abhijitkrm/cometcli/internal/logscan"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
	"github.com/abhijitkrm/cometcli/internal/tools/val"
)

// Timeout bounds each collector (tests lower it).
var Timeout = 20 * time.Second

// Report is one sweep's outcome.
type Report struct {
	Chain   string            // cosmos-sdk | cosmos-evm
	Signals kb.Signals        // flat name → value
	Sources map[string]string // collector → "ok" or why it was unavailable
	Samples map[string][]string
	Since   time.Duration // log window
}

// ChainType decides which case set applies: profile metadata chain_type
// wins, else any EVM marker makes it cosmos-evm.
func ChainType(c *toolkit.Context) string {
	p := c.Profile
	if p == nil {
		return ""
	}
	if t := p.Metadata["chain_type"]; t != "" {
		return t
	}
	if p.EVMChainID != 0 || p.Endpoints.EVM != "" || strings.Contains(strings.ToLower(p.Binary), "evm") {
		return "cosmos-evm"
	}
	return "cosmos-sdk"
}

type collector struct {
	name     string
	fn       func(ctx *toolkit.Context, r *Report, set func(string, any)) error
	prefixes []string // signal families it produces (derived ones need comet too)
}

// collectors in display order.
var collectors = []collector{
	{"comet", collectComet, []string{"node."}},
	{"chain", collectChain, []string{"val.", "upgrade.", "node.key_mismatch"}},
	{"gov", collectGov, []string{"gov."}},
	{"host", collectHost, []string{"host.", "clock."}},
	{"process", collectProcess, []string{"proc."}},
	{"logs", collectLogs, []string{"logs."}},
	{"config", collectConfig, []string{"config.", "app.", "pvs."}},
	{"evm", collectEVM, []string{"evm."}},
}

// Collect runs every collector concurrently; one failing source is
// reported in Sources and never fails the sweep.
func Collect(c *toolkit.Context, since time.Duration) *Report { return CollectFor(c, since, nil) }

// CollectFor runs only the collectors that produce the named signals
// (all of them when names is empty) — wait.until polls this way.
func CollectFor(c *toolkit.Context, since time.Duration, names []string) *Report {
	want := func(col collector) bool {
		if len(names) == 0 || col.name == "comet" {
			return true
		}
		for _, n := range names {
			for _, p := range col.prefixes {
				if strings.HasPrefix(n, p) {
					return true
				}
			}
		}
		return false
	}
	r := &Report{Chain: ChainType(c), Signals: kb.Signals{}, Sources: map[string]string{}, Samples: map[string][]string{}, Since: since}
	r.Signals["chain.type"] = r.Chain
	if c.Profile != nil {
		r.Signals["profile.role"] = c.Profile.Role
	}
	r.Signals["logs.window_min"] = since.Minutes()
	// dial shared clients once, up front (their errors surface per collector)
	if c.Profile != nil {
		_, _ = c.Comet()
		if c.Profile.Endpoints.GRPC != "" {
			_, _ = c.GRPC()
		}
		if c.Profile.Endpoints.EVM != "" {
			_, _ = c.EVM()
		}
	}
	_, _ = c.Host()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, col := range collectors {
		if (col.name == "evm" && r.Chain != "cosmos-evm") || !want(col) {
			continue
		}
		wg.Add(1)
		go func(col collector) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(c, Timeout)
			defer cancel()
			cc := c.Derive(ctx)
			local := kb.Signals{}
			var mu2 sync.Mutex // set may race with a timed-out collector
			guarded := func(k string, v any) { mu2.Lock(); local[k] = v; mu2.Unlock() }
			done := make(chan error, 1)
			go func() { done <- safe(func() error { return col.fn(cc, r, guarded) }) }()
			var err error
			select {
			case err = <-done:
			case <-ctx.Done():
				err = fmt.Errorf("timed out after %s", Timeout)
			}
			mu2.Lock()
			defer mu2.Unlock()
			mu.Lock()
			defer mu.Unlock()
			for k, v := range local {
				if k == "_samples" {
					r.Samples = v.(map[string][]string)
					continue
				}
				r.Signals[k] = v
			}
			if err != nil {
				r.Sources[col.name] = err.Error()
			} else {
				r.Sources[col.name] = "ok"
			}
		}(col)
	}
	wg.Wait()
	derive(r)
	return r
}

func safe(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("collector panicked: %v", p)
		}
	}()
	return fn()
}

// --- collectors ---------------------------------------------------------------

func collectComet(c *toolkit.Context, r *Report, set func(string, any)) error {
	cc, err := c.Comet()
	if err != nil {
		set("node.reachable", false)
		return err
	}
	st, err := cc.Status(c)
	if err != nil {
		set("node.reachable", false)
		return err
	}
	set("node.reachable", true)
	set("node.height", float64(st.SyncInfo.LatestBlockHeight))
	set("node.catching_up", st.SyncInfo.CatchingUp)
	set("node.block_age_s", time.Since(st.SyncInfo.LatestBlockTime).Round(time.Second).Seconds())
	set("node.version", st.NodeInfo.Version)
	set("node.network", st.NodeInfo.Network)
	set("node.voting_power", float64(st.ValidatorInfo.VotingPower))
	set("node.cons_addr_hex", strings.ToUpper(fmt.Sprintf("%x", []byte(st.ValidatorInfo.Address))))
	if ni, err := cc.NetInfo(c); err == nil {
		set("node.peers", float64(ni.NPeers))
	}
	return nil
}

func collectChain(c *toolkit.Context, r *Report, set func(string, any)) error {
	if c.Profile == nil || c.Profile.Endpoints.GRPC == "" {
		return fmt.Errorf("no grpc endpoint in profile")
	}
	f, err := val.Gather(c, nil)
	if err != nil {
		return err
	}
	set("chain.via_fallback", c.GRPCFallback)
	set("val.jailed", f.Jailed)
	set("val.tombstoned", f.Tombstoned)
	set("val.status", f.Status)
	set("val.bonded", f.Status == "BONDED")
	set("val.missed", float64(f.Missed))
	if f.Window > 0 {
		set("val.window", float64(f.Window))
		set("val.missed_pct", round1(100*float64(f.Missed)/float64(f.Window)))
	}
	if !f.JailedUntil.IsZero() && f.JailedUntil.After(time.Unix(0, 0)) {
		left := time.Until(f.JailedUntil).Seconds()
		if left < 0 {
			left = 0
		}
		set("val.jail_remaining_s", float64(int64(left)))
	}
	if f.MinSelfDelegation != "" && f.SelfDelegation != "" {
		set("val.self_below_min", lessThan(f.SelfDelegation, f.MinSelfDelegation))
	}
	if f.FeeBalance != "" {
		set("val.fee_balance_zero", f.FeeBalance == "0")
	}
	if len(f.ConsHex) > 0 {
		set("val.cons_addr_hex", strings.ToUpper(fmt.Sprintf("%x", f.ConsHex)))
		if f.CometErr == "" {
			if power, err := val.InValidatorSet(c, f.ConsHex); err == nil {
				set("val.in_set", power > 0)
				set("val.power", float64(power))
			}
			if signed, checked, err := val.SignedInWindow(c, f.ConsHex, 20); err == nil && checked > 0 {
				set("val.signed_recent_pct", round1(100*float64(signed)/float64(checked)))
			}
		}
	}
	if g, err := c.GRPC(); err == nil {
		// how much of the downtime budget is spent: 100 = jailed next miss
		if p, err := g.Slashing.Params(c, &slashingv1beta1.QueryParamsRequest{}); err == nil && p.Params != nil && f.Window > 0 {
			if min, ok := decFrac(p.Params.MinSignedPerWindow); ok && min < 1 {
				budget := float64(f.Window) * (1 - min)
				if budget > 0 {
					set("val.downtime_budget_used_pct", round1(100*float64(f.Missed)/budget))
				}
			}
		}
		if plan, err := g.Upgrade.CurrentPlan(c, &upgradev1beta1.QueryCurrentPlanRequest{}); err == nil {
			if plan.Plan != nil && plan.Plan.Name != "" {
				set("upgrade.pending", true)
				set("upgrade.name", plan.Plan.Name)
				set("upgrade.height", float64(plan.Plan.Height))
			} else {
				set("upgrade.pending", false)
			}
		}
	}
	return nil
}

func collectGov(c *toolkit.Context, r *Report, set func(string, any)) error {
	if c.Profile == nil || c.Profile.Endpoints.GRPC == "" {
		return fmt.Errorf("no grpc endpoint in profile")
	}
	g, err := c.GRPC()
	if err != nil {
		return err
	}
	ps, err := g.GovV1.Proposals(c, &govv1.QueryProposalsRequest{ProposalStatus: govv1.ProposalStatus_PROPOSAL_STATUS_VOTING_PERIOD})
	if err != nil {
		return err
	}
	set("gov.voting_open", float64(len(ps.Proposals)))
	valoper, err := common.Valoper(c, nil)
	if err != nil {
		return nil
	}
	acc := accFromValoper(valoper)
	if acc == "" {
		return nil
	}
	unvoted, soonest := 0, -1.0
	for _, p := range ps.Proposals {
		if _, err := g.GovV1.Vote(c, &govv1.QueryVoteRequest{ProposalId: p.Id, Voter: acc}); err != nil {
			unvoted++
			if p.VotingEndTime != nil {
				h := time.Until(p.VotingEndTime.AsTime()).Hours()
				if soonest < 0 || h < soonest {
					soonest = h
				}
			}
		}
	}
	set("gov.unvoted", float64(unvoted))
	if soonest >= 0 {
		set("gov.unvoted_ends_in_h", round1(soonest))
	}
	return nil
}

func collectHost(c *toolkit.Context, r *Report, set func(string, any)) error {
	h, err := c.Host()
	if err != nil {
		return err
	}
	home := "/"
	if c.Profile != nil && c.Profile.Home != "" {
		home = c.Profile.Home
	}
	run := func(cmd string) string {
		out, code, err := h.Run(c, cmd)
		if err != nil || code != 0 {
			return ""
		}
		return strings.TrimSpace(out)
	}
	if fs := strings.Fields(lastLine(run("df -Pk " + common.ShellQ(home) + " 2>/dev/null"))); len(fs) >= 5 {
		set("host.disk_used_pct", pct(fs[4]))
		if kb, err := strconv.ParseFloat(fs[3], 64); err == nil {
			set("host.disk_free_gb", round1(kb/1024/1024))
		}
	}
	// inode columns differ: Linux "IUse%" is 5th, macOS "%iused" is 8th
	if ls := strings.Split(run("df -Pi "+common.ShellQ(home)+" 2>/dev/null"), "\n"); len(ls) >= 2 {
		head, row := strings.Fields(ls[0]), strings.Fields(ls[len(ls)-1])
		for i, h := range head {
			if strings.Contains(strings.ToLower(h), "iuse") && i < len(row) {
				set("host.inode_used_pct", pct(row[i]))
			}
		}
	}
	if out := run("free -b 2>/dev/null | awk '/^Mem:/{print $2, $7}'"); out != "" {
		if fs := strings.Fields(out); len(fs) == 2 {
			total, _ := strconv.ParseFloat(fs[0], 64)
			avail, _ := strconv.ParseFloat(fs[1], 64)
			if total > 0 {
				set("host.mem_used_pct", round1(100*(total-avail)/total))
			}
		}
	}
	if out := run("cat /proc/loadavg 2>/dev/null; nproc 2>/dev/null"); out != "" {
		ls := strings.Split(out, "\n")
		if len(ls) == 2 {
			load, _ := strconv.ParseFloat(strings.Fields(ls[0])[0], 64)
			n, _ := strconv.ParseFloat(strings.TrimSpace(ls[1]), 64)
			if n > 0 {
				set("host.load_per_cpu", round1(load/n))
			}
		}
	}
	switch run("timedatectl show -p NTPSynchronized --value 2>/dev/null") {
	case "yes":
		set("clock.ntp_synced", true)
	case "no":
		set("clock.ntp_synced", false)
	}
	return nil
}

func collectProcess(c *toolkit.Context, r *Report, set func(string, any)) error {
	if c.Profile == nil {
		return fmt.Errorf("no profile")
	}
	h, err := c.Host()
	if err != nil {
		return err
	}
	svc := c.Profile.Service
	switch svc.Type {
	case "docker":
		out, code, err := h.Run(c, "docker inspect -f 'status={{.State.Status}} restarts={{.RestartCount}} oom_killed={{.State.OOMKilled}} started={{.State.StartedAt}} exit={{.State.ExitCode}}' "+common.ShellQ(svc.Unit))
		if err != nil || code != 0 {
			return fmt.Errorf("docker inspect %s failed: %v", svc.Unit, err)
		}
		kv := fields(out)
		set("proc.running", kv["status"] == "running")
		set("proc.state", kv["status"])
		set("proc.restarts", num(kv["restarts"]))
		set("proc.oom_killed", kv["oom_killed"] == "true")
		set("proc.exit_code", num(kv["exit"]))
		if t, err := time.Parse(time.RFC3339Nano, kv["started"]); err == nil && t.Year() > 1 {
			set("proc.uptime_s", float64(int64(time.Since(t).Seconds())))
		}
	case "systemd", "":
		unit := svc.Unit
		if unit == "" {
			unit = c.Profile.Binary
		}
		if unit == "" {
			return fmt.Errorf("no service unit in profile")
		}
		out, code, err := h.Run(c, "systemctl show "+common.ShellQ(unit)+" -p ActiveState -p NRestarts -p Result -p ExecMainStatus 2>/dev/null")
		if err != nil || code != 0 || strings.TrimSpace(out) == "" {
			return fmt.Errorf("systemctl show %s unavailable", unit)
		}
		kv := map[string]string{}
		for _, l := range strings.Split(out, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
				kv[k] = v
			}
		}
		set("proc.running", kv["ActiveState"] == "active")
		set("proc.state", kv["ActiveState"])
		set("proc.restarts", num(kv["NRestarts"]))
		set("proc.oom_killed", kv["Result"] == "oom-kill")
		set("proc.exit_code", num(kv["ExecMainStatus"]))
	default:
		return fmt.Errorf("service type %q not inspected", svc.Type)
	}
	return nil
}

func collectLogs(c *toolkit.Context, r *Report, set func(string, any)) error {
	if c.Profile == nil {
		return fmt.Errorf("no profile")
	}
	h, err := c.Host()
	if err != nil {
		return err
	}
	since := r.Since
	svc := c.Profile.Service
	var cmd string
	switch svc.Type {
	case "docker":
		cmd = fmt.Sprintf("docker logs --since %s %s 2>&1 | tail -n 20000", since.Round(time.Second).String(), common.ShellQ(svc.Unit))
	case "systemd", "":
		unit := svc.Unit
		if unit == "" {
			unit = c.Profile.Binary
		}
		cmd = fmt.Sprintf("journalctl -u %s --since %s --no-pager -o cat 2>&1 | tail -n 20000", common.ShellQ(unit), common.ShellQ(fmt.Sprintf("%d min ago", int(since.Minutes()))))
	default:
		return fmt.Errorf("no log source for service type %q", svc.Type)
	}
	res, err := host.Exec(c, h, cmd, 8<<20)
	if err != nil && res.Output == "" {
		return err
	}
	sc := logscan.Scan(res.Output)
	set("logs.lines", float64(sc.Lines))
	for _, cat := range logscan.Categories {
		set("logs."+cat.Slug, float64(sc.Counts[cat.Slug]))
	}
	if sc.UpgradeName != "" {
		set("logs.upgrade_name", sc.UpgradeName)
	}
	set("_samples", sc.Samples)
	return nil
}

// readNodeFile reads <home>/<rel> from the host, falling back to the
// container when the node runs in docker and the home isn't on the host.
func readNodeFile(c *toolkit.Context, h host.Host, rel string) ([]byte, error) {
	p := c.Profile
	if p.Home != "" {
		if b, err := h.ReadFile(c, path.Join(p.Home, rel)); err == nil {
			return b, nil
		}
	}
	if p.Service.Type == "docker" && p.Service.Unit != "" {
		home := p.Signer.ContainerHome
		if home == "" {
			home = p.Home
		}
		if home != "" {
			out, code, err := h.Run(c, "docker exec "+common.ShellQ(p.Service.Unit)+" cat "+common.ShellQ(path.Join(home, rel)))
			if err == nil && code == 0 {
				return []byte(out), nil
			}
		}
	}
	return nil, fmt.Errorf("cannot read %s", rel)
}

func collectConfig(c *toolkit.Context, r *Report, set func(string, any)) error {
	if c.Profile == nil || (c.Profile.Home == "" && c.Profile.Service.Type != "docker") {
		return fmt.Errorf("no node home in profile")
	}
	h, err := c.Host()
	if err != nil {
		return err
	}
	var errs []string
	if raw, err := readNodeFile(c, h, "config/config.toml"); err != nil {
		errs = append(errs, err.Error())
	} else {
		var m map[string]any
		if err := toml.Unmarshal(raw, &m); err != nil {
			set("config.parse_error", true)
		} else {
			get := getter(m)
			set("config.db_backend", str(get("db_backend")))
			set("config.mempool_type", str(get("mempool", "type")))
			set("config.persistent_peers", float64(countList(get("p2p", "persistent_peers"))))
			set("config.seeds", float64(countList(get("p2p", "seeds"))))
			set("config.statesync_enable", get("statesync", "enable") == true)
			set("config.remote_signer", str(get("priv_validator_laddr")) != "")
			if v, ok := get("consensus", "double_sign_check_height").(int64); ok {
				set("config.double_sign_check_height", float64(v))
			}
			if d, err := time.ParseDuration(str(get("consensus", "timeout_commit"))); err == nil {
				set("config.timeout_commit_s", d.Seconds())
			}
			if v, ok := get("p2p", "max_num_inbound_peers").(int64); ok {
				set("config.max_inbound_peers", float64(v))
			}
			set("config.pex", get("p2p", "pex") != false)
			set("config.prometheus", get("instrumentation", "prometheus") == true)
		}
	}
	if raw, err := readNodeFile(c, h, "config/app.toml"); err != nil {
		errs = append(errs, err.Error())
	} else {
		var m map[string]any
		if err := toml.Unmarshal(raw, &m); err != nil {
			set("app.parse_error", true)
		} else {
			get := getter(m)
			mgp := str(get("minimum-gas-prices"))
			set("app.min_gas_prices", mgp)
			set("app.min_gas_prices_empty", strings.TrimSpace(mgp) == "")
			set("app.pruning", str(get("pruning")))
			set("app.app_db_backend", str(get("app-db-backend")))
			if v, ok := get("halt-height").(int64); ok {
				set("app.halt_height", float64(v))
			}
			if v, ok := get("mempool", "max-txs").(int64); ok {
				set("app.mempool_max_txs", float64(v))
			}
			set("app.api_enable", get("api", "enable") == true)
			set("app.grpc_enable", get("grpc", "enable") != false)
			if v, ok := get("state-sync", "snapshot-interval").(int64); ok {
				set("app.snapshot_interval", float64(v))
			}
			if r.Chain == "cosmos-evm" {
				set("app.json_rpc_enable", get("json-rpc", "enable") == true)
				set("app.json_rpc_address", str(get("json-rpc", "address")))
				if v, ok := get("evm", "evm-chain-id").(int64); ok {
					set("app.evm_chain_id", float64(v))
				}
			}
		}
	}
	if raw, err := readNodeFile(c, h, "data/priv_validator_state.json"); err == nil {
		var pvs struct {
			Height string `json:"height"`
			Round  int64  `json:"round"`
		}
		if json.Unmarshal(raw, &pvs) == nil {
			set("pvs.height", num(pvs.Height))
		}
	}
	if len(errs) == 2 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func collectEVM(c *toolkit.Context, r *Report, set func(string, any)) error {
	if c.Profile.Endpoints.EVM == "" {
		return fmt.Errorf("no evm endpoint in profile")
	}
	ev, err := c.EVM()
	if err != nil {
		set("evm.reachable", false)
		return err
	}
	bn, err := ev.BlockNumber(c)
	if err != nil {
		set("evm.reachable", false)
		return err
	}
	set("evm.reachable", true)
	set("evm.height", float64(bn))
	if id, err := ev.ChainID(c); err == nil {
		set("evm.chain_id", float64(id))
	}
	if s, err := ev.Syncing(c); err == nil {
		set("evm.syncing", s != nil)
	}
	return nil
}

// derive computes signals that need more than one collector.
func derive(r *Report) {
	s := r.Signals
	f := func(k string) (float64, bool) { v, ok := s[k].(float64); return v, ok }
	if age, ok := f("node.block_age_s"); ok {
		s["node.stalled"] = s["node.catching_up"] == false && age > 60
	}
	if a, ok := s["node.cons_addr_hex"].(string); ok {
		if b, ok := s["val.cons_addr_hex"].(string); ok && a != "" && b != "" {
			s["node.key_mismatch"] = a != b
		}
	}
	if h, ok := f("node.height"); ok {
		if eh, ok := f("evm.height"); ok {
			s["evm.drift"] = h - eh
		}
		if uh, ok := f("upgrade.height"); ok {
			s["upgrade.blocks_to_halt"] = uh - h
		}
		if ph, ok := f("pvs.height"); ok {
			s["pvs.ahead_by"] = ph - h
		}
		if hh, ok := f("app.halt_height"); ok && hh > 0 {
			s["app.blocks_to_halt_height"] = hh - h
		}
	}
	if a, ok := s["app.app_db_backend"].(string); ok && a != "" {
		if b, ok := s["config.db_backend"].(string); ok && b != "" {
			s["config.db_backend_mismatch"] = a != b
		}
	}
	if t, ok := s["config.mempool_type"].(string); ok {
		if n, ok := f("app.mempool_max_txs"); ok {
			// CometBFT's app mempool hands txs to the app's mempool, which
			// max-txs < 0 disables: the node would accept no txs.
			s["config.mempool_mismatch"] = t == "app" && n < 0
		}
	}
	if id, ok := f("evm.chain_id"); ok {
		if want, ok := f("app.evm_chain_id"); ok && want > 0 {
			s["evm.chain_id_mismatch"] = id != want
		}
	}
	if s["val.in_set"] == true || s["val.bonded"] == true {
		if s["profile.role"] == "validator" {
			s["val.active"] = true
		}
	}
}

// --- helpers ------------------------------------------------------------------

func getter(m map[string]any) func(path ...string) any {
	return func(path ...string) any {
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
}

func str(v any) string { s, _ := v.(string); return s }

func countList(v any) int {
	n := 0
	for _, p := range strings.Split(str(v), ",") {
		if strings.TrimSpace(p) != "" {
			n++
		}
	}
	return n
}

func fields(s string) map[string]string {
	kv := map[string]string{}
	for _, f := range strings.Fields(s) {
		if k, v, ok := strings.Cut(f, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

func num(s string) float64 { f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64); return f }

func pct(s string) float64 { return num(strings.TrimSuffix(s, "%")) }

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return ls[len(ls)-1]
}

func lessThan(a, b string) bool {
	x, ok1 := new(big.Int).SetString(strings.Split(a, ".")[0], 10)
	y, ok2 := new(big.Int).SetString(strings.Split(b, ".")[0], 10)
	return ok1 && ok2 && x.Cmp(y) < 0
}

// decFrac reads an SDK Dec as bytes ("500000000000000000" = 0.5) or a
// decimal string ("0.5").
func decFrac(b []byte) (float64, bool) {
	s := string(b)
	if strings.Contains(s, ".") {
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f / 1e18, true
}

func accFromValoper(v string) string { a, _ := keys.AccFromValoper(v); return a }

// sortedKeys returns map keys in order.
func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
