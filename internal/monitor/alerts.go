package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Rule evaluates a snapshot and returns an alert string ("" = ok).
type Rule struct {
	Name string
	Eval func(s *Snapshot, prev *Snapshot) string
}

// DefaultRules are the built-in alert rules.
func DefaultRules(missedThreshold int64, diskPct float64, stallSecs int) []Rule {
	return []Rule{
		{Name: "unreachable", Eval: func(s, p *Snapshot) string {
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
		{Name: "service-down", Eval: func(s, p *Snapshot) string {
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook %s: %d", w.url, resp.StatusCode)
	}
	return nil
}

type telegram struct{ token, chatID string }

func (t *telegram) Name() string { return "telegram" }

func (t *telegram) Send(ctx context.Context, msg string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token)
	body, _ := json.Marshal(map[string]any{"chat_id": t.chatID, "text": msg})
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// Watcher polls snapshots and fires sinks on rule triggers (deduped).
type Watcher struct {
	Ctx       *toolkit.Context
	Interval  time.Duration
	Rules     []Rule
	Sinks     []Sink
	OnEvent   func(msg string, isAlert bool)
	fired     map[string]bool
	prev      *Snapshot
}

// Run loops until ctx is cancelled.
func (w *Watcher) Run() {
	if w.Interval == 0 {
		w.Interval = 10 * time.Second
	}
	w.fired = map[string]bool{}
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
		msg := r.Eval(s, w.prev)
		key := r.Name + ":" + msg
		if msg != "" && !w.fired[key] {
			w.fired[key] = true
			alert := fmt.Sprintf("[cometcli %s] %s: %s", w.Ctx.Profile.Name, r.Name, msg)
			if w.OnEvent != nil {
				w.OnEvent(alert, true)
			}
			for _, sink := range w.Sinks {
				_ = sink.Send(w.Ctx, alert)
			}
		}
	}
	w.prev = s
}
