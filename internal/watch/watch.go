// Package watch is cometcli's autonomous watcher: it sweeps the fleet on
// an interval, turns newly appearing problems into incidents, has the
// agent work them (read-only, or fixing with remote approvals), and
// reports findings, results and recoveries to Slack, Discord or Telegram.
package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/triage"
)

// Mode is what the watcher does with a new incident.
type Mode string

const (
	ModeNotify   Mode = "notify"   // report the finding only
	ModeDiagnose Mode = "diagnose" // the agent investigates read-only and reports
	ModeFix      Mode = "fix"      // the agent may act; every approval goes to the operator
)

// ParseMode validates a mode name ("" = diagnose).
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case "", ModeDiagnose:
		return ModeDiagnose, nil
	case ModeNotify:
		return ModeNotify, nil
	case ModeFix:
		return ModeFix, nil
	}
	return "", fmt.Errorf("watch mode %q: want notify, diagnose or fix", s)
}

// Notifier delivers a message somewhere (Slack, Discord, Telegram, stdout).
type Notifier interface {
	Send(ctx context.Context, text string) error
}

// Runner has the agent work an incident on a profile and returns its
// report. readOnly forbids any change; approve gates the rest.
type Runner func(ctx context.Context, p *config.Profile, description string, readOnly bool, approve toolkit.Approver) (string, error)

// Issue is a problem seen in a sweep.
type Issue struct {
	Key      string // stable identity: profile|case, or fleet|<finding>
	Profile  string
	Case     string
	Severity string
	Text     string
}

// Watcher runs sweeps.
type Watcher struct {
	Mode      Mode
	Cooldown  time.Duration
	Collect   func(ctx context.Context) []triage.NodeReport
	Profiles  map[string]*config.Profile
	Run       Runner
	Notify    []Notifier
	Approver  toolkit.Approver // remote approvals (fix mode); nil = none
	StatePath string
	Out       io.Writer
	Now       func() time.Time

	state state
}

type state struct {
	Active  map[string]time.Time `json:"active"`  // issues seen in the last sweep
	Handled map[string]time.Time `json:"handled"` // when each issue was last worked
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) logf(format string, a ...any) {
	if w.Out != nil {
		fmt.Fprintf(w.Out, "%s %s\n", w.now().Format("15:04:05"), fmt.Sprintf(format, a...))
	}
}

func (w *Watcher) load() {
	w.state = state{Active: map[string]time.Time{}, Handled: map[string]time.Time{}}
	if w.StatePath == "" {
		return
	}
	if raw, err := os.ReadFile(w.StatePath); err == nil {
		_ = json.Unmarshal(raw, &w.state)
	}
	if w.state.Active == nil {
		w.state.Active = map[string]time.Time{}
	}
	if w.state.Handled == nil {
		w.state.Handled = map[string]time.Time{}
	}
}

func (w *Watcher) save() {
	if w.StatePath == "" {
		return
	}
	raw, _ := json.MarshalIndent(w.state, "", "  ")
	_ = os.WriteFile(w.StatePath, raw, 0o600)
}

func (w *Watcher) send(ctx context.Context, text string) {
	w.logf("%s", strings.ReplaceAll(text, "\n", "\n         "))
	for _, n := range w.Notify {
		if err := n.Send(ctx, text); err != nil {
			w.logf("notify failed: %v", err)
		}
	}
}

var digits = regexp.MustCompile(`[0-9]+`)

// Issues extracts the problems worth acting on from a sweep: each node's
// most severe root case (critical or high), and fleet findings that mean
// trouble (halts, double-sign risk, nodes cut off or down).
func Issues(nodes []triage.NodeReport, findings []string) []Issue {
	var out []Issue
	for _, n := range nodes {
		for _, h := range n.Hits {
			if h.Case.Kind == "advisory" || h.SymptomOf != "" {
				continue
			}
			if h.Case.Severity == "critical" || h.Case.Severity == "high" {
				out = append(out, Issue{Key: n.Profile.Name + "|" + h.Case.ID, Profile: n.Profile.Name, Case: h.Case.ID,
					Severity: h.Case.Severity, Text: h.Case.Title + " — " + strings.Join(h.Matched, ", ")})
			}
			break // the top root case speaks for the node
		}
	}
	for _, f := range findings {
		if strings.Contains(f, "CHAIN HALT") || strings.Contains(f, "DOUBLE-SIGN") || strings.Contains(f, "cut off") {
			// numbers move between sweeps; the finding's shape is its identity
			out = append(out, Issue{Key: "fleet|" + digits.ReplaceAllString(f, "#"), Severity: "critical", Text: f})
		}
	}
	return out
}

// Sweep runs one round: collect, find new and cleared issues, act.
func (w *Watcher) Sweep(ctx context.Context) {
	if w.state.Active == nil {
		w.load()
	}
	nodes := w.Collect(ctx)
	issues := Issues(nodes, triage.FleetFindings(nodes))
	now := w.now()
	current := map[string]Issue{}
	for _, is := range issues {
		current[is.Key] = is
	}
	// recovered: active last time, gone now
	var cleared []string
	for k := range w.state.Active {
		if _, still := current[k]; !still {
			cleared = append(cleared, k)
		}
	}
	sort.Strings(cleared)
	for _, k := range cleared {
		delete(w.state.Active, k)
		w.send(ctx, "✅ resolved: "+strings.Replace(k, "|", ": ", 1))
	}
	keys := make([]string, 0, len(current))
	for k := range current {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		is := current[k]
		_, wasActive := w.state.Active[k]
		w.state.Active[k] = now
		if wasActive {
			continue // already known and being (or been) handled
		}
		if last, ok := w.state.Handled[k]; ok && now.Sub(last) < w.Cooldown {
			continue // worked recently: don't loop on it
		}
		w.state.Handled[k] = now
		w.save()
		w.handle(ctx, is)
	}
	w.save()
	w.logf("sweep: %d nodes, %d open issues, %d resolved", len(nodes), len(current), len(cleared))
}

func (w *Watcher) handle(ctx context.Context, is Issue) {
	where := is.Profile
	if where == "" {
		where = "fleet"
	}
	w.send(ctx, fmt.Sprintf("🚨 %s: %s [%s]\n%s", where, orDash(is.Case), is.Severity, is.Text))
	if w.Mode == ModeNotify || is.Profile == "" || w.Run == nil {
		return
	}
	p := w.Profiles[is.Profile]
	if p == nil {
		return
	}
	readOnly := w.Mode == ModeDiagnose || w.Approver == nil
	approve := w.Approver
	if readOnly {
		approve = toolkit.DenyApprover
	}
	verb := "investigating (read-only)"
	if !readOnly {
		verb = "working it — approvals will be sent here"
	}
	w.send(ctx, fmt.Sprintf("🔎 %s: %s", is.Profile, verb))
	report, err := w.Run(ctx, p, fmt.Sprintf("%s on %s detected by the watcher: %s", orDash(is.Case), is.Profile, is.Text), readOnly, approve)
	if err != nil {
		w.send(ctx, fmt.Sprintf("⚠️ %s: the agent stopped: %v", is.Profile, err))
		return
	}
	w.send(ctx, fmt.Sprintf("📋 %s report:\n%s", is.Profile, clip(strings.TrimSpace(report), 3000)))
}

// Loop sweeps every interval until ctx ends; once runs a single sweep.
func (w *Watcher) Loop(ctx context.Context, interval time.Duration, once bool) {
	w.load()
	for {
		w.Sweep(ctx)
		if once {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// RemoteApprover adapts an asker (Telegram) into a toolkit.Approver:
// transactions are refused unless allowTx, and the question carries the
// tool's detail (command, diff, tx document).
func RemoteApprover(ask func(ctx context.Context, text string) (bool, string, error), allowTx bool) toolkit.Approver {
	return func(c *toolkit.Context, prompt string, tier toolkit.Tier, detail map[string]any) (bool, error) {
		if tier == toolkit.TierOnChain && !allowTx {
			return false, fmt.Errorf("transactions are disabled in watch mode (set watch.allow_tx to approve them remotely)")
		}
		node := "fleet"
		if c != nil && c.Profile != nil {
			node = c.Profile.Name
		}
		var b strings.Builder
		fmt.Fprintf(&b, "🔐 approval needed on %s [%s]\n%s", node, tier, prompt)
		var keys []string
		for k := range detail {
			if !strings.HasPrefix(k, "_") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "\n%s: %v", k, detail[k])
		}
		ctx := context.Background()
		if c != nil {
			ctx = c
		}
		ok, _, err := ask(ctx, b.String())
		if err != nil {
			return false, fmt.Errorf("remote approval failed: %w", err)
		}
		if !ok {
			return false, fmt.Errorf("denied by the operator (or no answer in time)")
		}
		return true, nil
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
