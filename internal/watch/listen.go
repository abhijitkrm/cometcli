package watch

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// Alert is a monitor's alert, normalized from cometduty's webhook payload or
// an Alertmanager webhook. Only the structured fields steer what cometcli
// does; a monitor's free text (which can quote node logs) is never passed on.
type Alert struct {
	Source   string // cometduty | alertmanager
	Key      string // pairs a raise with its resolve
	Kind     string // node-down, stalled, … (alertname for Alertmanager)
	Resolved bool
	Severity string // critical | warning | info
	Node     string
	Target   string
	ChainID  string
	Labels   map[string]string
}

// Signature headers of cometduty's webhook (HMAC-SHA256 over "<ts>.<body>").
const (
	TimestampHeader = "X-Cometduty-Timestamp"
	SignatureHeader = "X-Cometduty-Signature"
	// MaxSkew is how old (or how far ahead) a signed alert may be.
	MaxSkew = 5 * time.Minute
)

// Receiver accepts monitor alerts over HTTP and hands them to the watcher.
// A request must carry a valid cometduty signature (Secret) or the bearer
// Token; with neither configured nothing is accepted.
type Receiver struct {
	Secret string
	Token  string
	Out    chan<- Alert
	Now    func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // signatures already accepted (replays)
}

func (r *Receiver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 256<<10))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if err := r.authorize(req, body); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	alerts, err := ParseAlerts(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, a := range alerts {
		select {
		case r.Out <- a:
		default:
			http.Error(w, "busy — alert queue full", http.StatusTooManyRequests)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func (r *Receiver) authorize(req *http.Request, body []byte) error {
	if sig := req.Header.Get(SignatureHeader); sig != "" && r.Secret != "" {
		ts, err := strconv.ParseInt(req.Header.Get(TimestampHeader), 10, 64)
		if err != nil {
			return fmt.Errorf("bad %s", TimestampHeader)
		}
		if d := r.now().Sub(time.Unix(ts, 0)); d > MaxSkew || d < -MaxSkew {
			return fmt.Errorf("signature timestamp outside ±%s", MaxSkew)
		}
		if !hmac.Equal([]byte(sig), []byte(Sign(r.Secret, ts, body))) {
			return fmt.Errorf("bad signature")
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.seen == nil {
			r.seen = map[string]time.Time{}
		}
		for s, at := range r.seen {
			if r.now().Sub(at) > 2*MaxSkew {
				delete(r.seen, s)
			}
		}
		if _, dup := r.seen[sig]; dup {
			return fmt.Errorf("replayed alert")
		}
		r.seen[sig] = r.now()
		return nil
	}
	if got, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); ok && r.Token != "" {
		if subtle.ConstantTimeCompare([]byte(got), []byte(r.Token)) == 1 {
			return nil
		}
	}
	return fmt.Errorf("unsigned or wrong token")
}

// Sign is cometduty's webhook signature for a body sent at ts.
func Sign(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// ParseAlerts reads a cometduty webhook payload (version 1) or an
// Alertmanager webhook (version 4).
func ParseAlerts(body []byte) ([]Alert, error) {
	var probe struct {
		Version string          `json:"version"`
		Source  string          `json:"source"`
		Alerts  json.RawMessage `json:"alerts"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	switch {
	case probe.Source == "cometduty":
		var p struct {
			Status   string            `json:"status"`
			Key      string            `json:"key"`
			Kind     string            `json:"kind"`
			Target   string            `json:"target"`
			Severity string            `json:"severity"`
			ChainID  string            `json:"chain_id"`
			Node     string            `json:"node"`
			Labels   map[string]string `json:"labels"`
			Reminder bool              `json:"reminder"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, err
		}
		if p.Key == "" || p.Kind == "" {
			return nil, fmt.Errorf("cometduty payload without key/kind (webhook template_body must be unset)")
		}
		if p.Reminder {
			return nil, nil // nothing new
		}
		return []Alert{{Source: "cometduty", Key: p.Key, Kind: p.Kind, Resolved: p.Status == "resolved",
			Severity: p.Severity, Node: p.Node, Target: p.Target, ChainID: p.ChainID, Labels: p.Labels}}, nil
	case len(probe.Alerts) > 0:
		var list []struct {
			Status      string            `json:"status"`
			Labels      map[string]string `json:"labels"`
			Fingerprint string            `json:"fingerprint"`
		}
		if err := json.Unmarshal(probe.Alerts, &list); err != nil {
			return nil, fmt.Errorf("alertmanager alerts: %w", err)
		}
		var out []Alert
		for _, a := range list {
			l := a.Labels
			key := a.Fingerprint
			if key == "" {
				key = labelKey(l)
			}
			out = append(out, Alert{Source: "alertmanager", Key: key, Kind: l["alertname"],
				Resolved: a.Status == "resolved", Severity: l["severity"], Node: l["node"],
				Target: l["instance"], ChainID: l["chain_id"], Labels: l})
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown alert format (want a cometduty or Alertmanager webhook)")
}

func labelKey(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + l[k] + ",")
	}
	return b.String()
}

// ProfileFor finds the profile an alert is about: an explicit "profile"
// label, a node named like a profile, or a target on a profile's host.
// "" means the alert isn't about one node (or not one cometcli knows).
func ProfileFor(a Alert, profiles map[string]*config.Profile) string {
	if p := a.Labels["profile"]; p != "" && profiles[p] != nil {
		return p
	}
	if a.Node != "" && profiles[a.Node] != nil {
		return a.Node
	}
	host := hostOf(a.Target)
	if host == "" || host == "127.0.0.1" || host == "localhost" {
		return "" // a loopback target names no host
	}
	var hit string
	for name, p := range profiles {
		if hostOf(p.Endpoints.Comet) == host || hostOf(p.Endpoints.EVM) == host || p.Transport.Host == host {
			if a.ChainID != "" && p.ChainID != "" && a.ChainID != p.ChainID {
				continue
			}
			if hit != "" {
				return "" // two nodes on that host: ambiguous
			}
			hit = name
		}
	}
	return hit
}

func hostOf(s string) string {
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "tcp://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	if h, _, err := net.SplitHostPort(u.Host); err == nil {
		return h
	}
	return u.Host
}
