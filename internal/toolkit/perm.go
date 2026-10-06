package toolkit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Decision is the outcome of a permission check.
type Decision int

const (
	// DecideDefault means no rule matched: fall back to the tier policy.
	DecideDefault Decision = iota
	// DecideAllow runs without prompting (never for on-chain actions).
	DecideAllow
	// DecideAsk always prompts, even for read-only actions.
	DecideAsk
	// DecideDeny refuses outright.
	DecideDeny
)

func (d Decision) String() string {
	switch d {
	case DecideAllow:
		return "allow"
	case DecideAsk:
		return "ask"
	case DecideDeny:
		return "deny"
	}
	return "default"
}

// Rule is one permission pattern, in Claude Code's syntax:
//
//	bash                       any shell command
//	bash(git status)           exactly this command
//	bash(systemctl status:*)   commands starting with "systemctl status"
//	read(/etc/**)              paths matching a glob (** spans directories)
//	edit(./run-validator/**)   relative to the session's working root
//	web_fetch(domain:x.com)    a host and its subdomains
//	val.unjail, node.*         registry tools by name or prefix
//
// Tool names match case-insensitively, so Bash(...) and WebFetch(...)
// from Claude Code configs work too.
type Rule struct {
	Raw  string
	Tool string // lower-cased; may end in ".*"
	Spec string // "" matches any invocation of Tool
}

var ruleRe = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_.*-]*)\s*(?:\((.*)\))?\s*$`)

// toolAliases maps Claude Code tool names onto ours.
var toolAliases = map[string]string{"webfetch": "web_fetch", "todowrite": "todo_write", "multiedit": "edit"}

// ParseRule parses one rule string.
func ParseRule(s string) (Rule, error) {
	m := ruleRe.FindStringSubmatch(s)
	if m == nil {
		return Rule{}, fmt.Errorf("bad permission rule %q — want tool or tool(pattern)", s)
	}
	tool := strings.ToLower(m[1])
	if a, ok := toolAliases[tool]; ok {
		tool = a
	}
	spec := strings.TrimSpace(m[2])
	if spec == "*" {
		spec = ""
	}
	return Rule{Raw: strings.TrimSpace(s), Tool: tool, Spec: spec}, nil
}

func (r Rule) toolMatches(tool string) bool {
	tool = strings.ToLower(tool)
	if strings.HasSuffix(r.Tool, ".*") {
		return strings.HasPrefix(tool, strings.TrimSuffix(r.Tool, "*"))
	}
	return r.Tool == tool
}

// Request describes one action for rule matching.
type Request struct {
	Tool string
	// Specs are the things the action touches: each shell segment of a
	// compound command, a file path, or a URL. Empty for plain tools.
	Specs []string
	// Kind selects how Spec patterns match: "command", "path", "url".
	Kind string
	// Root resolves relative path patterns ("./x/**").
	Root string
}

func (r Rule) specMatches(req Request, spec string) bool {
	if r.Spec == "" {
		return true
	}
	switch req.Kind {
	case "command":
		if p, ok := strings.CutSuffix(r.Spec, ":*"); ok {
			return spec == p || strings.HasPrefix(spec, p+" ")
		}
		if strings.HasSuffix(r.Spec, "*") {
			return strings.HasPrefix(spec, strings.TrimSuffix(r.Spec, "*"))
		}
		return spec == r.Spec
	case "path":
		return MatchPathGlob(expandPattern(r.Spec, req.Root), spec)
	case "url":
		d, ok := strings.CutPrefix(r.Spec, "domain:")
		if !ok {
			return strings.HasPrefix(spec, r.Spec)
		}
		h := urlHost(spec)
		return h == d || strings.HasSuffix(h, "."+d)
	}
	return spec == r.Spec
}

func expandPattern(p, root string) string {
	switch {
	case strings.HasPrefix(p, "~/"):
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "//"): // Claude Code's absolute-path form
		return p[1:]
	case filepath.IsAbs(p):
		return p
	}
	return filepath.Join(root, p)
}

func urlHost(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndexByte(u, '@'); i >= 0 {
		u = u[i+1:]
	}
	if h, _, ok := strings.Cut(u, ":"); ok && !strings.Contains(h, "[") {
		u = h
	}
	return strings.ToLower(u)
}

// Rules is a session's permission rule set. Deny beats ask beats allow;
// an allow must cover every spec of a compound action, while a single
// matching deny or ask spec decides the whole action.
type Rules struct {
	mu               sync.RWMutex
	allow, ask, deny []Rule
}

// NewRules parses the three lists.
func NewRules(allow, ask, deny []string) (*Rules, error) {
	r := &Rules{}
	for _, l := range []struct {
		in  []string
		out *[]Rule
	}{{allow, &r.allow}, {ask, &r.ask}, {deny, &r.deny}} {
		for _, s := range l.in {
			rule, err := ParseRule(s)
			if err != nil {
				return nil, err
			}
			*l.out = append(*l.out, rule)
		}
	}
	return r, nil
}

// Add appends a rule to one list ("allow" | "ask" | "deny").
func (r *Rules) Add(list, s string) error {
	rule, err := ParseRule(s)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch list {
	case "allow":
		r.allow = append(r.allow, rule)
	case "ask":
		r.ask = append(r.ask, rule)
	case "deny":
		r.deny = append(r.deny, rule)
	default:
		return fmt.Errorf("unknown rule list %q", list)
	}
	return nil
}

// Lists returns copies of the rule strings.
func (r *Rules) Lists() (allow, ask, deny []string) {
	if r == nil {
		return nil, nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	raw := func(rs []Rule) []string {
		out := make([]string, len(rs))
		for i, x := range rs {
			out[i] = x.Raw
		}
		return out
	}
	return raw(r.allow), raw(r.ask), raw(r.deny)
}

// Decide evaluates a request. It returns the decision and the rule that
// produced it ("" for DecideDefault).
func (r *Rules) Decide(req Request) (Decision, string) {
	if r == nil {
		return DecideDefault, ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	specs := req.Specs
	if len(specs) == 0 {
		specs = []string{""}
	}
	any := func(rules []Rule) (string, bool) {
		for _, rule := range rules {
			if !rule.toolMatches(req.Tool) {
				continue
			}
			for _, s := range specs {
				if rule.specMatches(req, s) {
					return rule.Raw, true
				}
			}
		}
		return "", false
	}
	if raw, ok := any(r.deny); ok {
		return DecideDeny, raw
	}
	if raw, ok := any(r.ask); ok {
		return DecideAsk, raw
	}
	var used string
	for _, s := range specs {
		covered := false
		for _, rule := range r.allow {
			if rule.toolMatches(req.Tool) && rule.specMatches(req, s) {
				covered, used = true, rule.Raw
				break
			}
		}
		if !covered {
			return DecideDefault, ""
		}
	}
	if used != "" {
		return DecideAllow, used
	}
	return DecideDefault, ""
}

// MatchPathGlob matches a cleaned absolute path against a glob where *
// and ? stay within one path segment and ** spans any number of them.
func MatchPathGlob(pattern, path string) bool {
	re, err := globRegexp(filepath.Clean(pattern))
	if err != nil {
		return false
	}
	return re.MatchString(filepath.Clean(path))
}

var globCache sync.Map

func globRegexp(p string) (*regexp.Regexp, error) {
	if re, ok := globCache.Load(p); ok {
		return re.(*regexp.Regexp), nil
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				i++
				if i+1 < len(p) && p[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?") // "**/" matches zero or more dirs
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err == nil {
		globCache.Store(p, re)
	}
	return re, err
}
