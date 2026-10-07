package val

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	"github.com/cometbft/cometbft/types"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/keys"
	"github.com/abhijitkrm/cometcli/internal/logscan"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

// Facts is what the chain and the node say about the validator right now.
type Facts struct {
	Valoper, Account, ConsAddr string
	ConsHex                    []byte

	Jailed, Tombstoned bool
	Status             string // BONDED, UNBONDING, UNBONDED
	JailedUntil        time.Time
	Missed             int64
	Window             int64
	DowntimeJail       time.Duration

	MinSelfDelegation string
	SelfDelegation    string
	FeeDenom          string
	FeeBalance        string

	Height      int64
	BlockTime   time.Time
	CatchingUp  bool
	CometErr    string
	MaxBlockLag time.Duration
}

// Synced reports whether the node is caught up: not catching up and its
// latest block is recent.
func (f *Facts) Synced() bool {
	return f.CometErr == "" && !f.CatchingUp && time.Since(f.BlockTime) <= f.MaxBlockLag
}

// SyncState renders the node's sync position.
func (f *Facts) SyncState() string {
	if f.CometErr != "" {
		return "node RPC unreachable: " + f.CometErr
	}
	lag := time.Since(f.BlockTime).Round(time.Second)
	switch {
	case f.Synced():
		return fmt.Sprintf("synced at height %d", f.Height)
	case f.CatchingUp:
		return fmt.Sprintf("catching up — height %d, %s behind", f.Height, lag)
	default:
		return fmt.Sprintf("stalled — height %d, last block %s ago", f.Height, lag)
	}
}

// Gather collects Facts. Unreachable pieces are left zero (and noted)
// rather than failing the whole check.
func Gather(c *toolkit.Context, a toolkit.Args) (*Facts, error) {
	valoper, err := common.Valoper(c, a)
	if err != nil {
		return nil, err
	}
	f := &Facts{Valoper: valoper, MaxBlockLag: time.Minute}
	f.Account, _ = keys.AccFromValoper(valoper)
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	vr, err := g.Staking.Validator(c, &stakingv1beta1.QueryValidatorRequest{ValidatorAddr: valoper})
	if err != nil {
		return nil, fmt.Errorf("validator %s: %w", valoper, err)
	}
	v := vr.Validator
	f.Jailed, f.Status, f.MinSelfDelegation = v.Jailed, bondStatus(v.Status), v.MinSelfDelegation
	if cons, err := common.ConsAddress(c, valoper); err == nil {
		f.ConsAddr = cons
		if si, err := g.Slashing.SigningInfo(c, &slashingv1beta1.QuerySigningInfoRequest{ConsAddress: cons}); err == nil && si.ValSigningInfo != nil {
			info := si.ValSigningInfo
			f.Tombstoned, f.Missed = info.Tombstoned, info.MissedBlocksCounter
			if info.JailedUntil != nil {
				f.JailedUntil = info.JailedUntil.AsTime()
			}
		}
		if hexAddr, err := keys.Bech32ToHex(cons); err == nil {
			f.ConsHex = mustHex(hexAddr)
		}
	}
	if p, err := g.Slashing.Params(c, &slashingv1beta1.QueryParamsRequest{}); err == nil && p.Params != nil {
		f.Window = p.Params.SignedBlocksWindow
		if p.Params.DowntimeJailDuration != nil {
			f.DowntimeJail = p.Params.DowntimeJailDuration.AsDuration()
		}
	}
	if f.Account != "" {
		if d, err := g.Staking.Delegation(c, &stakingv1beta1.QueryDelegationRequest{DelegatorAddr: f.Account, ValidatorAddr: valoper}); err == nil && d.DelegationResponse != nil && d.DelegationResponse.Balance != nil {
			f.SelfDelegation = d.DelegationResponse.Balance.Amount
		}
		f.FeeDenom = c.Profile.Metadata["fee_denom"]
		if f.FeeDenom != "" {
			if b, err := g.Bank.Balance(c, &bankv1beta1.QueryBalanceRequest{Address: f.Account, Denom: f.FeeDenom}); err == nil && b.Balance != nil {
				f.FeeBalance = b.Balance.Amount
			}
		}
	}
	if cc, err := c.Comet(); err != nil {
		f.CometErr = err.Error()
	} else if st, err := cc.Status(c); err != nil {
		f.CometErr = err.Error()
	} else {
		f.Height, f.BlockTime, f.CatchingUp = st.SyncInfo.LatestBlockHeight, st.SyncInfo.LatestBlockTime, st.SyncInfo.CatchingUp
	}
	return f, nil
}

func mustHex(s string) []byte {
	var b []byte
	_, _ = fmt.Sscanf(s, "0x%x", &b)
	return b
}

// Blocker is something that stops an unjail from working, with its fix.
type Blocker struct {
	Issue, Fix string
	Hard       bool // the chain would reject the unjail
}

// UnjailBlockers lists what stands between the validator and a
// successful unjail that sticks.
func (f *Facts) UnjailBlockers(c *toolkit.Context) []Blocker {
	var out []Blocker
	switch {
	case f.Tombstoned:
		return []Blocker{{Hard: true, Issue: "tombstoned for double-signing — it can never be unjailed",
			Fix: "do NOT restart this signer; investigate the double-sign (two signers with one key?); a new validator needs a new consensus key"}}
	case !f.Jailed:
		return []Blocker{{Hard: true, Issue: "not jailed", Fix: "nothing to unjail — check val.consensus instead"}}
	}
	chainNow := f.BlockTime
	if chainNow.IsZero() {
		chainNow = time.Now()
	}
	if !f.JailedUntil.IsZero() && chainNow.Before(f.JailedUntil) {
		out = append(out, Blocker{Hard: true, Issue: fmt.Sprintf("jail period runs until %s (%s from the latest block)", f.JailedUntil.UTC().Format(time.RFC3339), f.JailedUntil.Sub(chainNow).Round(time.Second)),
			Fix: "wait.until condition=unjailable"})
	}
	if !f.Synced() {
		out = append(out, Blocker{Hard: true, Issue: "node not synced: " + f.SyncState(),
			Fix: "wait.until condition=synced (and fix why it fell behind) — an unjailed validator that can't sign gets jailed again"})
	}
	if lessThan(f.SelfDelegation, f.MinSelfDelegation) {
		out = append(out, Blocker{Hard: true, Issue: fmt.Sprintf("self-delegation %s is below min_self_delegation %s", f.SelfDelegation, f.MinSelfDelegation),
			Fix: "self-delegate the difference with tx.delegate before unjailing"})
	}
	if f.FeeDenom != "" && f.FeeBalance != "" {
		need := feeEstimate(c)
		if need != nil && lessThan(f.FeeBalance, need.String()) {
			out = append(out, Blocker{Hard: true, Issue: fmt.Sprintf("operator account has %s%s, a tx needs about %s%s in fees", f.FeeBalance, f.FeeDenom, need, f.FeeDenom),
				Fix: "fund the operator account (" + f.Account + ")"})
		}
	}
	return out
}

// feeEstimate is a conservative fee for one simple tx at the profile's
// gas price (300k gas).
func feeEstimate(c *toolkit.Context) *big.Int {
	price, ok := new(big.Rat).SetString(c.Profile.Metadata["gas_price"])
	if !ok {
		return nil
	}
	fee := new(big.Rat).Mul(price, big.NewRat(300_000, 1))
	return new(big.Int).Quo(fee.Num(), fee.Denom())
}

func lessThan(a, b string) bool {
	x, ok1 := new(big.Int).SetString(a, 10)
	y, ok2 := new(big.Int).SetString(b, 10)
	return ok1 && ok2 && x.Cmp(y) < 0
}

// SignedInWindow counts our signatures in the last n commits.
func SignedInWindow(c *toolkit.Context, consHex []byte, n int64) (signed, checked int64, err error) {
	cc, err := c.Comet()
	if err != nil {
		return 0, 0, err
	}
	st, err := cc.Status(c)
	if err != nil {
		return 0, 0, err
	}
	top := st.SyncInfo.LatestBlockHeight
	for h := top; h > top-n && h > 0; h-- {
		hh := h
		cm, err := cc.RPC.Commit(c, &hh)
		if err != nil {
			return signed, checked, err
		}
		checked++
		for _, s := range cm.Commit.Signatures {
			if s.BlockIDFlag == types.BlockIDFlagCommit && bytes.Equal(s.ValidatorAddress, consHex) {
				signed++
				break
			}
		}
	}
	return signed, checked, nil
}

// InValidatorSet reports our voting power in the CometBFT set (0 = absent).
func InValidatorSet(c *toolkit.Context, consHex []byte) (int64, error) {
	cc, err := c.Comet()
	if err != nil {
		return 0, err
	}
	vals, err := cc.AllValidators(c, nil)
	if err != nil {
		return 0, err
	}
	for _, v := range vals {
		if bytes.Equal(v.Address, consHex) {
			return v.VotingPower, nil
		}
	}
	return 0, nil
}

// --- log forensics -----------------------------------------------------------

var (
	jailLineRe = regexp.MustCompile(`(?i)jail|liveness fault|double.?sign|tombston|slashing`)
)

// Forensics is what the node's own logs and process state show around
// the jailing.
type Forensics struct {
	JailLines  []string
	Causes     map[string][]string // cause → sample lines
	Counts     map[string]int
	Process    string // container/service state (restarts, OOM kills)
	// Lifecycle is the supervisor's record of stops, kills, deaths and
	// starts since the window opened (docker events) — who stopped it.
	Lifecycle []string
	// Gaps are silences in the node's log (it logs every block): when it
	// was down, and the last thing it said before.
	Gaps []string
	Disk       string
	LogsSource string
	Err        string
}

// Investigate reads the node's logs over the window ending at the jail
// (estimated from jailed_until − downtime_jail_duration) plus process
// and disk state.
func Investigate(c *toolkit.Context, f *Facts) *Forensics {
	fo := &Forensics{Causes: map[string][]string{}, Counts: map[string]int{}}
	h, err := c.Host()
	if err != nil {
		fo.Err = err.Error()
		return fo
	}
	since, until := time.Now().Add(-6*time.Hour), time.Now()
	if !f.JailedUntil.IsZero() && f.DowntimeJail > 0 {
		jailedAt := f.JailedUntil.Add(-f.DowntimeJail)
		since, until = jailedAt.Add(-30*time.Minute), jailedAt.Add(5*time.Minute)
	}
	svc := c.Profile.Service
	var logCmd, procCmd string
	switch svc.Type {
	case "docker":
		logCmd = fmt.Sprintf("docker logs -t --since %s --until %s %s 2>&1", since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339), common.ShellQ(svc.Unit))
		procCmd = fmt.Sprintf("docker inspect -f 'status={{.State.Status}} restarts={{.RestartCount}} oom_killed={{.State.OOMKilled}} started={{.State.StartedAt}}' %s", common.ShellQ(svc.Unit))
		fo.LogsSource = "docker logs " + svc.Unit
	case "systemd", "":
		unit := svc.Unit
		if unit == "" {
			unit = c.Profile.Binary
		}
		logCmd = fmt.Sprintf("journalctl -u %s --since %s --until %s --no-pager -o cat 2>&1", common.ShellQ(unit),
			common.ShellQ(since.Local().Format("2006-01-02 15:04:05")), common.ShellQ(until.Local().Format("2006-01-02 15:04:05")))
		procCmd = fmt.Sprintf("systemctl show %s -p ActiveState -p NRestarts -p ExecMainStartTimestamp -p Result 2>/dev/null | tr '\\n' ' '", common.ShellQ(unit))
		fo.LogsSource = "journalctl -u " + unit
	}
	ctx, cancel := context.WithTimeout(c, 30*time.Second)
	defer cancel()
	if logCmd != "" {
		res, err := host.Exec(ctx, h, logCmd, 4<<20)
		if err != nil {
			fo.Err = err.Error()
		}
		var prevT time.Time
		var prevLine string
		for _, line := range strings.Split(res.Output, "\n") {
			if line = logscan.Clean(line); line == "" {
				continue
			}
			if ts, rest, ok := strings.Cut(line, " "); ok && svc.Type == "docker" {
				if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
					line = rest
					if !prevT.IsZero() && t.Sub(prevT) > time.Minute && len(fo.Gaps) < 5 {
						fo.Gaps = append(fo.Gaps, fmt.Sprintf("silent %s → %s (%s); last line before: %s",
							prevT.UTC().Format("15:04:05"), t.UTC().Format("15:04:05"), t.Sub(prevT).Round(time.Second), clipLine(prevLine)))
					}
					prevT, prevLine = t, line
				}
			}
			if jailLineRe.MatchString(line) && len(fo.JailLines) < 6 {
				fo.JailLines = append(fo.JailLines, clipLine(line))
			}
			for _, cs := range logscan.Classify(line) {
				if cs.Slug != "jail" {
					fo.Counts[cs.Name]++
					if len(fo.Causes[cs.Name]) < 3 {
						fo.Causes[cs.Name] = append(fo.Causes[cs.Name], clipLine(line))
					}
				}
			}
		}
	}
	if procCmd != "" {
		if res, err := host.Exec(ctx, h, procCmd, 4096); err == nil && res.Code == 0 {
			fo.Process = strings.TrimSpace(res.Output)
		}
	}
	if svc.Type == "docker" {
		fo.Lifecycle = DockerLifecycle(ctx, h, svc.Unit, since)
	}
	if c.Profile.Home != "" {
		if res, err := host.Exec(ctx, h, "df -P "+common.ShellQ(c.Profile.Home)+" 2>/dev/null | tail -1", 4096); err == nil {
			if fs := strings.Fields(res.Output); len(fs) >= 5 {
				fo.Disk = fs[4] + " used on " + fs[len(fs)-1]
			}
		}
	}
	return fo
}

// stoppedBy summarizes an external stop (a SIGTERM/SIGKILL sent to the
// container before it died), or "" if it died on its own.
func stoppedBy(events []string) string {
	for i, e := range events {
		if strings.Contains(e, " kill ") || strings.HasSuffix(e, " stop") {
			for _, d := range events[i:] {
				if strings.Contains(d, " die") {
					return e + " → " + d[strings.Index(d, " ")+1:]
				}
			}
		}
	}
	return ""
}

// DockerLifecycle lists a container's kill/stop/die/oom/start/restart
// events since t, oldest first ("05:33:01 kill signal=15").
func DockerLifecycle(ctx context.Context, h host.Host, container string, since time.Time) []string {
	cmd := fmt.Sprintf("docker events --since %d --until %d --filter container=%s --format '{{.Time}} {{.Action}} exit={{index .Actor.Attributes \"exitCode\"}} signal={{index .Actor.Attributes \"signal\"}}'",
		since.Unix(), time.Now().Unix(), common.ShellQ(container))
	res, err := host.Exec(ctx, h, cmd, 64<<10)
	if err != nil || res.Code != 0 {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(res.Output), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch f[1] {
		case "kill", "stop", "die", "oom", "start", "restart", "pause", "unpause":
		default:
			continue
		}
		var ts int64
		fmt.Sscan(f[0], &ts)
		e := time.Unix(ts, 0).UTC().Format("15:04:05") + " " + f[1]
		for _, kv := range f[2:] {
			if !strings.HasSuffix(kv, "=") && !strings.HasSuffix(kv, "=<no value>") {
				e += " " + kv
			}
		}
		out = append(out, e)
	}
	if len(out) > 20 {
		out = out[len(out)-20:]
	}
	return out
}

func clipLine(s string) string {
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}

// --- val.jail-check ------------------------------------------------------------

type jailCheckTool struct{}

func (jailCheckTool) Name() string { return "val.jail-check" }
func (jailCheckTool) Desc() string {
	return "Why is the validator jailed, can it be unjailed now, and what must be fixed first: jail reason and root-cause evidence from the node's logs, jail period, sync state, self-delegation, fee balance, process restarts/OOM, disk"
}
func (jailCheckTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"validator": toolkit.Str("valoper address (default: profile)"),
	})
}
func (jailCheckTool) Tier() toolkit.Tier { return toolkit.TierDiagnose }

func (jailCheckTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	f, err := Gather(c, a)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	data := map[string]any{"valoper": f.Valoper, "jailed": f.Jailed, "tombstoned": f.Tombstoned, "status": f.Status,
		"sync": f.SyncState(), "missed_blocks": f.Missed}
	fmt.Fprintf(&b, "validator: %s (%s)\n", f.Valoper, f.Status)
	switch {
	case f.Tombstoned:
		b.WriteString("state:     TOMBSTONED — jailed permanently for double-signing\n")
	case f.Jailed:
		fmt.Fprintf(&b, "state:     jailed until %s\n", f.JailedUntil.UTC().Format(time.RFC3339))
		reason := "downtime (missed too many blocks in the signing window)"
		data["reason"] = "downtime"
		b.WriteString("reason:    " + reason + "\n")
		if f.DowntimeJail > 0 && !f.JailedUntil.IsZero() {
			at := f.JailedUntil.Add(-f.DowntimeJail)
			fmt.Fprintf(&b, "jailed at: ~%s\n", at.UTC().Format(time.RFC3339))
			data["jailed_at"] = at.UTC().Format(time.RFC3339)
		}
	default:
		b.WriteString("state:     not jailed\n")
	}
	fmt.Fprintf(&b, "node:      %s\n", f.SyncState())
	if f.SelfDelegation != "" {
		fmt.Fprintf(&b, "self-delegation: %s (min %s)\n", f.SelfDelegation, f.MinSelfDelegation)
	}
	if f.FeeBalance != "" {
		fmt.Fprintf(&b, "fee balance: %s%s\n", f.FeeBalance, f.FeeDenom)
	}

	if f.Jailed || f.Tombstoned {
		fo := Investigate(c, f)
		data["process"], data["disk"] = fo.Process, fo.Disk
		if len(fo.Lifecycle) > 0 {
			data["lifecycle"] = fo.Lifecycle
		}
		if len(fo.Gaps) > 0 {
			data["log_gaps"] = fo.Gaps
		}
		fmt.Fprintf(&b, "\nevidence (%s around the jailing):\n", orElse(fo.LogsSource, "logs"))
		if fo.Err != "" {
			fmt.Fprintf(&b, "  logs unavailable: %s\n", fo.Err)
		}
		if len(fo.JailLines) > 0 {
			b.WriteString("  jail event:\n")
			for _, l := range fo.JailLines {
				fmt.Fprintf(&b, "    %s\n", l)
				if regexp.MustCompile(`(?i)double.?sign`).MatchString(l) {
					data["reason"] = "double-sign"
				}
			}
		}
		var names []string
		for n := range fo.Counts {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool { return fo.Counts[names[i]] > fo.Counts[names[j]] })
		if len(names) == 0 {
			if stop := stoppedBy(fo.Lifecycle); stop == "" && len(fo.Gaps) > 0 {
				b.WriteString("  no error patterns in that window — the log goes silent with no crash message (see log gap): the process was stopped or killed from outside (operator, deploy, host reboot, OOM/SIGKILL) or the host froze\n")
			} else if stop != "" {
				fmt.Fprintf(&b, "  no error patterns in that window — the container was STOPPED (%s), not crashed: find who/what stopped it (operator, deploy, host reboot, autoheal)\n", stop)
			} else {
				b.WriteString("  no error patterns in that window — the node may have been down entirely (check the process state below)\n")
			}
		}
		var likely []string
		for _, n := range names {
			fmt.Fprintf(&b, "  %s ×%d:\n", n, fo.Counts[n])
			for _, l := range fo.Causes[n] {
				fmt.Fprintf(&b, "    %s\n", l)
			}
			likely = append(likely, n)
		}
		data["likely_causes"] = likely
		for _, g := range fo.Gaps {
			fmt.Fprintf(&b, "  log gap (node down or hung): %s\n", g)
		}
		if len(fo.Lifecycle) > 0 {
			fmt.Fprintf(&b, "  lifecycle (docker events, UTC): %s\n", strings.Join(fo.Lifecycle, " → "))
		}
		if fo.Process != "" {
			fmt.Fprintf(&b, "  process: %s\n", fo.Process)
		}
		if fo.Disk != "" {
			fmt.Fprintf(&b, "  disk: %s\n", fo.Disk)
		}
	}

	blockers := f.UnjailBlockers(c)
	can := f.Jailed && !f.Tombstoned
	var bl []map[string]any
	if len(blockers) > 0 {
		b.WriteString("\nbefore unjailing:\n")
	}
	for _, x := range blockers {
		if x.Hard {
			can = false
		}
		fmt.Fprintf(&b, "  ✗ %s\n    → %s\n", x.Issue, x.Fix)
		bl = append(bl, map[string]any{"issue": x.Issue, "fix": x.Fix, "hard": x.Hard})
	}
	if can {
		b.WriteString("\n✓ ready to unjail (val.unjail) — fix the root cause above first or it will be jailed again\n")
	}
	data["can_unjail_now"], data["blockers"] = can, bl
	return &toolkit.Result{Text: strings.TrimRight(b.String(), "\n"), Data: data}, nil
}

func orElse(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// --- val.consensus -----------------------------------------------------------

type consensusTool struct{}

func (consensusTool) Name() string { return "val.consensus" }
func (consensusTool) Desc() string {
	return "Is the validator taking part in consensus right now: bonded and unjailed, in the CometBFT validator set with voting power, and its signature present in the last N commits"
}
func (consensusTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"validator": toolkit.Str("valoper address (default: profile)"),
		"window":    toolkit.Int("recent blocks to check for our signature (default 20)"),
	})
}
func (consensusTool) Tier() toolkit.Tier { return toolkit.TierObserve }

// ConsensusState is the verdict of val.consensus.
type ConsensusState struct {
	Bonded, Jailed  bool
	Power           int64
	Signed, Checked int64
	Synced          bool
}

// In reports whether the validator is participating.
func (s ConsensusState) In(minPct float64) bool {
	return s.Bonded && !s.Jailed && s.Power > 0 && s.Checked > 0 && float64(s.Signed) >= minPct*float64(s.Checked)
}

// Consensus measures participation over a window of recent blocks.
func Consensus(c *toolkit.Context, a toolkit.Args, window int64) (*Facts, ConsensusState, error) {
	f, err := Gather(c, a)
	if err != nil {
		return nil, ConsensusState{}, err
	}
	st := ConsensusState{Bonded: f.Status == "BONDED", Jailed: f.Jailed, Synced: f.Synced()}
	if len(f.ConsHex) > 0 {
		st.Power, _ = InValidatorSet(c, f.ConsHex)
		st.Signed, st.Checked, _ = SignedInWindow(c, f.ConsHex, window)
	}
	return f, st, nil
}

func (consensusTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	window := a.Int("window", 20)
	f, st, err := Consensus(c, a, window)
	if err != nil {
		return nil, err
	}
	in := st.In(0.9)
	mark := func(ok bool) string {
		if ok {
			return "✓"
		}
		return "✗"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s bonded and unjailed (%s, jailed=%v)\n", mark(st.Bonded && !st.Jailed), f.Status, st.Jailed)
	fmt.Fprintf(&b, "%s in the validator set (voting power %d)\n", mark(st.Power > 0), st.Power)
	fmt.Fprintf(&b, "%s signed %d of the last %d blocks\n", mark(st.Checked > 0 && float64(st.Signed) >= 0.9*float64(st.Checked)), st.Signed, st.Checked)
	fmt.Fprintf(&b, "%s node %s\n", mark(st.Synced), f.SyncState())
	if in {
		b.WriteString("→ participating in consensus")
	} else {
		b.WriteString("→ NOT participating in consensus")
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{
		"in_consensus": in, "bonded": st.Bonded, "jailed": st.Jailed, "voting_power": st.Power,
		"signed": st.Signed, "checked": st.Checked, "synced": st.Synced, "height": f.Height,
	}}, nil
}
