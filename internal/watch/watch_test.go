package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/triage"
)

type memNotify struct{ msgs []string }

func (m *memNotify) Send(_ context.Context, t string) error { m.msgs = append(m.msgs, t); return nil }

func jailedNode(jailed bool) []triage.NodeReport {
	p := &config.Profile{Name: "val01", ChainID: "c-1", Role: "validator"}
	r := &triage.Report{Signals: kb.Signals{"node.reachable": true, "node.height": 10.0, "node.block_age_s": 1.0}}
	var hits []kb.Hit
	if jailed {
		hits = []kb.Hit{{Case: &kb.Case{ID: "val-jailed-downtime", Title: "Validator jailed", Severity: "critical", Kind: "incident"}, Matched: []string{"val.jailed == true"}}}
	}
	return []triage.NodeReport{{Profile: p, Report: r, Hits: hits}}
}

func TestSweepLifecycle(t *testing.T) {
	jailed := true
	now := time.Now()
	var runs []bool
	n := &memNotify{}
	w := &Watcher{
		Mode: ModeDiagnose, Cooldown: time.Hour, StatePath: filepath.Join(t.TempDir(), "state.json"),
		Profiles: map[string]*config.Profile{"val01": {Name: "val01"}},
		Collect:  func(context.Context) []triage.NodeReport { return jailedNode(jailed) },
		Run: func(_ context.Context, p *config.Profile, desc string, readOnly bool, approve toolkit.Approver) (string, error) {
			runs = append(runs, readOnly)
			if ok, _ := approve(nil, "restart", toolkit.TierLocalChange, nil); ok {
				t.Error("diagnose mode approved a change")
			}
			return "cause: container stopped", nil
		},
		Notify: []Notifier{n}, Now: func() time.Time { return now },
	}
	ctx := context.Background()
	w.Sweep(ctx) // new incident: report + read-only investigation
	all := strings.Join(n.msgs, "\n")
	if len(runs) != 1 || !runs[0] || !strings.Contains(all, "**val01: Validator jailed**\ncritical") || !strings.Contains(all, "> raw: `val-jailed-downtime` · val.jailed == true") ||
		!strings.Contains(all, "**Status:** worked — report below\n\n> cause: container stopped") {
		t.Fatalf("first sweep: runs=%v\n%s", runs, all)
	}
	w.Sweep(ctx) // still jailed: nothing new
	if len(runs) != 1 {
		t.Fatal("worked the same incident twice")
	}
	jailed = false
	w.Sweep(ctx) // cleared
	if !strings.Contains(n.msgs[len(n.msgs)-1], "**val01: Validator jailed**\nresolved after") {
		t.Fatalf("no recovery message: %v", n.msgs)
	}
	jailed = true
	w.Sweep(ctx) // back within the cooldown: not worked again
	if len(runs) != 1 {
		t.Fatal("cooldown ignored")
	}
	now = now.Add(2 * time.Hour)
	jailed = false
	w.Sweep(ctx)
	jailed = true
	w.Sweep(ctx) // after the cooldown: worked again
	if len(runs) != 2 {
		t.Fatalf("runs after cooldown = %d", len(runs))
	}
	// state survives a restart
	w2 := &Watcher{StatePath: w.StatePath, Mode: ModeNotify, Collect: w.Collect, Notify: []Notifier{&memNotify{}}, Cooldown: time.Hour, Now: w.Now}
	w2.Sweep(ctx)
	if len(w2.Notify[0].(*memNotify).msgs) != 0 {
		t.Fatal("restart re-announced an open incident")
	}
}

func TestIssuesFromFleetFindings(t *testing.T) {
	a := Issues(nil, []string{"CHAIN HALT on c-1: all 4 reachable nodes are stalled near height 3644 — …"})
	b := Issues(nil, []string{"CHAIN HALT on c-1: all 4 reachable nodes are stalled near height 3651 — …"})
	if len(a) != 1 || a[0].Key != b[0].Key {
		t.Fatalf("a moving height changed the halt's identity: %v %v", a, b)
	}
	if got := Issues(nil, []string{"val2 is 30 blocks behind the other c-1 nodes"}); len(got) != 0 {
		t.Fatalf("minor finding became an incident: %v", got)
	}
}

func TestRemoteApproverRefusesTxUnlessAllowed(t *testing.T) {
	asked := 0
	ask := func(context.Context, string) (bool, string, error) { asked++; return true, "op", nil }
	ap := RemoteApprover(ask, false)
	if ok, err := ap(nil, "unjail", toolkit.TierOnChain, nil); ok || err == nil || asked != 0 {
		t.Fatalf("tx approved without allow_tx: %v %v asked=%d", ok, err, asked)
	}
	if ok, _ := ap(nil, "restart node", toolkit.TierLocalChange, map[string]any{"command": "docker restart val01"}); !ok || asked != 1 {
		t.Fatal("local change not sent for approval")
	}
	if ok, _ := RemoteApprover(ask, true)(nil, "unjail", toolkit.TierOnChain, nil); !ok {
		t.Fatal("allow_tx ignored")
	}
}

// fakeTelegram serves the Bot API methods the approver uses.
type fakeTelegram struct {
	mu       sync.Mutex
	pressBy  []int64 // users who press "approve", in order
	sent     []string
	edited   []string
	answered []string
	cbData   string
}

func (f *fakeTelegram) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v}) }
	switch method {
	case "sendMessage":
		f.sent = append(f.sent, body["text"].(string))
		if rm, ok := body["reply_markup"].(map[string]any); ok {
			f.cbData = rm["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)["callback_data"].(string)
		}
		reply(map[string]any{"message_id": 7})
	case "getUpdates":
		var ups []any
		for i, u := range f.pressBy {
			ups = append(ups, map[string]any{"update_id": i + 1, "callback_query": map[string]any{
				"id": "cb", "data": f.cbData, "from": map[string]any{"id": u, "username": "user" + string(rune('0'+i))},
				"message": map[string]any{"chat": map[string]any{"id": 42}}}})
		}
		f.pressBy = nil
		reply(ups)
	case "answerCallbackQuery":
		f.answered = append(f.answered, body["text"].(string))
		reply(true)
	case "editMessageText":
		f.edited = append(f.edited, body["text"].(string))
		reply(true)
	}
}

func TestTelegramApproval(t *testing.T) {
	f := &fakeTelegram{pressBy: []int64{999, 111}} // a stranger first, then the operator
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	tg := &Telegram{Base: srv.URL, Token: "tok", ChatID: "42", Users: map[int64]bool{111: true}, Timeout: 5 * time.Second}
	ok, who, err := tg.Ask(context.Background(), "restart val01?")
	if err != nil || !ok || who != "user1" {
		t.Fatalf("ok=%v who=%q err=%v", ok, who, err)
	}
	if len(f.answered) < 2 || !strings.Contains(f.answered[0], "can't approve") {
		t.Fatalf("stranger's press not refused: %v", f.answered)
	}
	if len(f.edited) != 1 || !strings.Contains(f.edited[0], "<b>Approved</b> by user1") {
		t.Fatalf("message not updated: %v", f.edited)
	}
	// nobody answers: denied when the timeout passes
	f2 := &fakeTelegram{}
	srv2 := httptest.NewServer(http.HandlerFunc(f2.handler))
	defer srv2.Close()
	tg2 := &Telegram{Base: srv2.URL, Token: "tok", ChatID: "42", Timeout: 1500 * time.Millisecond}
	if ok, _, _ := tg2.Ask(context.Background(), "restart?"); ok || len(f2.edited) == 0 || !strings.Contains(f2.edited[0], "Denied") {
		t.Fatalf("timeout: ok=%v edited=%v", ok, f2.edited)
	}
}

func TestRemoteKeyUseApprovalExpires(t *testing.T) {
	var text string
	var deadline time.Time
	ap := RemoteApprover(func(ctx context.Context, s string) (bool, string, error) {
		text = s
		deadline, _ = ctx.Deadline()
		return true, "", nil
	}, true)
	ok, err := ap(nil, "KEY ACCESS — sign a transaction", toolkit.TierOnChain, map[string]any{toolkit.KeyUseMark: true})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if !strings.Contains(text, "Expires in 5m") || strings.Contains(text, toolkit.KeyUseMark) {
		t.Errorf("message: %q", text)
	}
	if time.Until(deadline) > KeyUseTimeout || time.Until(deadline) < KeyUseTimeout-time.Minute {
		t.Errorf("key use approval should expire in %s, deadline in %s", KeyUseTimeout, time.Until(deadline))
	}
}

func TestPlannedWorkIsLeftAlone(t *testing.T) {
	down := true
	planned := "node restart (until 10:15 IST)"
	var runs int
	n := &memNotify{}
	w := &Watcher{
		Mode: ModeDiagnose, Cooldown: time.Hour, StatePath: filepath.Join(t.TempDir(), "state.json"),
		Profiles: map[string]*config.Profile{"val01": {Name: "val01"}},
		Collect:  func(context.Context) []triage.NodeReport { return jailedNode(down) },
		Run: func(context.Context, *config.Profile, string, bool, toolkit.Approver) (string, error) {
			runs++
			return "ok", nil
		},
		Planned: func(context.Context, *config.Profile) string { return planned },
		Notify:  []Notifier{n},
	}
	ctx := context.Background()
	w.Sweep(ctx)
	w.Sweep(ctx)
	all := strings.Join(n.msgs, "\n")
	if runs != 0 || strings.Count(all, "planned work under way — not acting (node restart") != 1 || strings.Contains(all, "investigating") {
		t.Fatalf("planned work: runs=%d\n%s", runs, all)
	}
	planned = "" // the work ended and the issue is still there: worked now
	w.Sweep(ctx)
	if runs != 1 || !strings.Contains(strings.Join(n.msgs, "\n"), "**Status:** worked — report below") {
		t.Fatalf("not worked after the planned work: runs=%d\n%v", runs, n.msgs)
	}
	// an issue that clears during planned work is never reported as resolved
	down, planned = false, "upgrade"
	w.Sweep(ctx)
	down = true
	n.msgs = nil
	w.Sweep(ctx)
	down = false
	w.Sweep(ctx)
	if all := strings.Join(n.msgs, "\n"); runs != 1 || strings.Contains(all, "resolved") || strings.Count(all, "planned work under way") != 1 {
		t.Fatalf("cleared during planned work: runs=%d\n%s", runs, all)
	}
}

func TestQueuedAlertsKeepLatestState(t *testing.T) {
	ch := make(chan Alert, 8)
	w := &Watcher{Alerts: ch, Out: io.Discard}
	w.state.Monitor = map[string]string{"node-down:b": "val02|monitor:node-down"}
	ch <- Alert{Key: "evm-down:a", Kind: "evm-down", Resolved: true}   // raised and resolved while busy
	ch <- Alert{Key: "node-down:b", Kind: "node-down"}                 // still open
	ch <- Alert{Key: "node-down:b", Kind: "node-down", Resolved: true} // known issue: its resolve still counts
	ch <- Alert{Key: "stalled:c", Kind: "stalled"}
	got := w.pending(Alert{Key: "evm-down:a", Kind: "evm-down"})
	var keys []string
	for _, a := range got {
		keys = append(keys, fmt.Sprintf("%s/%v", a.Key, a.Resolved))
	}
	if strings.Join(keys, " ") != "node-down:b/true stalled:c/false" {
		t.Fatalf("pending = %v", keys)
	}
}

type memCarder struct {
	posts, edits, replies []string
}

func (m *memCarder) Send(_ context.Context, t string) error { m.posts = append(m.posts, t); return nil }
func (m *memCarder) Post(_ context.Context, t string) (string, error) {
	m.posts = append(m.posts, t)
	return fmt.Sprint(len(m.posts)), nil
}
func (m *memCarder) Edit(_ context.Context, ref, t string) error {
	m.edits = append(m.edits, ref+"|"+t)
	return nil
}
func (m *memCarder) Reply(_ context.Context, ref, t string) error {
	m.replies = append(m.replies, ref+"|"+t)
	return nil
}

// An incident is one message edited through its stages, plus a reply when
// it resolves — not a message per step.
func TestIssueIsOneEditedCard(t *testing.T) {
	jailed := true
	m := &memCarder{}
	w := &Watcher{
		Mode: ModeDiagnose, Cooldown: time.Hour, StatePath: filepath.Join(t.TempDir(), "state.json"),
		Profiles: map[string]*config.Profile{"val01": {Name: "val01"}},
		Collect:  func(context.Context) []triage.NodeReport { return jailedNode(jailed) },
		Run: func(context.Context, *config.Profile, string, bool, toolkit.Approver) (string, error) {
			return "restarted → /Users/x/incidents/1.md", nil
		},
		Notify: []Notifier{m},
	}
	ctx := context.Background()
	w.Sweep(ctx)
	jailed = false
	w.Sweep(ctx)
	if len(m.posts) != 1 || len(m.replies) != 1 || len(m.edits) != 2 {
		t.Fatalf("posts %d edits %d replies %d", len(m.posts), len(m.edits), len(m.replies))
	}
	if !strings.HasPrefix(m.posts[0], "**val01: Validator jailed**\ncritical\n") || !strings.Contains(m.posts[0], "**Status:** investigating") {
		t.Fatalf("card:\n%s", m.posts[0])
	}
	if e := m.edits[0]; !strings.HasPrefix(e, "1|") || !strings.Contains(e, "> restarted") || strings.Contains(e, "/Users/") {
		t.Fatalf("report edit:\n%s", e)
	}
	if r := m.replies[0]; !strings.HasPrefix(r, "1|**val01: Validator jailed**\nresolved after") {
		t.Fatalf("reply:\n%s", r)
	}
	for _, s := range append(append(m.posts, m.edits...), m.replies...) {
		if regexp.MustCompile(`[\x{1F300}-\x{1FAFF}\x{2600}-\x{27BF}]`).MatchString(s) {
			t.Fatalf("emoji in %q", s)
		}
	}
}
