// Package kb is cometcli's knowledge base of known validator failure
// modes. Each case says which triage signals point to it, how to confirm
// it, its causes, the fix (each step tagged read / change / tx), how to
// verify the fix, and what never to do. Cases are data (YAML): built-in
// ones ship in the binary; ~/.cometcli/kb/ and <project>/.cometcli/kb/
// add or override.
package kb

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed cases/*.yaml
var builtin embed.FS

// Case is one known failure mode.
type Case struct {
	ID       string   `yaml:"id"`
	Title    string   `yaml:"title"`
	Chains   []string `yaml:"chains,omitempty"` // cosmos-sdk, cosmos-evm (empty = all)
	Severity string   `yaml:"severity"`         // critical | high | medium | low
	Symptoms string   `yaml:"symptoms"`
	Match    struct {
		All []string `yaml:"all,omitempty"`
		Any []string `yaml:"any,omitempty"`
	} `yaml:"match"`
	Confirm  []string `yaml:"confirm,omitempty"`
	Causes   []string `yaml:"causes,omitempty"`
	Fix      []string `yaml:"fix,omitempty"` // "[read] …", "[change] …", "[tx] …"
	Verify   []string `yaml:"verify,omitempty"`
	Warnings []string `yaml:"warnings,omitempty"`
	Refs     []string `yaml:"refs,omitempty"`
	Test     struct {
		Signals Signals `yaml:"signals"`
	} `yaml:"test,omitempty"`

	Source string `yaml:"-"` // builtin | user | project
	all    []Cond
	any    []Cond
}

var severityRank = map[string]int{"critical": 4, "high": 3, "medium": 2, "low": 1}

// compile parses and checks a case.
func (c *Case) compile() error {
	if c.ID == "" || c.Title == "" {
		return fmt.Errorf("case needs id and title")
	}
	if _, ok := severityRank[c.Severity]; !ok {
		return fmt.Errorf("case %s: severity %q: want critical, high, medium or low", c.ID, c.Severity)
	}
	if len(c.Match.All)+len(c.Match.Any) == 0 {
		return fmt.Errorf("case %s: match needs all or any conditions", c.ID)
	}
	c.all, c.any = nil, nil
	for _, s := range c.Match.All {
		cond, err := ParseCond(s)
		if err != nil {
			return fmt.Errorf("case %s: %w", c.ID, err)
		}
		c.all = append(c.all, cond)
	}
	for _, s := range c.Match.Any {
		cond, err := ParseCond(s)
		if err != nil {
			return fmt.Errorf("case %s: %w", c.ID, err)
		}
		c.any = append(c.any, cond)
	}
	for _, f := range c.Fix {
		if !strings.HasPrefix(f, "[read]") && !strings.HasPrefix(f, "[change]") && !strings.HasPrefix(f, "[tx]") {
			return fmt.Errorf("case %s: fix step %q must start with [read], [change] or [tx]", c.ID, f)
		}
	}
	return nil
}

// AppliesTo reports whether the case is for this chain type.
func (c *Case) AppliesTo(chain string) bool {
	if len(c.Chains) == 0 || chain == "" {
		return true
	}
	for _, x := range c.Chains {
		if x == chain || (x == "cosmos-sdk" && chain == "cosmos-evm") {
			return true // cosmos-evm chains are cosmos-sdk chains too
		}
	}
	return false
}

// Hit is a case whose conditions matched.
type Hit struct {
	Case    *Case
	Score   int
	Matched []string // the conditions that held
}

// Matches reports whether the case's conditions hold for the signals.
func (c *Case) Matches(s Signals) (bool, []string) {
	var matched []string
	for _, cond := range c.all {
		if !cond.Eval(s) {
			return false, nil
		}
		matched = append(matched, cond.Raw)
	}
	if len(c.any) > 0 {
		n := 0
		for _, cond := range c.any {
			if cond.Eval(s) {
				matched = append(matched, cond.Raw)
				n++
			}
		}
		if n == 0 {
			return false, nil
		}
	}
	return true, matched
}

// Base is a loaded knowledge base.
type Base struct {
	Cases map[string]*Case
	Errs  []string // cases that failed to load (reported, not fatal)
}

// Load reads the built-in cases, then ~/.cometcli/kb and the project's
// .cometcli/kb (later ones override by id).
func Load(userDir, projectRoot string) *Base {
	b := &Base{Cases: map[string]*Case{}}
	_ = fs.WalkDir(builtin, "cases", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".yaml") {
			raw, _ := builtin.ReadFile(p)
			b.add(raw, "builtin", p)
		}
		return nil
	})
	for _, src := range []struct{ dir, name string }{{userDir, "user"}, {filepath.Join(projectRoot, ".cometcli", "kb"), "project"}} {
		if src.dir == "" || (src.name == "project" && projectRoot == "") {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(src.dir, "*.yaml"))
		for _, f := range files {
			if raw, err := os.ReadFile(f); err == nil {
				b.add(raw, src.name, f)
			}
		}
	}
	return b
}

// add parses a file holding one case or a list of cases.
func (b *Base) add(raw []byte, source, path string) {
	var many []*Case
	if err := yaml.Unmarshal(raw, &many); err != nil {
		var one Case
		if err := yaml.Unmarshal(raw, &one); err != nil {
			b.Errs = append(b.Errs, fmt.Sprintf("%s: %v", path, err))
			return
		}
		many = []*Case{&one}
	}
	for _, c := range many {
		if err := c.compile(); err != nil {
			b.Errs = append(b.Errs, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		c.Source = source
		b.Cases[c.ID] = c
	}
}

// Match returns the cases the signals point to, best first.
func (b *Base) Match(s Signals, chain string) []Hit {
	var hits []Hit
	for _, c := range b.Cases {
		if !c.AppliesTo(chain) {
			continue
		}
		if ok, matched := c.Matches(s); ok {
			hits = append(hits, Hit{Case: c, Score: len(matched)*10 + severityRank[c.Severity]*3, Matched: matched})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Case.ID < hits[j].Case.ID
	})
	return hits
}

// Search ranks cases by keyword hits in id, title, symptoms and causes.
func (b *Base) Search(q, chain string, n int) []*Case {
	words := strings.Fields(strings.ToLower(q))
	type scored struct {
		c *Case
		s int
	}
	var out []scored
	for _, c := range b.Cases {
		if !c.AppliesTo(chain) {
			continue
		}
		hay := strings.ToLower(c.ID + " " + c.Title + " " + c.Symptoms + " " + strings.Join(c.Causes, " ") + " " + strings.Join(c.Match.All, " ") + " " + strings.Join(c.Match.Any, " "))
		s := 0
		for _, w := range words {
			if len(w) < 3 {
				continue
			}
			if strings.Contains(strings.ToLower(c.Title+" "+c.ID), w) {
				s += 3
			}
			if strings.Contains(hay, w) {
				s++
			}
		}
		if s > 0 {
			out = append(out, scored{c, s})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].s != out[j].s {
			return out[i].s > out[j].s
		}
		return out[i].c.ID < out[j].c.ID
	})
	var res []*Case
	for i := 0; i < len(out) && i < n && out[i].s*2 >= out[0].s; i++ {
		res = append(res, out[i].c)
	}
	return res
}

// Render formats a case as the card the agent reads.
func (c *Case) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "CASE %s — %s [%s]\n", c.ID, c.Title, c.Severity)
	if c.Symptoms != "" {
		fmt.Fprintf(&b, "symptoms: %s\n", c.Symptoms)
	}
	section := func(name string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s:\n", name)
		for i, it := range items {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, it)
		}
	}
	section("confirm", c.Confirm)
	section("causes", c.Causes)
	section("fix", c.Fix)
	section("verify", c.Verify)
	section("NEVER", c.Warnings)
	if len(c.Refs) > 0 {
		fmt.Fprintf(&b, "refs: %s\n", strings.Join(c.Refs, " "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// Validate loads a single case file's bytes (for kb.add).
func Validate(raw []byte) (*Case, error) {
	var c Case
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if err := c.compile(); err != nil {
		return nil, err
	}
	if len(c.Test.Signals) > 0 {
		if ok, _ := c.Matches(c.Test.Signals); !ok {
			return nil, fmt.Errorf("case %s doesn't match its own test signals", c.ID)
		}
	}
	return &c, nil
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)

// ValidID reports whether id is safe as a file name.
func ValidID(id string) bool { return idRe.MatchString(id) }
