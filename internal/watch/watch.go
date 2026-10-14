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

// Carder is a notifier that can edit what it posted (Telegram): an issue
// is one card that changes as the work goes on, not a message per step.
type Carder interface {
	Post(ctx context.Context, text string) (ref string, err error)
	Edit(ctx context.Context, ref, text string) error
	Reply(ctx context.Context, ref, text string) error
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
	// for the card: a plain reason and the raw one
	Title    string // headline (the case title)
	Plain    string // what it means, in one sentence
	Evidence string // the raw reason: what matched
}

// Watcher runs sweeps.
type Watcher struct {
	Mode     Mode
	Cooldown time.Duration
	Collect  func(ctx context.Context) []triage.NodeReport
	Profiles map[string]*config.Profile
	Run      Runner
	Notify   []Notifier
	Approver toolkit.Approver // remote approvals (fix mode); nil = none
	// Planned says why a node is under planned work (a restart or upgrade
	// cometcli is doing, a silence at its monitor); the watcher reports
	// its issues but leaves it alone. nil = never.
	Planned   func(ctx context.Context, p *config.Profile) string
	StatePath string
	Out       io.Writer
	Now       func() time.Time

	// Alerts, when set, are monitor alerts (cometduty, Alertmanager) that
	// trigger work at once instead of at the next sweep. CollectOne
	// triages one node for them (nil = Collect, filtered).
	Alerts     <-chan Alert
	CollectOne func(ctx context.Context, p *config.Profile) []triage.NodeReport

	state     state
	lastSweep time.Time
	swept     bool // a sweep ran in this process
}

type state struct {
	Active  map[string]time.Time `json:"active"`  // issues seen in the last sweep
	Handled map[string]time.Time `json:"handled"` // when each issue was last worked
	// Monitor maps an open monitor alert's key to the issue it raised when
	// local triage found nothing of its own; its resolve clears the issue.
	Monitor map[string]string `json:"monitor,omitempty"`
	// Paused are issues seen during planned work on their node: reported
	// once, not worked; handled afresh if still open when the work ends.
	Paused map[string]bool `json:"paused,omitempty"`
	// Cards are the open issues' messages, edited as the work goes on.
	Cards map[string]*card `json:"cards,omitempty"`
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
	if w.state.Monitor == nil {
		w.state.Monitor = map[string]string{}
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
					Severity: h.Case.Severity, Text: h.Case.Title + " — " + strings.Join(h.Matched, ", "),
					Title: h.Case.Title, Plain: h.Case.Plain, Evidence: strings.Join(h.Matched, ", ")})
			}
			break // the top root case speaks for the node
		}
	}
	for _, f := range findings {
		if strings.Contains(f, "CHAIN HALT") || strings.Contains(f, "DOUBLE-SIGN") || strings.Contains(f, "cut off") {
			// numbers move between sweeps; the finding's shape is its identity
			out = append(out, Issue{Key: "fleet|" + digits.ReplaceAllString(f, "#"), Severity: "critical", Text: f,
				Title: fleetTitle(f), Evidence: f})
		}
	}
	return out
}

// Sweep runs one round: collect, find new and cleared issues, act.
func (w *Watcher) Sweep(ctx context.Context) {
	if w.state.Active == nil {
		w.load()
	}
	w.lastSweep = w.now()
	for ak, k := range w.state.Monitor {
		// a monitor alert whose resolve never came (monitor restarted, config
		// changed) is dropped after a day rather than kept open forever
		if at, ok := w.state.Active[k]; !ok || w.now().Sub(at) > 24*time.Hour {
			delete(w.state.Monitor, ak)
			delete(w.state.Active, k)
		}
	}
	nodes := w.Collect(ctx)
	issues := Issues(nodes, triage.FleetFindings(nodes))
	open, cleared := w.reconcile(ctx, issues, func(k string) bool { return !w.fromMonitor(k) })
	w.swept = true
	w.logf("sweep: %d nodes, %d open issues, %d resolved", len(nodes), open, cleared)
}

// reconcile compares issues with the active ones in scope: cleared ones are
// reported resolved, new ones worked (unless worked within the cooldown).
func (w *Watcher) reconcile(ctx context.Context, issues []Issue, inScope func(key string) bool) (open, cleared int) {
	now := w.now()
	current := map[string]Issue{}
	for _, is := range issues {
		current[is.Key] = is
	}
	var gone []string
	for k := range w.state.Active {
		if _, still := current[k]; !still && inScope(k) {
			gone = append(gone, k)
		}
	}
	sort.Strings(gone)
	if !w.swept && len(gone) > 0 {
		// issues saved by an earlier run, gone by now: one line, not a
		// burst of stale "resolved" messages
		for _, k := range gone {
			delete(w.state.Active, k)
		}
		for _, k := range gone {
			delete(w.state.Cards, k)
		}
		w.send(ctx, fmt.Sprintf("**Watcher started**\n%d issue(s) open when it last ran are gone", len(gone)))
		gone = nil
	}
	for _, k := range gone {
		delete(w.state.Active, k)
		if w.state.Paused[k] {
			delete(w.state.Paused, k)
			w.closeCard(ctx, k, "cleared during the planned work", false)
			continue // nothing was reported as broken
		}
		w.closeCard(ctx, k, "resolved", true)
	}
	keys := make([]string, 0, len(current))
	for k := range current {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	planned := map[string]string{}
	for _, k := range keys {
		is := current[k]
		if p := w.Profiles[is.Profile]; p != nil && w.Planned != nil {
			why, asked := planned[is.Profile]
			if !asked {
				why = w.Planned(ctx, p)
				planned[is.Profile] = why
			}
			if why != "" {
				w.state.Active[k] = now
				if !w.state.Paused[k] {
					if w.state.Paused == nil {
						w.state.Paused = map[string]bool{}
					}
					w.state.Paused[k] = true
					c := newCard(is, w.now())
					c.Status = "planned work under way — not acting (" + why + ")"
					w.showCard(ctx, k, c, false)
				}
				continue
			}
		}
		_, wasActive := w.state.Active[k]
		if w.state.Paused[k] {
			// the planned work is over and the issue is still there
			delete(w.state.Paused, k)
			delete(w.state.Handled, k)
			wasActive = false
		}
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
	return len(current), len(gone)
}

// localPath is a path on the watcher's machine (incident files): noise in
// a chat read elsewhere.
var localPath = regexp.MustCompile(` → (?:/|~/)\S+`)

// resolvedLine announces a cleared issue ("profile|case" or "fleet|…").
func resolvedLine(key, source string) string {
	where, what, _ := strings.Cut(key, "|")
	line := fmt.Sprintf("**%s: resolved** · `%s`", where, what)
	if source != "" {
		line += " (per " + source + ")"
	}
	return line
}

func (w *Watcher) fromMonitor(issueKey string) bool {
	for _, k := range w.state.Monitor {
		if k == issueKey {
			return true
		}
	}
	return false
}

// pending takes a and every alert queued behind it (they pile up while a
// long piece of work runs) and keeps each condition's latest state: an
// alert raised and resolved in the meantime is a resolve, not work.
func (w *Watcher) pending(a Alert) []Alert {
	batch := []Alert{a}
	for more := true; more; {
		select {
		case b := <-w.Alerts:
			batch = append(batch, b)
		default:
			more = false
		}
	}
	last := map[string]int{}
	raised := map[string]bool{}
	for i, b := range batch {
		last[b.Key] = i
		if !b.Resolved {
			raised[b.Key] = true
		}
	}
	var out []Alert
	for i, b := range batch {
		if last[b.Key] != i {
			continue
		}
		if b.Resolved && raised[b.Key] {
			w.logf("monitor alert from %s: %s raised and resolved while busy — skipped", b.Source, b.Kind)
			if _, known := w.state.Monitor[b.Key]; !known {
				continue
			}
		}
		out = append(out, b)
	}
	return out
}

// OnAlert works a monitor alert. One about a node triages that node now:
// what triage finds is worked like a sweep's issues, and when triage finds
// nothing the alert itself becomes the issue. One about no particular node
// (a stalled chain, a validator-set change) runs a fleet sweep. Info-level
// alerts are only logged.
func (w *Watcher) OnAlert(ctx context.Context, a Alert) {
	if w.state.Active == nil {
		w.load()
	}
	name := ProfileFor(a, w.Profiles)
	where := name
	if where == "" {
		where = orDash(a.Node)
	}
	w.logf("monitor alert from %s: %s on %s (%s)%s", a.Source, a.Kind, where, orDash(a.Severity), map[bool]string{true: " resolved"}[a.Resolved])
	if a.Resolved {
		if k, ok := w.state.Monitor[a.Key]; ok {
			delete(w.state.Monitor, a.Key)
			if _, open := w.state.Active[k]; open {
				delete(w.state.Active, k)
				w.closeCard(ctx, k, "resolved (per "+a.Source+")", true)
			}
			w.save()
		}
		if name != "" {
			w.triageOne(ctx, name, nil) // clears what triage no longer sees
		}
		return
	}
	if a.Severity == "info" {
		return
	}
	if name == "" {
		if w.now().Sub(w.lastSweep) >= 30*time.Second {
			w.Sweep(ctx)
		}
		return
	}
	w.triageOne(ctx, name, &a)
}

// triageOne triages one profile and reconciles its issues; with an alert,
// an empty triage turns the alert into the issue.
func (w *Watcher) triageOne(ctx context.Context, name string, a *Alert) {
	p := w.Profiles[name]
	var nodes []triage.NodeReport
	if w.CollectOne != nil {
		nodes = w.CollectOne(ctx, p)
	} else {
		for _, n := range w.Collect(ctx) {
			if n.Profile != nil && n.Profile.Name == name {
				nodes = append(nodes, n)
			}
		}
	}
	issues := Issues(nodes, nil)
	if a != nil && len(issues) == 0 {
		sev := "high"
		if a.Severity == "critical" {
			sev = "critical"
		}
		is := Issue{Key: name + "|monitor:" + a.Kind, Profile: name, Case: "monitor:" + a.Kind, Severity: sev,
			Text:     fmt.Sprintf("%s reports %s on %s; local triage found no matching problem", a.Source, a.Kind, name),
			Title:    fmt.Sprintf("%s alert: %s", a.Source, a.Kind),
			Plain:    fmt.Sprintf("%s raised %s, but cometcli's own checks find nothing wrong on the node right now.", a.Source, a.Kind),
			Evidence: fmt.Sprintf("%s %s (%s), key %s", a.Source, a.Kind, orDash(a.Severity), a.Key)}
		w.state.Monitor[a.Key] = is.Key
		issues = append(issues, is)
	}
	w.reconcile(ctx, issues, func(k string) bool {
		return strings.HasPrefix(k, name+"|") && !w.fromMonitor(k)
	})
}

func (w *Watcher) handle(ctx context.Context, is Issue) {
	c := w.state.Cards[is.Key]
	if c == nil {
		c = newCard(is, w.now())
	}
	p := w.Profiles[is.Profile]
	if w.Mode == ModeNotify || is.Profile == "" || w.Run == nil || p == nil {
		c.Status = "detected"
		w.showCard(ctx, is.Key, c, true)
		return
	}
	readOnly := w.Mode == ModeDiagnose || w.Approver == nil
	approve := w.Approver
	if readOnly {
		approve = toolkit.DenyApprover
	}
	c.Status = "investigating (read-only)"
	if !readOnly {
		c.Status = "working on it — approvals come as separate messages"
	}
	w.showCard(ctx, is.Key, c, false)
	report, err := w.Run(ctx, p, fmt.Sprintf("%s on %s detected by the watcher: %s", orDash(is.Case), is.Profile, is.Text), readOnly, approve)
	if err != nil {
		c.Status = fmt.Sprintf("the agent stopped: %v", err)
	} else {
		c.Status = "worked — report below"
		c.Report = clip(localPath.ReplaceAllString(strings.TrimSpace(report), ""), 3000)
	}
	w.showCard(ctx, is.Key, c, true)
}

// Loop sweeps every interval until ctx ends, working monitor alerts as
// they arrive; once runs a single sweep.
func (w *Watcher) Loop(ctx context.Context, interval time.Duration, once bool) {
	w.load()
	for {
		w.Sweep(ctx)
		if once {
			return
		}
		next := time.After(interval)
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case a := <-w.Alerts:
				for _, a := range w.pending(a) {
					w.OnAlert(ctx, a)
				}
			case <-next:
				break wait
			}
		}
	}
}

// KeyUseTimeout bounds a remote approval of a key use.
var KeyUseTimeout = 5 * time.Minute

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
		fmt.Fprintf(&b, "**Approval needed: %s**\n%s\n\n%s", node, tier, prompt)
		var keys []string
		for k := range detail {
			if !strings.HasPrefix(k, "_") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			b.WriteString("\n")
		}
		for _, k := range keys {
			fmt.Fprintf(&b, "\n> %s: %v", k, detail[k])
		}
		ctx := context.Background()
		if c != nil {
			ctx = c
		}
		if detail[toolkit.KeyUseMark] == true {
			// a key use is approved for this moment only
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, KeyUseTimeout)
			defer cancel()
			b.WriteString("\n\nExpires in " + KeyUseTimeout.String())
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

// --- issue cards ---

// card is an issue's message: headline, plain and raw reason, status and
// the agent's report, re-rendered and edited as the work goes on.
type card struct {
	Head   string            `json:"head"`
	Meta   string            `json:"meta"`
	Plain  string            `json:"plain,omitempty"`
	Raw    string            `json:"raw,omitempty"`
	Status string            `json:"status"`
	Report string            `json:"report,omitempty"`
	Opened time.Time         `json:"opened"`
	Refs   map[string]string `json:"refs,omitempty"` // notifier → its message
}

func newCard(is Issue, now time.Time) *card {
	where := is.Profile
	if where == "" {
		where = "fleet"
	}
	title := is.Title
	if title == "" {
		title = is.Text
	}
	plain := is.Plain
	if plain == "" && is.Title != "" && is.Text != is.Title {
		plain = is.Title + "."
	}
	raw := is.Evidence
	if is.Case != "" {
		raw = "`" + is.Case + "` · " + raw
	}
	return &card{Head: where + ": " + title, Meta: is.Severity, Plain: plain, Raw: strings.TrimSuffix(raw, " · "), Opened: now}
}

// text renders the card as cometcli Markdown; "> " lines are collapsed
// quotes in Telegram.
func (c *card) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**\n%s\n", c.Head, c.Meta)
	if c.Plain != "" && !strings.EqualFold(strings.TrimSuffix(c.Plain, "."), strings.TrimSuffix(c.Head, ".")) {
		fmt.Fprintf(&b, "\n%s\n", c.Plain)
	}
	if c.Raw != "" {
		fmt.Fprintf(&b, "> raw: %s\n", c.Raw)
	}
	fmt.Fprintf(&b, "\n**Status:** %s", c.Status)
	if c.Report != "" {
		b.WriteString("\n")
		for _, l := range strings.Split(c.Report, "\n") {
			b.WriteString("\n> " + l)
		}
	}
	return b.String()
}

func sinkName(n Notifier, i int) string {
	if nn, ok := n.(interface{ Name() string }); ok {
		return nn.Name()
	}
	return fmt.Sprint(i)
}

// showCard posts the card, or edits it where it was posted. Notifiers that
// can't edit get it when it opens and when it's final.
func (w *Watcher) showCard(ctx context.Context, key string, c *card, final bool) {
	if w.state.Cards == nil {
		w.state.Cards = map[string]*card{}
	}
	w.state.Cards[key] = c
	if c.Refs == nil {
		c.Refs = map[string]string{}
	}
	w.logf("%s · %s · %s", c.Head, c.Meta, c.Status)
	text := c.text()
	for i, n := range w.Notify {
		name := sinkName(n, i)
		if cd, ok := n.(Carder); ok {
			if ref := c.Refs[name]; ref != "" && cd.Edit(ctx, ref, text) == nil {
				continue
			}
			ref, err := cd.Post(ctx, text)
			if err != nil {
				w.logf("notify failed: %v", err)
				continue
			}
			c.Refs[name] = ref
			continue
		}
		if c.Refs[name] == "" || final {
			if err := n.Send(ctx, text); err != nil {
				w.logf("notify failed: %v", err)
			}
			c.Refs[name] = "sent"
		}
	}
	w.save()
}

// closeCard ends an issue's card with how it ended; announce also posts a
// short reply, since an edit makes no sound.
func (w *Watcher) closeCard(ctx context.Context, key, how string, announce bool) {
	c := w.state.Cards[key]
	delete(w.state.Cards, key)
	if c == nil {
		if announce {
			w.send(ctx, resolvedLine(key, ""))
		}
		return
	}
	line := fmt.Sprintf("%s after %s", how, w.now().Sub(c.Opened).Round(time.Second))
	c.Status = line
	w.logf("%s · %s", c.Head, line)
	text := c.text()
	for i, n := range w.Notify {
		name := sinkName(n, i)
		if cd, ok := n.(Carder); ok {
			if ref := c.Refs[name]; ref != "" {
				_ = cd.Edit(ctx, ref, text)
				if announce {
					_ = cd.Reply(ctx, ref, fmt.Sprintf("**%s**\n%s", c.Head, line))
				}
				continue
			}
		}
		if announce {
			_ = n.Send(ctx, fmt.Sprintf("**%s**\n%s", c.Head, line))
		}
	}
	w.save()
}

// fleetTitle is a fleet finding's first clause, as a headline.
func fleetTitle(f string) string {
	t, _, _ := strings.Cut(f, " — ")
	t, _, _ = strings.Cut(t, ": ")
	if len(t) > 80 {
		t = t[:80] + "…"
	}
	return t
}
