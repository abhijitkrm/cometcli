package watch

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/abhijitkrm/cometcli/internal/monitor"
)

// Telegram asks for approvals with inline Approve / Deny buttons and
// waits for a press from the configured chat (and, if set, users).
// Anything unanswered by Timeout is a "no".
type Telegram struct {
	Base    string // API base (tests); default https://api.telegram.org
	Token   string
	ChatID  string
	Users   map[int64]bool // allowed approvers; empty = anyone in the chat
	Timeout time.Duration
	HTTP    *http.Client

	mu     sync.Mutex // one approval at a time; getUpdates offsets are shared
	offset int64
}

func (t *Telegram) api(ctx context.Context, method string, body any, out any) error {
	base := t.Base
	if base == "" {
		base = "https://api.telegram.org"
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/bot%s/%s", base, t.Token, method), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	hc := t.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err) // never echo the URL: it holds the token
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// Send posts a message (cometcli's Markdown, rendered for Telegram).
func (t *Telegram) Send(ctx context.Context, text string) error {
	return t.send(ctx, "sendMessage", map[string]any{"chat_id": t.ChatID}, clip(text, 3500), nil)
}

// send calls method with text as Telegram HTML, or as plain text when
// Telegram refuses the markup.
func (t *Telegram) send(ctx context.Context, method string, m map[string]any, text string, out any) error {
	m["text"], m["parse_mode"], m["disable_web_page_preview"] = monitor.TelegramHTML(text), "HTML", true
	err := t.api(ctx, method, m, out)
	if err != nil && strings.Contains(err.Error(), "can't parse entities") {
		delete(m, "parse_mode")
		m["text"] = text
		err = t.api(ctx, method, m, out)
	}
	return err
}

// Ask posts the question with buttons and waits for the answer.
func (t *Telegram) Ask(ctx context.Context, text string) (bool, string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	err := t.send(ctx, "sendMessage", map[string]any{
		"chat_id": t.ChatID,
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{{
			{"text": "Approve", "callback_data": "a:" + id},
			{"text": "Deny", "callback_data": "d:" + id},
		}}},
	}, clip(text, 3500), &msg)
	if err != nil {
		return false, "", err
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		wait := int(time.Until(deadline).Seconds())
		if wait > 25 {
			wait = 25
		}
		var updates []struct {
			UpdateID int64 `json:"update_id"`
			Callback *struct {
				ID   string `json:"id"`
				Data string `json:"data"`
				From struct {
					ID       int64  `json:"id"`
					Username string `json:"username"`
				} `json:"from"`
				Message struct {
					Chat struct {
						ID int64 `json:"id"`
					} `json:"chat"`
				} `json:"message"`
			} `json:"callback_query"`
		}
		if err := t.api(ctx, "getUpdates", map[string]any{"offset": t.offset, "timeout": wait, "allowed_updates": []string{"callback_query"}}, &updates); err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		for _, u := range updates {
			t.offset = u.UpdateID + 1
			cq := u.Callback
			if cq == nil || len(cq.Data) < 3 || cq.Data[2:] != id {
				continue
			}
			if fmt.Sprint(cq.Message.Chat.ID) != t.ChatID || (len(t.Users) > 0 && !t.Users[cq.From.ID]) {
				_ = t.api(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": cq.ID, "text": "you can't approve this"}, nil)
				continue
			}
			ok := strings.HasPrefix(cq.Data, "a:")
			who := cq.From.Username
			if who == "" {
				who = fmt.Sprint(cq.From.ID)
			}
			verdict := "**Denied** by " + who
			if ok {
				verdict = "**Approved** by " + who
			}
			_ = t.api(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": cq.ID, "text": verdict}, nil)
			_ = t.send(ctx, "editMessageText", map[string]any{"chat_id": t.ChatID, "message_id": msg.MessageID}, clip(text, 3500)+"\n\n"+verdict, nil)
			return ok, who, nil
		}
	}
	_ = t.send(ctx, "editMessageText", map[string]any{"chat_id": t.ChatID, "message_id": msg.MessageID}, clip(text, 3500)+"\n\n**Denied:** no answer in "+timeout.String(), nil)
	return false, "", nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
