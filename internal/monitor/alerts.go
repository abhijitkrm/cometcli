package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Rule evaluates a snapshot and returns an alert string ("" = ok).
type Rule struct {
	Name string
	Eval func(s *Snapshot, prev *Snapshot) string
	// Consecutive requires the rule to fire on N successive polls before
	// alerting — guards against transient blips paging operators.
	Consecutive int
}

// DefaultRules are the built-in alert rules.
func DefaultRules(missedThreshold int64, diskPct float64, stallSecs int) []Rule {
	return []Rule{
		{Name: "unreachable", Consecutive: 2, Eval: func(s, p *Snapshot) string {
			if !s.Reachable {
				return "node RPC unreachable"
			}
			return ""
		}},
		{Name: "height-stall", Eval: func(s, p *Snapshot) string {
			if p != nil && s.Reachable && s.Height == p.Height &&
				s.TS.Sub(p.TS) > time.Duration(stallSecs)*time.Second {
				return fmt.Sprintf("height stuck at %d for %s", s.Height, s.TS.Sub(p.TS).Round(time.Second))
			}
			return ""
		}},
		{Name: "catching-up", Eval: func(s, p *Snapshot) string {
			if s.CatchingUp {
				return "node is catching up"
			}
			return ""
		}},
		{Name: "missed-blocks", Eval: func(s, p *Snapshot) string {
			if s.Window > 0 && s.Missed >= missedThreshold {
				return fmt.Sprintf("missed %d/%d blocks (uptime %.1f%%)", s.Missed, s.Window, s.UptimePct)
			}
			return ""
		}},
		{Name: "jailed", Eval: func(s, p *Snapshot) string {
			if s.Jailed {
				return "validator is JAILED"
			}
			return ""
		}},
		{Name: "tombstoned", Eval: func(s, p *Snapshot) string {
			if s.Tombstoned {
				return "validator TOMBSTONED (double-sign) — do NOT restart signing blindly"
			}
			return ""
		}},
		{Name: "service-down", Consecutive: 2, Eval: func(s, p *Snapshot) string {
			if s.ServiceUp != nil && !*s.ServiceUp {
				return "node service is not active"
			}
			return ""
		}},
		{Name: "no-peers", Eval: func(s, p *Snapshot) string {
			if s.Reachable && s.Peers == 0 {
				return "0 peers — isolated"
			}
			return ""
		}},
		{Name: "disk", Eval: func(s, p *Snapshot) string {
			if s.DiskUsedPct >= diskPct {
				return fmt.Sprintf("disk %.0f%% used", s.DiskUsedPct)
			}
			return ""
		}},
		{Name: "evm-drift", Eval: func(s, p *Snapshot) string {
			if s.EVMDrift > 25 {
				return fmt.Sprintf("EVM JSON-RPC lagging %d blocks", s.EVMDrift)
			}
			return ""
		}},
	}
}

// Sink delivers an alert message somewhere.
type Sink interface {
	Send(ctx context.Context, msg string) error
	Name() string
}

// Sinks builds sinks from profile alert config. Values may be "env:VAR".
func Sinks(a config.Alerts) []Sink {
	var out []Sink
	resolve := func(v string) string {
		if strings.HasPrefix(v, "env:") {
			return os.Getenv(strings.TrimPrefix(v, "env:"))
		}
		return v
	}
	if w := resolve(a.SlackWebhook); w != "" {
		out = append(out, &webhook{kind: "slack", url: w})
	}
	if w := resolve(a.DiscordWebhook); w != "" {
		out = append(out, &webhook{kind: "discord", url: w})
	}
	if tok, chat := resolve(a.TelegramToken), resolve(a.TelegramChatID); tok != "" && chat != "" {
		out = append(out, &telegram{token: tok, chatID: chat})
	}
	return out
}

type webhook struct{ kind, url string }

func (w *webhook) Name() string { return w.kind }

func (w *webhook) Send(ctx context.Context, msg string) error {
	var body []byte
	if w.kind == "discord" {
		body, _ = json.Marshal(map[string]string{"content": msg})
	} else {
		body, _ = json.Marshal(map[string]string{"text": msg})
	}
	req, err := http.NewRequestWithContext(ctx, "POST", w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sinkHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook %s: %d", w.url, resp.StatusCode)
	}
	return nil
}

// sinkHTTP bounds sink latency so a stalling endpoint can't freeze the
// watcher loop.
var sinkHTTP = &http.Client{Timeout: 10 * time.Second}

type telegram struct{ token, chatID string }

func (t *telegram) Name() string { return "telegram" }

func (t *telegram) Send(ctx context.Context, msg string) error {
	_, err := t.Post(ctx, msg)
	return err
}

// Post sends msg and returns its message id, so watch can edit it later.
func (t *telegram) Post(ctx context.Context, msg string) (string, error) {
	return t.send(ctx, "sendMessage", map[string]any{"chat_id": t.chatID}, msg)
}

// Edit replaces a posted message's text.
func (t *telegram) Edit(ctx context.Context, ref, msg string) error {
	_, err := t.send(ctx, "editMessageText", map[string]any{"chat_id": t.chatID, "message_id": ref}, msg)
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

// Reply posts msg as a reply to a posted message.
func (t *telegram) Reply(ctx context.Context, ref, msg string) error {
	_, err := t.send(ctx, "sendMessage", map[string]any{"chat_id": t.chatID, "reply_to_message_id": ref, "allow_sending_without_reply": true}, msg)
	return err
}

// send renders msg as Telegram HTML (plain text if Telegram refuses the
// markup) and returns the message id.
func (t *telegram) send(ctx context.Context, method string, m map[string]any, msg string) (string, error) {
	m["text"], m["parse_mode"], m["disable_web_page_preview"] = TelegramHTML(msg), "HTML", true
	id, err := t.post(ctx, method, m)
	if err != nil && strings.Contains(err.Error(), "can't parse entities") {
		delete(m, "parse_mode")
		m["text"] = msg
		id, err = t.post(ctx, method, m)
	}
	return id, err
}

func (t *telegram) post(ctx context.Context, method string, m map[string]any) (string, error) {
	url := fmt.Sprintf("%s/bot%s/%s", telegramAPI, t.token, method)
	body, _ := json.Marshal(m)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sinkHTTP.Do(req)
	if err != nil {
		return "", errors.New(strings.ReplaceAll(err.Error(), t.token, "<token>"))
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env)
	if resp.StatusCode >= 300 || !env.OK {
		return "", fmt.Errorf("telegram %s: %d %s", method, resp.StatusCode, env.Description)
	}
	return strconv.FormatInt(env.Result.MessageID, 10), nil
}

// telegramAPI is the Bot API base; a var so tests can stub it.
var telegramAPI = "https://api.telegram.org"

// Watcher polls snapshots and fires sinks on rule triggers (deduped).
type Watcher struct {
	Ctx      *toolkit.Context
	Interval time.Duration
	Rules    []Rule
	Sinks    []Sink
	OnEvent  func(msg string, isAlert bool)
	// Triage, if set, diagnoses each firing alert — its output is appended
	// to the alert text delivered to sinks (bounded, best-effort).
	Triage func(rule, msg string) string
	// Once runs a single check pass and returns.
	Once bool
	// Muted skips these rule names entirely.
	Muted map[string]bool
	// Repeat re-fires a still-firing alert after this duration (0 = once).
	Repeat time.Duration
	fired  map[string]time.Time
	streak map[string]int
	prev   *Snapshot
}

// Run loops until ctx is cancelled.
func (w *Watcher) Run() {
	if w.Interval == 0 {
		w.Interval = 10 * time.Second
	}
	w.fired = map[string]time.Time{}
	w.streak = map[string]int{}
	if w.Once {
		w.check()
		return
	}
	tick := time.NewTicker(w.Interval)
	defer tick.Stop()
	for {
		w.check()
		select {
		case <-w.Ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (w *Watcher) check() {
	s := Collect(w.Ctx)
	for _, r := range w.Rules {
		if w.Muted[r.Name] {
			continue
		}
		msg := r.Eval(s, w.prev)
		if msg == "" {
			w.streak[r.Name] = 0
			continue
		}
		w.streak[r.Name]++
		if need := r.Consecutive; !w.Once && need > 0 && w.streak[r.Name] < need {
			continue
		}
		key := r.Name + ":" + msg
		if at, ok := w.fired[key]; ok {
			if w.Repeat <= 0 || time.Since(at) < w.Repeat {
				continue
			}
		}
		w.fired[key] = time.Now()
		alert := fmt.Sprintf("[cometcli %s] %s: %s", w.Ctx.Profile.Name, r.Name, msg)
		if w.Triage != nil {
			if d := w.Triage(r.Name, msg); d != "" {
				alert += "\n— triage: " + d
			}
		}
		if w.OnEvent != nil {
			w.OnEvent(alert, true)
		}
		for _, sink := range w.Sinks {
			_ = sink.Send(w.Ctx, alert)
		}
	}
	w.prev = s
}
