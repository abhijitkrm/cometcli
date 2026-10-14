package silence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Planned work is recorded locally too, so the watcher leaves a node alone
// while cometcli works on it even with no monitor configured.
const plannedFile = "planned.json"

type plan struct {
	Until time.Time `json:"until"`
	Why   string    `json:"why"`
}

var plannedMu sync.Mutex

func planKey(p *config.Profile, chainWide bool) string {
	if chainWide {
		return "chain:" + p.ChainID
	}
	return "profile:" + p.Name
}

func loadPlans() (map[string]plan, string) {
	path, err := config.Path(plannedFile)
	if err != nil {
		return nil, ""
	}
	m := map[string]plan{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m, path
}

// Record notes planned work on p (or its whole chain) for d.
func Record(p *config.Profile, chainWide bool, d time.Duration, why string) {
	plannedMu.Lock()
	defer plannedMu.Unlock()
	m, path := loadPlans()
	if path == "" {
		return
	}
	now := time.Now()
	for k, v := range m {
		if now.After(v.Until) {
			delete(m, k)
		}
	}
	m[planKey(p, chainWide)] = plan{Until: now.Add(d), Why: why}
	if b, err := json.Marshal(m); err == nil {
		_ = os.WriteFile(path, b, 0o600)
	}
}

// Planned says why p is under planned work right now: cometcli recorded it
// here, or a monitor holds an active silence for the node (or its whole
// chain). "" means it isn't. Monitors that can't be reached are skipped.
func Planned(c *toolkit.Context, cfg *config.Config, p *config.Profile) string {
	if p == nil {
		return ""
	}
	plannedMu.Lock()
	m, _ := loadPlans()
	plannedMu.Unlock()
	now := time.Now()
	for _, k := range []string{planKey(p, false), planKey(p, true)} {
		if v, ok := m[k]; ok && now.Before(v.Until) {
			return fmt.Sprintf("%s (until %s)", v.Why, v.Until.Format("15:04 MST"))
		}
	}
	targets, closeAll := monitors(c, cfg, p)
	defer closeAll()
	labels := map[string]string{"profile": p.Name, "node": p.Name, "chain_id": p.ChainID}
	token := os.Getenv(TokenCredential)
	if token == "" {
		token = config.Credential(TokenCredential)
	}
	for _, t := range targets {
		hc := &http.Client{Timeout: 10 * time.Second}
		if t.ctx != nil {
			var err error
			if hc, err = t.ctx.NodeHTTP(t.url, 10*time.Second); err != nil {
				continue
			}
		}
		list, err := List(c, hc, t.url, token)
		if err != nil {
			continue
		}
		for _, s := range list {
			if s.State == "active" && coversNode(s.Matchers, labels) {
				return fmt.Sprintf("silenced at %s by %s: %s (until %s)", t.url, orDash(s.CreatedBy), s.Comment, s.EndsAt.Local().Format("15:04 MST"))
			}
		}
	}
	return ""
}

// Silence is a listed silence.
type Silence struct {
	Matchers  []Matcher `json:"matchers"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
	State     string    `json:"-"`
}

// List reads the silences at base (Alertmanager v2 API).
func List(ctx context.Context, hc *http.Client, base, token string) ([]Silence, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(strings.TrimSuffix(base, "/metrics"), "/")+"/api/v2/silences", nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoAPI
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var raw []struct {
		Silence
		Matchers []struct {
			Name    string `json:"name"`
			Value   string `json:"value"`
			IsRegex bool   `json:"isRegex"`
			IsEqual *bool  `json:"isEqual"` // absent = true (Alertmanager)
		} `json:"matchers"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]Silence, len(raw))
	for i, r := range raw {
		out[i] = r.Silence
		out[i].State = r.Status.State
		out[i].Matchers = nil
		for _, m := range r.Matchers {
			out[i].Matchers = append(out[i].Matchers, Matcher{Name: m.Name, Value: m.Value, IsRegex: m.IsRegex, IsEqual: m.IsEqual == nil || *m.IsEqual})
		}
	}
	return out, nil
}

// coversNode: every matcher is on one of the node's labels and matches —
// a silence of the node (or its chain) as a whole. Silences that also
// select an alert kind (the chain view's no-nodes|stalled) are about
// particular alerts, not work on the node, and don't count.
func coversNode(ms []Matcher, labels map[string]string) bool {
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		v, ok := labels[m.Name]
		if !ok {
			return false
		}
		hit := v == m.Value
		if m.IsRegex {
			re, err := regexp.Compile("^(?:" + m.Value + ")$")
			hit = err == nil && re.MatchString(v)
		}
		if hit != m.IsEqual {
			return false
		}
	}
	return true
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
