// Package silence mutes a monitor's alerts while cometcli does planned work
// on a node (restart, recreate, reconfigure, upgrade switch). It speaks the
// Alertmanager v2 silence API, which both Alertmanager and cometduty serve.
// It is optional: with no monitor configured nothing is called, and a
// monitor that can't be reached never blocks the work.
package silence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// TokenCredential holds the monitors' API token (env or credentials file).
const TokenCredential = "MONITOR_API_TOKEN"

// ErrNoAPI means the monitor answers but serves no silence API (cometduty
// without api_token) — nothing to silence there, and nothing to report.
var ErrNoAPI = errors.New("no silence API")

// Matcher is one Alertmanager label matcher.
type Matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

// Create posts one silence to the API at base, returning its id.
func Create(ctx context.Context, hc *http.Client, base, token string, m []Matcher, d time.Duration, comment string) (string, error) {
	now := time.Now().UTC()
	body, _ := json.Marshal(map[string]any{
		"matchers": m, "startsAt": now.Format(time.RFC3339), "endsAt": now.Add(d).Format(time.RFC3339),
		"createdBy": "cometcli", "comment": comment,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(strings.TrimSuffix(base, "/metrics"), "/")+"/api/v2/silences", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNoAPI
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		SilenceID string `json:"silenceID"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.SilenceID, nil
}

// ForNode silences one node's alerts for d (matched by its profile label),
// or the whole chain's when chainWide (an upgrade halts every node), at the
// node's own monitor (endpoints.monitor) and every monitor.silence API. It
// returns a line for the tool's output, "" when no monitor is configured.
func ForNode(c *toolkit.Context, cfg *config.Config, p *config.Profile, chainWide bool, d time.Duration, why string) string {
	if p == nil {
		return ""
	}
	type target struct {
		url string
		ctx *toolkit.Context
		// source: the monitor reads the chain through this node (via), so
		// its chain-level "can't see the chain" alerts are this work too
		source bool
	}
	var targets []target
	if p.Endpoints.Monitor != "" {
		targets = append(targets, target{url: p.Endpoints.Monitor, ctx: c})
	}
	if cfg != nil {
		for _, s := range cfg.Monitor.Silence {
			t := target{url: s.URL, source: s.Via == p.Name}
			if vp := cfg.Profiles[s.Via]; s.Via != "" && vp != nil {
				vp.Name = s.Via
				if s.Via == p.Name {
					t.ctx = c
				} else {
					vc := &toolkit.Context{Context: c, Profile: vp, Cfg: cfg}
					defer vc.Close()
					t.ctx = vc
				}
			}
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return ""
	}
	m := []Matcher{{Name: "profile", Value: p.Name, IsEqual: true}}
	scope := p.Name
	if chainWide {
		m = []Matcher{{Name: "chain_id", Value: p.ChainID, IsEqual: true}}
		scope = p.ChainID
	}
	token := os.Getenv(TokenCredential)
	if token == "" {
		token = config.Credential(TokenCredential)
	}
	var ok, failed []string
	for _, t := range targets {
		hc := &http.Client{Timeout: 10 * time.Second}
		if t.ctx != nil {
			var err error
			if hc, err = t.ctx.NodeHTTP(t.url, 10*time.Second); err != nil {
				failed = append(failed, fmt.Sprintf("%s (%v)", t.url, err))
				continue
			}
		}
		_, err := Create(c, hc, t.url, token, m, d, "cometcli: "+why)
		if err == nil && t.source && !chainWide {
			_, err = Create(c, hc, t.url, token, []Matcher{
				{Name: "chain_id", Value: p.ChainID, IsEqual: true},
				{Name: "alertname", Value: "no-nodes|stalled", IsRegex: true, IsEqual: true},
			}, d, "cometcli: "+why+" (the monitor's view of the chain)")
		}
		if errors.Is(err, ErrNoAPI) {
			continue
		} else if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", t.url, err))
		} else {
			ok = append(ok, t.url)
		}
	}
	line := ""
	if len(ok) > 0 {
		line = fmt.Sprintf("alerts for %s silenced for %s at %s", scope, d, strings.Join(ok, ", "))
	}
	if len(failed) > 0 {
		if line != "" {
			line += "; "
		}
		line += "could not silence alerts at " + strings.Join(failed, ", ") + " — expect pages for this work"
	}
	return line
}
