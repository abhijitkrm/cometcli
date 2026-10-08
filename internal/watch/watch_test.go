package watch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	if len(runs) != 1 || !runs[0] || !strings.Contains(all, "🚨 val01: val-jailed-downtime [critical]") || !strings.Contains(all, "📋 val01 report:\ncause: container stopped") {
		t.Fatalf("first sweep: runs=%v\n%s", runs, all)
	}
	w.Sweep(ctx) // still jailed: nothing new
	if len(runs) != 1 {
		t.Fatal("worked the same incident twice")
	}
	jailed = false
	w.Sweep(ctx) // cleared
	if !strings.Contains(n.msgs[len(n.msgs)-1], "✅ resolved: val01: val-jailed-downtime") {
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
	if len(f.edited) != 1 || !strings.Contains(f.edited[0], "✅ approved by user1") {
		t.Fatalf("message not updated: %v", f.edited)
	}
	// nobody answers: denied when the timeout passes
	f2 := &fakeTelegram{}
	srv2 := httptest.NewServer(http.HandlerFunc(f2.handler))
	defer srv2.Close()
	tg2 := &Telegram{Base: srv2.URL, Token: "tok", ChatID: "42", Timeout: 1500 * time.Millisecond}
	if ok, _, _ := tg2.Ask(context.Background(), "restart?"); ok || len(f2.edited) == 0 || !strings.Contains(f2.edited[0], "denied") {
		t.Fatalf("timeout: ok=%v edited=%v", ok, f2.edited)
	}
}
