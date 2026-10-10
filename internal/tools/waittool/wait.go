// Package waittool implements wait.until: block until a node or chain
// condition holds, polling in-process (no model tokens spent while
// waiting) and reporting progress.
package waittool

import (
	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/tools/triage"

	"fmt"
	"strconv"
	"strings"
	"time"

	govv1 "cosmossdk.io/api/cosmos/gov/v1"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/val"
)

// Register adds wait.until.
func Register(r *toolkit.Registry) { r.Register(Until{}) }

// MinInterval floors the poll interval (tests lower it).
var MinInterval = time.Second

const (
	defaultTimeout = 30 * time.Minute
	maxTimeout     = 6 * time.Hour
)

// Until waits for a condition.
type Until struct{}

func (Until) Name() string { return "wait.until" }
func (Until) Desc() string {
	return "Wait until a condition holds, checking every interval without spending model tokens, with progress: synced (caught up, recent blocks) | height (value=N) | unjailable (jail period over) | bonded | signing (signed ≥ min_signed_pct of the last window blocks) | in-consensus (bonded + in the set + signing) | tx-committed (value=hash) | proposal-status (value=id:STATUS, e.g. 12:PASSED) | signal (value=node.triage expressions joined by &&, e.g. \"host.disk_used_pct < 85 && proc.running == true\"). Returns done=false with the last state on timeout."
}
func (Until) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"condition":      toolkit.Enum("what to wait for", "synced", "height", "unjailable", "bonded", "signing", "in-consensus", "tx-committed", "proposal-status", "signal"),
		"value":          toolkit.Str("height, tx hash, proposal id:STATUS, or signal expressions — for the conditions that take one"),
		"validator":      toolkit.Str("valoper address (default: profile)"),
		"window":         toolkit.Int("blocks to check for signing / in-consensus (default 20)"),
		"min_signed_pct": toolkit.Int("percent of window that must be signed (default 90)"),
		"interval":       toolkit.Int("seconds between checks (default 10)"),
		"timeout":        toolkit.Int("give up after this many seconds (default 1800, max 21600)"),
	}, "condition")
}
func (Until) Tier() toolkit.Tier { return toolkit.TierObserve }

// Timeout covers the wait plus slack for the last check.
func (Until) Timeout(a toolkit.Args) time.Duration { return timeout(a) + 2*time.Minute }

func timeout(a toolkit.Args) time.Duration {
	d := time.Duration(a.Int("timeout", 0)) * time.Second
	if d <= 0 {
		return defaultTimeout
	}
	return min(d, maxTimeout)
}

// state is one check's outcome.
type state struct {
	done   bool
	status string
	data   map[string]any
}

func (Until) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	cond := a.String("condition", "")
	check, err := checker(cond, a)
	if err != nil {
		return nil, err
	}
	if v := a.String("validator", ""); v != "" && cond == "signal" && c.Profile != nil {
		// val.* signals are about the profile's validator: point them at
		// the one asked for
		p := *c.Profile
		p.Metadata = map[string]string{}
		for k, x := range c.Profile.Metadata {
			p.Metadata[k] = x
		}
		p.Metadata["valoper"] = v
		sub := c.Derive(c)
		sub.Profile = &p
		c = sub
	}
	iv := max(time.Duration(a.Int("interval", 10))*time.Second, MinInterval)
	if v, ok := a["interval"].(float64); ok && v > 0 && v < 1 {
		iv = max(time.Duration(v*float64(time.Second)), MinInterval)
	}
	deadline := time.Now().Add(timeout(a))
	start := time.Now()
	sp := &syncProgress{}
	var last state
	for checks := 1; ; checks++ {
		st, err := check(c, sp)
		if err != nil {
			st = state{status: "check failed: " + err.Error()}
		}
		last = st
		elapsed := time.Since(start).Round(time.Second)
		if st.done {
			data := map[string]any{"done": true, "condition": cond, "elapsed_s": int(elapsed.Seconds()), "checks": checks}
			for k, v := range st.data {
				data[k] = v
			}
			return &toolkit.Result{Text: fmt.Sprintf("✓ %s after %s — %s", cond, elapsed, st.status), Data: data}, nil
		}
		progress(c, fmt.Sprintf("waiting for %s · %s · %s elapsed", cond, st.status, elapsed))
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		// never sleep past the deadline: the last check lands on it
		select {
		case <-c.Done():
			return nil, c.Err()
		case <-time.After(min(iv, left)):
		}
	}
	data := map[string]any{"done": false, "condition": cond, "last": last.status}
	for k, v := range last.data {
		data[k] = v
	}
	// an error, not a result: scripts get a failing exit code, and a
	// playbook mustn't carry on as if the condition held
	return nil, fmt.Errorf("%s not reached within %s — last: %s", cond, timeout(a), last.status)
}

func progress(c *toolkit.Context, s string) {
	if c.Progress != nil {
		c.Progress(s)
	} else if c.Out != nil {
		fmt.Fprintln(c.Out, s)
	}
}

// syncProgress estimates sync speed between checks.
type syncProgress struct {
	at     time.Time
	height int64
	btime  time.Time
}

func (sp *syncProgress) update(f *val.Facts) string {
	now := time.Now()
	behind := now.Sub(f.BlockTime).Round(time.Second)
	s := fmt.Sprintf("height %d, %s behind", f.Height, behind)
	if !sp.at.IsZero() && f.Height > sp.height {
		wall := now.Sub(sp.at).Seconds()
		rate := float64(f.Height-sp.height) / wall
		chainPerWall := f.BlockTime.Sub(sp.btime).Seconds() / wall
		s += fmt.Sprintf(", %.1f blocks/s", rate)
		if chainPerWall > 1.01 {
			eta := time.Duration(behind.Seconds() / (chainPerWall - 1) * float64(time.Second)).Round(time.Second)
			s += ", ETA ~" + eta.String()
		}
	} else if !sp.at.IsZero() && f.Height == sp.height {
		s += ", NOT advancing"
	}
	sp.at, sp.height, sp.btime = now, f.Height, f.BlockTime
	return s
}

type checkFn func(c *toolkit.Context, sp *syncProgress) (state, error)

func checker(cond string, a toolkit.Args) (checkFn, error) {
	window := a.Int("window", 20)
	pct := float64(a.Int("min_signed_pct", 90)) / 100
	switch cond {
	case "synced":
		return func(c *toolkit.Context, sp *syncProgress) (state, error) {
			f, err := val.Gather(c, a)
			if err != nil {
				// archive and RPC nodes have no validator: sync is
				// the node's own business
				return syncedFromNode(c, sp)
			}
			if f.CometErr != "" {
				return state{status: "node RPC unreachable: " + f.CometErr}, nil
			}
			if f.Synced() {
				return state{done: true, status: f.SyncState(), data: map[string]any{"height": f.Height}}, nil
			}
			return state{status: sp.update(f), data: map[string]any{"height": f.Height}}, nil
		}, nil
	case "height":
		target, err := strconv.ParseInt(a.String("value", ""), 10, 64)
		if err != nil || target <= 0 {
			return nil, fmt.Errorf("height needs value=<block height>")
		}
		return func(c *toolkit.Context, _ *syncProgress) (state, error) {
			cc, err := c.Comet()
			if err != nil {
				return state{}, err
			}
			st, err := cc.Status(c)
			if err != nil {
				return state{}, err
			}
			h := st.SyncInfo.LatestBlockHeight
			return state{done: h >= target, status: fmt.Sprintf("height %d of %d (%d to go)", h, target, max(target-h, 0)), data: map[string]any{"height": h}}, nil
		}, nil
	case "unjailable":
		return func(c *toolkit.Context, _ *syncProgress) (state, error) {
			f, err := val.Gather(c, a)
			if err != nil {
				return state{}, err
			}
			switch {
			case f.Tombstoned:
				return state{}, fmt.Errorf("tombstoned — it will never be unjailable")
			case !f.Jailed:
				return state{done: true, status: "not jailed"}, nil
			}
			now := f.BlockTime
			if now.IsZero() {
				now = time.Now()
			}
			if !now.Before(f.JailedUntil) {
				return state{done: true, status: "jail period over at " + f.JailedUntil.UTC().Format(time.RFC3339)}, nil
			}
			return state{status: fmt.Sprintf("jailed until %s (%s left)", f.JailedUntil.UTC().Format(time.RFC3339), f.JailedUntil.Sub(now).Round(time.Second))}, nil
		}, nil
	case "bonded":
		return func(c *toolkit.Context, _ *syncProgress) (state, error) {
			f, err := val.Gather(c, a)
			if err != nil {
				return state{}, err
			}
			ok := f.Status == "BONDED" && !f.Jailed
			return state{done: ok, status: fmt.Sprintf("status %s, jailed=%v", f.Status, f.Jailed)}, nil
		}, nil
	case "signing", "in-consensus":
		return func(c *toolkit.Context, _ *syncProgress) (state, error) {
			_, st, err := val.Consensus(c, a, window)
			if err != nil {
				return state{}, err
			}
			signing := st.Checked >= window && float64(st.Signed) >= pct*float64(st.Checked)
			ok := signing
			if cond == "in-consensus" {
				ok = signing && st.Bonded && !st.Jailed && st.Power > 0
			}
			return state{done: ok, status: fmt.Sprintf("signed %d/%d recent blocks, bonded=%v, power=%d", st.Signed, st.Checked, st.Bonded && !st.Jailed, st.Power),
				data: map[string]any{"signed": st.Signed, "checked": st.Checked, "voting_power": st.Power}}, nil
		}, nil
	case "tx-committed":
		hash := strings.ToUpper(strings.TrimPrefix(a.String("value", ""), "0x"))
		if hash == "" {
			return nil, fmt.Errorf("tx-committed needs value=<tx hash>")
		}
		return func(c *toolkit.Context, _ *syncProgress) (state, error) {
			tb, err := c.Tx()
			if err != nil {
				return state{}, err
			}
			res, err := tb.GetTx(c, hash)
			if err != nil || res.TxResponse == nil {
				return state{status: "not committed yet"}, nil
			}
			r := res.TxResponse
			st := state{done: true, status: fmt.Sprintf("committed at height %d with code %d", r.Height, r.Code), data: map[string]any{"height": r.Height, "code": r.Code}}
			if r.Code != 0 {
				st.status += " (FAILED: " + r.RawLog + ")"
			}
			return st, nil
		}, nil
	case "proposal-status":
		id, want, ok := strings.Cut(a.String("value", ""), ":")
		pid, err := strconv.ParseUint(id, 10, 64)
		if !ok || err != nil {
			return nil, fmt.Errorf("proposal-status needs value=<id>:<STATUS>, e.g. 12:PASSED")
		}
		want = strings.ToUpper(strings.TrimPrefix(strings.ToUpper(want), "PROPOSAL_STATUS_"))
		return func(c *toolkit.Context, _ *syncProgress) (state, error) {
			g, err := c.GRPC()
			if err != nil {
				return state{}, err
			}
			res, err := g.GovV1.Proposal(c, &govv1.QueryProposalRequest{ProposalId: pid})
			if err != nil {
				return state{}, err
			}
			got := strings.TrimPrefix(res.Proposal.Status.String(), "PROPOSAL_STATUS_")
			done := got == want || (want != "REJECTED" && want != "FAILED" && (got == "REJECTED" || got == "FAILED"))
			st := state{done: done, status: "proposal " + id + " is " + got, data: map[string]any{"status": got}}
			if done && got != want {
				st.status += " (it will not reach " + want + ")"
			}
			return st, nil
		}, nil
	}
	if cond == "signal" {
		return signalChecker(a.String("value", ""))
	}
	return nil, fmt.Errorf("unknown condition %q", cond)
}

// signalChecker waits for node.triage signal expressions to all hold,
// collecting only the sources they need.
func signalChecker(expr string) (func(*toolkit.Context, *syncProgress) (state, error), error) {
	var conds []kb.Cond
	var names []string
	for _, part := range strings.Split(expr, "&&") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		cd, err := kb.ParseCond(part)
		if err != nil {
			return nil, err
		}
		conds = append(conds, cd)
		names = append(names, cd.Signal)
	}
	if len(conds) == 0 {
		return nil, fmt.Errorf("signal needs value=<expr>[ && <expr>…], e.g. \"node.peers >= 3\"")
	}
	return func(c *toolkit.Context, _ *syncProgress) (state, error) {
		r := triage.CollectFor(c, 10*time.Minute, names)
		done := true
		var parts []string
		data := map[string]any{}
		for _, cd := range conds {
			ok := cd.Eval(r.Signals)
			done = done && ok
			v, has := r.Signals[cd.Signal]
			if !has {
				v = "unavailable"
			}
			data[cd.Signal] = v
			mark := "✗"
			if ok {
				mark = "✓"
			}
			parts = append(parts, fmt.Sprintf("%s %s=%v", mark, cd.Signal, v))
		}
		return state{done: done, status: strings.Join(parts, ", "), data: data}, nil
	}, nil
}

// syncedFromNode decides "synced" from CometBFT status alone.
func syncedFromNode(c *toolkit.Context, sp *syncProgress) (state, error) {
	cc, err := c.Comet()
	if err != nil {
		return state{}, err
	}
	st, err := cc.Status(c)
	if err != nil {
		return state{status: "node RPC unreachable: " + err.Error()}, nil
	}
	f := &val.Facts{Height: st.SyncInfo.LatestBlockHeight, BlockTime: st.SyncInfo.LatestBlockTime,
		CatchingUp: st.SyncInfo.CatchingUp, MaxBlockLag: 2 * time.Minute}
	if f.Synced() {
		return state{done: true, status: f.SyncState(), data: map[string]any{"height": f.Height}}, nil
	}
	return state{status: sp.update(f), data: map[string]any{"height": f.Height}}, nil
}
