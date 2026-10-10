// Package router answers known questions without a model call. Every
// prompt is classified locally first: a question the intent catalog
// covers ("is anyone jailed?", "how many peers?") runs its read-only tools
// and renders a fixed answer; anything open-ended, any action, and
// anything the catalog doesn't clearly cover goes to the model.
//
// Intents are data (YAML), like knowledge-base cases: built-ins are
// embedded, and ~/.cometcli/intents/*.yaml adds or overrides by id.
package router

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed intents/*.yaml
var builtin embed.FS

// Intent is one known question and how to answer it locally.
type Intent struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
	// Kind is "question" (default: read-only tools, a fixed answer) or
	// "command": a direct instruction whose words are the tool's
	// arguments ("restart val3", "vote yes on 6"). Commands match only by
	// pattern; their named groups fill the steps' {placeholders}; any
	// tier runs, through the normal approvals.
	Kind     string   `yaml:"kind,omitempty"`
	Patterns []string `yaml:"patterns"` // regexes over the normalized prompt; a hit routes locally
	Examples []string `yaml:"examples"` // phrasings; a close enough prompt routes locally
	// Scope: "node" needs one node (the session's, the one named, or the
	// active profile); "chain" can be answered from any node of the chain.
	Scope  string        `yaml:"scope"`
	Steps  []Step        `yaml:"steps"`
	Answer string        `yaml:"answer"` // text/template over the steps' results
	TTL    time.Duration `yaml:"ttl"`    // how long an answer is reused (default 30s)

	res []*regexp.Regexp
	tpl *template.Template
	ex  [][]string // examples as token sets
}

// Step is a read-only tool call.
type Step struct {
	As       string         `yaml:"as"`
	Tool     string         `yaml:"tool"`
	Args     map[string]any `yaml:"args"`
	Optional bool           `yaml:"optional"`
}

// Result is a step's outcome, as the answer template sees it.
type Result struct {
	Text string
	Data map[string]any
	Err  string
}

// Catalog is the set of intents.
type Catalog struct {
	Intents []*Intent
}

// Load reads the built-in intents and those in dirs (later ids override).
func Load(dirs ...string) (*Catalog, error) {
	byID := map[string]*Intent{}
	var order []string
	add := func(name string, raw []byte) error {
		var list []*Intent
		if err := yaml.Unmarshal(raw, &list); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		for _, in := range list {
			if err := in.compile(); err != nil {
				return fmt.Errorf("%s: intent %s: %w", name, in.ID, err)
			}
			if _, ok := byID[in.ID]; !ok {
				order = append(order, in.ID)
			}
			byID[in.ID] = in
		}
		return nil
	}
	files, _ := fs.Glob(builtin, "intents/*.yaml")
	sort.Strings(files)
	for _, f := range files {
		raw, _ := builtin.ReadFile(f)
		if err := add(f, raw); err != nil {
			return nil, err
		}
	}
	for _, d := range dirs {
		user, _ := filepath.Glob(filepath.Join(d, "*.yaml"))
		sort.Strings(user)
		for _, f := range user {
			raw, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			if err := add(f, raw); err != nil {
				return nil, err
			}
		}
	}
	c := &Catalog{}
	for _, id := range order {
		c.Intents = append(c.Intents, byID[id])
	}
	return c, nil
}

func (in *Intent) compile() error {
	if in.ID == "" || len(in.Steps) == 0 || (in.Answer == "" && in.Kind != "command") {
		return fmt.Errorf("needs id, steps and answer")
	}
	if in.Kind != "" && in.Kind != "question" && in.Kind != "command" {
		return fmt.Errorf("kind %q: question or command", in.Kind)
	}
	if in.Answer == "" {
		in.Answer = "{{.node}}" // commands show their tools' own output
	}
	if in.Scope == "" {
		in.Scope = "node"
	}
	if in.TTL == 0 {
		in.TTL = 30 * time.Second
	}
	for _, p := range in.Patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return err
		}
		in.res = append(in.res, re)
	}
	for _, e := range in.Examples {
		in.ex = append(in.ex, tokens(Normalize(e)))
	}
	for i := range in.Steps {
		if in.Steps[i].As == "" {
			in.Steps[i].As = fmt.Sprintf("s%d", i)
		}
	}
	tpl, err := template.New(in.ID).Funcs(funcs).Option("missingkey=zero").Parse(in.Answer)
	if err != nil {
		return err
	}
	in.tpl = tpl
	return nil
}

// --- classification ----------------------------------------------------------

// Match is the classifier's verdict.
type Match struct {
	Intent *Intent // nil: the model handles it
	Score  float64
	Why    string            // why it went where it went (for /why and tests)
	Args   map[string]string // a command's named groups
}

// Threshold is the example similarity needed to answer locally.
const Threshold = 0.8

var (
	punctRe = regexp.MustCompile(`[^a-z0-9\s.\-_]+`)
	spaceRe = regexp.MustCompile(`\s+`)
	// open-ended or acting prompts always go to the model
	openRe = regexp.MustCompile(`\b(why|how (do|can|should|to|would)|explain|should (i|we)|what if|help|fix|investigate|troubleshoot|diagnose|recommend|compare|write|create|vote (yes|no|abstain|on|for)|unjail|restart|stop|start|delegate|undelegate|send|withdraw|edit|change|update|upgrade to|install|delete|remove|prune|reset|migrate|but|because|then|after|before)\b`)
)

// Normalize lowercases a prompt and strips punctuation and extra space.
func Normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = punctRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

var stop = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "am": true, "do": true, "does": true,
	"my": true, "our": true, "me": true, "i": true, "we": true, "of": true, "on": true, "in": true,
	"for": true, "to": true, "it": true, "this": true, "that": true, "there": true, "what": true,
	"whats": true, "please": true, "can": true, "you": true, "tell": true, "show": true, "check": true,
	"right": true, "now": true, "currently": true, "current": true, "any": true, "anything": true,
	"node": true, "s": true, "with": true, "at": true, "be": true, "have": true, "has": true,
}

var synonyms = map[string]string{
	"val": "validator", "vals": "validator", "validators": "validator", "jail": "jailed",
	"jails": "jailed", "proposals": "proposal", "props": "proposal", "gov": "proposal", "governance": "proposal",
	"peer": "peers", "connections": "peers", "connected": "peers", "blockheight": "height", "block": "height",
	"blocks": "height", "synced": "sync", "syncing": "sync", "catching": "sync", "caught": "sync",
	"missed": "miss", "missing": "miss", "misses": "miss", "votes": "vote", "voted": "vote", "voting": "vote",
	"rewards": "reward", "commissions": "commission", "balances": "balance", "funds": "balance",
	"upgrades": "upgrade", "scheduled": "upgrade", "planned": "upgrade", "count": "many", "number": "many",
	"total": "many", "signing": "sign", "signed": "sign", "signs": "sign", "params": "param", "parameters": "param",
}

func tokens(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.Fields(s) {
		w = strings.Trim(w, ".-_")
		if v, ok := synonyms[w]; ok {
			w = v
		}
		if stop[w] || w == "" || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// dice is the overlap of two token sets: 2|A∩B| / (|A|+|B|).
func dice(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, w := range a {
		set[w] = true
	}
	n := 0
	for _, w := range b {
		if set[w] {
			n++
		}
	}
	return 2 * float64(n) / float64(len(a)+len(b))
}

// commandText is a prompt as commands see it: lowercased, spaces
// collapsed, trailing punctuation dropped — but ':' '@' '#' kept (images,
// peers, proposal numbers).
// politeRe is courtesy around a command: "please restart val3 now, thanks".
var politeRe = regexp.MustCompile(`^(please|pls|can you|could you|kindly)\s+|[\s,]+(please|pls|now|thanks|thank you)$`)

func commandText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimRight(s, ".!? ")
	for prev := ""; prev != s; {
		prev = s
		s = strings.TrimRight(politeRe.ReplaceAllString(s, ""), ",.!? ")
	}
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// Classify decides whether a prompt can be answered locally.
func (c *Catalog) Classify(prompt string) Match {
	if strings.Contains(strings.TrimSpace(prompt), "\n") {
		return Match{Why: "multi-line prompt"}
	}
	// direct commands first: an action spelled out completely needs no model
	ct := commandText(prompt)
	for _, in := range c.Intents {
		if in.Kind != "command" {
			continue
		}
		for _, re := range in.res {
			if m := re.FindStringSubmatch(ct); m != nil {
				args := map[string]string{}
				for i, name := range re.SubexpNames() {
					if name != "" && m[i] != "" {
						args[name] = m[i]
					}
				}
				return Match{Intent: in, Score: 1, Why: "command " + in.ID, Args: args}
			}
		}
	}
	n := Normalize(prompt)
	if n == "" {
		return Match{Why: "empty"}
	}
	if len(strings.Fields(n)) > 14 {
		return Match{Why: "long prompt"}
	}
	if m := openRe.FindString(n); m != "" {
		return Match{Why: "open-ended or an action (" + m + ")"}
	}
	for _, in := range c.Intents {
		if in.Kind == "command" {
			continue
		}
		for _, re := range in.res {
			if m := re.FindStringSubmatch(n); m != nil {
				// named groups are the steps' arguments ("proposal 7")
				var args map[string]string
				for i, name := range re.SubexpNames() {
					if name != "" && m[i] != "" {
						if args == nil {
							args = map[string]string{}
						}
						args[name] = m[i]
					}
				}
				return Match{Intent: in, Score: 1, Why: "pattern " + re.String(), Args: args}
			}
		}
	}
	tk := tokens(n)
	var best *Intent
	bestScore := 0.0
	for _, in := range c.Intents {
		if in.Kind == "command" {
			continue // commands match only by their exact patterns
		}
		for _, ex := range in.ex {
			if s := dice(tk, ex); s > bestScore {
				best, bestScore = in, s
			}
		}
	}
	if best != nil && bestScore >= Threshold {
		return Match{Intent: best, Score: bestScore, Why: fmt.Sprintf("similar to %s examples (%.2f)", best.ID, bestScore)}
	}
	why := "no known question matches"
	if best != nil {
		why = fmt.Sprintf("closest is %s at %.2f, below %.2f", best.ID, bestScore, Threshold)
	}
	return Match{Score: bestScore, Why: why}
}

// --- answers -------------------------------------------------------------------

var funcs = template.FuncMap{
	"trim": strings.TrimSpace,
	// join renders a list ([]string or []any) as "a, b, c"
	"join": func(list any) string {
		var parts []string
		switch l := list.(type) {
		case []string:
			parts = l
		case []any:
			for _, x := range l {
				parts = append(parts, fmt.Sprint(x))
			}
		}
		return strings.Join(parts, ", ")
	},
	// head keeps the first n lines
	"head": func(n int, s string) string {
		l := strings.Split(strings.TrimSpace(s), "\n")
		if len(l) > n {
			l = l[:n]
		}
		return strings.Join(l, "\n")
	},
	"lines": func(s string) []string {
		return strings.Split(strings.TrimSpace(s), "\n")
	},
	"get": func(m map[string]any, k string) any {
		if m == nil {
			return nil
		}
		return m[k]
	},
	// filter keeps the maps in a list whose key equals v
	"filter": func(list any, k string, v any) []map[string]any {
		var out []map[string]any
		switch l := list.(type) {
		case []map[string]any:
			for _, m := range l {
				if fmt.Sprint(m[k]) == fmt.Sprint(v) {
					out = append(out, m)
				}
			}
		case []any:
			for _, x := range l {
				if m, ok := x.(map[string]any); ok && fmt.Sprint(m[k]) == fmt.Sprint(v) {
					out = append(out, m)
				}
			}
		}
		return out
	},
	"count": func(x any) int {
		switch v := x.(type) {
		case []map[string]any:
			return len(v)
		case []any:
			return len(v)
		case string:
			return len(v)
		case map[string]any:
			return len(v)
		}
		return 0
	},
}

// Render fills the intent's answer from its steps' results.
func (in *Intent) Render(node string, results map[string]Result) (string, error) {
	var b bytes.Buffer
	data := map[string]any{"node": node}
	for k, v := range results {
		data[k] = v
	}
	if err := in.tpl.Execute(&b, data); err != nil {
		return "", err
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", fmt.Errorf("empty answer")
	}
	return out, nil
}

// --- cache ---------------------------------------------------------------------

// Cache keeps recent local answers.
type Cache struct {
	mu sync.Mutex
	m  map[string]cached
}

type cached struct {
	text string
	exp  time.Time
}

// Get returns a fresh answer for key.
func (c *Cache) Get(key string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || now.After(e.exp) {
		return "", false
	}
	return e.text, true
}

// Put stores an answer for ttl.
func (c *Cache) Put(key, text string, ttl time.Duration, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]cached{}
	}
	c.m[key] = cached{text: text, exp: now.Add(ttl)}
}

// Fill puts a command's named groups into a step's arguments: "{node}"
// becomes the captured word. An argument whose group didn't match is
// left out (the tool's default applies).
func Fill(args map[string]any, groups map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		s, ok := v.(string)
		if !ok {
			out[k] = v
			continue
		}
		missing := false
		s = placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
			g, ok := groups[m[1:len(m)-1]]
			if !ok {
				missing = true
			}
			return g
		})
		if !missing && s != "" {
			out[k] = s
		}
	}
	return out
}

var placeholderRe = regexp.MustCompile(`\{[a-z_]+\}`)
