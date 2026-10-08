// Package incidents keeps a record of every incident cometcli worked on,
// per profile (~/.cometcli/incidents/<profile>/<id>.{json,md}): what was
// wrong, the root cause, what was done (with tx hashes), the outcome, and
// a timeline rebuilt from the audit log. Triage reads it back, so a repeat
// problem is recognized as one.
package incidents

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
)

// Incident is one worked incident.
type Incident struct {
	ID        string    `json:"id"`
	Profile   string    `json:"profile"`
	Title     string    `json:"title"`
	Case      string    `json:"case,omitempty"` // knowledge-base case id
	Started   time.Time `json:"started"`
	Resolved  time.Time `json:"resolved,omitempty"`
	RootCause string    `json:"root_cause"`
	Evidence  []string  `json:"evidence,omitempty"`
	Actions   []string  `json:"actions,omitempty"`
	TxHashes  []string  `json:"tx_hashes,omitempty"`
	Outcome   string    `json:"outcome"`
	Timeline  []string  `json:"timeline,omitempty"`
	Session   string    `json:"session,omitempty"`
}

func dir(profile string) (string, error) {
	if profile == "" || strings.ContainsAny(profile, `/\`) || strings.HasPrefix(profile, ".") {
		return "", fmt.Errorf("incidents: bad profile name %q", profile)
	}
	return config.Path("incidents", profile)
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Save writes inc (assigning an ID if empty) and returns the Markdown path.
func Save(inc *Incident) (string, error) {
	d, err := dir(inc.Profile)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	if inc.ID == "" {
		slug := inc.Case
		if slug == "" {
			slug = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(inc.Title), "-"), "-")
		}
		if len(slug) > 40 {
			slug = slug[:40]
		}
		inc.ID = inc.Started.UTC().Format("20060102-1504") + "-" + slug
	}
	raw, err := json.MarshalIndent(inc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(d, inc.ID+".json"), raw, 0o600); err != nil {
		return "", err
	}
	md := filepath.Join(d, inc.ID+".md")
	return md, os.WriteFile(md, []byte(Markdown(inc)), 0o600)
}

// Markdown renders an incident as a short postmortem.
func Markdown(inc *Incident) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", inc.Title)
	fmt.Fprintf(&b, "- **Node:** %s\n", inc.Profile)
	if inc.Case != "" {
		fmt.Fprintf(&b, "- **Known case:** `%s`\n", inc.Case)
	}
	fmt.Fprintf(&b, "- **Started:** %s\n", inc.Started.UTC().Format("2006-01-02 15:04 UTC"))
	if !inc.Resolved.IsZero() {
		fmt.Fprintf(&b, "- **Resolved:** %s (%s)\n", inc.Resolved.UTC().Format("2006-01-02 15:04 UTC"),
			inc.Resolved.Sub(inc.Started).Round(time.Minute))
	}
	fmt.Fprintf(&b, "- **Outcome:** %s\n\n", inc.Outcome)
	fmt.Fprintf(&b, "## Root cause\n\n%s\n", inc.RootCause)
	list := func(title string, items []string, code bool) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", title)
		for _, it := range items {
			if code {
				fmt.Fprintf(&b, "- `%s`\n", it)
			} else {
				fmt.Fprintf(&b, "- %s\n", it)
			}
		}
	}
	list("Evidence", inc.Evidence, false)
	list("Actions", inc.Actions, false)
	list("Transactions", inc.TxHashes, true)
	list("Timeline (UTC)", inc.Timeline, false)
	return b.String()
}

// List returns profile's incidents since t, newest first.
func List(profile string, since time.Time) ([]Incident, error) {
	d, err := dir(profile)
	if err != nil {
		return nil, err
	}
	files, _ := filepath.Glob(filepath.Join(d, "*.json"))
	var out []Incident
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var inc Incident
		if json.Unmarshal(raw, &inc) == nil && !inc.Started.Before(since) {
			out = append(out, inc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out, nil
}

// Get loads one incident by id.
func Get(profile, id string) (*Incident, error) {
	d, err := dir(profile)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
		return nil, fmt.Errorf("bad incident id %q", id)
	}
	raw, err := os.ReadFile(filepath.Join(d, id+".json"))
	if err != nil {
		return nil, fmt.Errorf("no incident %s for %s", id, profile)
	}
	var inc Incident
	return &inc, json.Unmarshal(raw, &inc)
}

// FromAudit fills the timeline and tx hashes from the audit log: the
// session's prompts, tool calls, approvals and broadcasts between since
// and until (all of profile's events when session is "").
func FromAudit(profile, session string, since, until time.Time) (timeline, hashes []string) {
	d, err := config.Path("audit")
	if err != nil {
		return nil, nil
	}
	for day := since.UTC().Truncate(24 * time.Hour); !day.After(until); day = day.Add(24 * time.Hour) {
		f, err := os.Open(filepath.Join(d, day.Format("2006-01-02")+".jsonl"))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var e audit.Event
			if json.Unmarshal(sc.Bytes(), &e) != nil || e.TS.Before(since) || e.TS.After(until) {
				continue
			}
			if e.Profile != profile || (session != "" && e.Session != session) {
				continue
			}
			if line := describe(e); line != "" {
				timeline = append(timeline, e.TS.UTC().Format("15:04:05")+" "+line)
			}
			if e.Kind == audit.KindTx && e.Detail["stage"] == "broadcast" {
				if h, _ := e.Detail["hash"].(string); h != "" {
					hashes = append(hashes, fmt.Sprintf("%s (code %v)", h, e.Detail["code"]))
				}
			}
		}
		f.Close()
	}
	if len(timeline) > 60 { // keep the start and the end
		timeline = append(append(timeline[:30:30], fmt.Sprintf("… %d events …", len(timeline)-50)), timeline[len(timeline)-20:]...)
	}
	return timeline, hashes
}

func describe(e audit.Event) string {
	d := e.Detail
	str := func(k string) string { s, _ := d[k].(string); return clip(s, 120) }
	switch e.Kind {
	case audit.KindPrompt:
		return "operator: " + str("text")
	case audit.KindTool:
		line := "tool " + str("name")
		if err := str("error"); err != "" {
			line += " — error: " + err
		}
		return line
	case audit.KindShell:
		return "shell: " + str("cmd")
	case audit.KindApproval:
		verdict := "declined"
		if g, _ := d["granted"].(bool); g {
			verdict = "approved"
		}
		return verdict + ": " + strings.SplitN(str("prompt"), "\n", 2)[0]
	case audit.KindTx:
		if d["stage"] == "broadcast" {
			return fmt.Sprintf("tx broadcast %v (code %v)", d["hash"], d["code"])
		}
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
