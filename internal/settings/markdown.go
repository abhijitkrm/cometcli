package settings

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// --- memory ----------------------------------------------------------------

// Memory is one loaded COMET.md-style file.
type Memory struct {
	Path    string
	Scope   string // user | project | local | node
	Content string
}

// maxMemory bounds all memory text injected into the system prompt.
const maxMemory = 40_000

// LoadMemory reads, in order: ~/.cometcli/COMET.md; COMET.md (or
// AGENTS.md when a directory has no COMET.md) and COMET.local.md in each
// directory from the project root down to cwd; and in node mode
// ~/.cometcli/memory/<profile>.md. "@path" lines import another file.
func LoadMemory(root, cwd, profile string) []Memory {
	var out []Memory
	seen := map[string]bool{}
	add := func(p, scope string) {
		if seen[p] {
			return
		}
		seen[p] = true
		if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			out = append(out, Memory{Path: p, Scope: scope, Content: expandImports(string(b), filepath.Dir(p), 0)})
		}
	}
	if d, err := config.Dir(); err == nil {
		add(filepath.Join(d, "COMET.md"), "user")
	}
	for _, dir := range chain(root, cwd) {
		if _, err := os.Stat(filepath.Join(dir, "COMET.md")); err == nil {
			add(filepath.Join(dir, "COMET.md"), "project")
		} else {
			add(filepath.Join(dir, "AGENTS.md"), "project")
		}
		add(filepath.Join(dir, "COMET.local.md"), "local")
	}
	if profile != "" {
		if p, err := NodeMemoryPath(profile); err == nil {
			add(p, "node")
		}
	}
	return out
}

// NodeMemoryPath is the per-profile memory file.
func NodeMemoryPath(profile string) (string, error) {
	return config.Path("memory", profile+".md")
}

// chain lists root, then each directory down to cwd (just cwd without a
// root).
func chain(root, cwd string) []string {
	cwd = filepath.Clean(cwd)
	if root == "" {
		return []string{cwd}
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || strings.HasPrefix(rel, "..") {
		return []string{cwd}
	}
	dirs := []string{root}
	if rel == "." {
		return dirs
	}
	d := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		d = filepath.Join(d, part)
		dirs = append(dirs, d)
	}
	return dirs
}

var importRe = regexp.MustCompile(`(?m)^@(\S+)\s*$`)

// keyFileRe matches files whose content must never enter a prompt.
var keyFileRe = regexp.MustCompile(`priv_validator_key\.json|node_key\.json|mnemonic|keyring-|/\.ssh/|\.cometcli/keys`)

func expandImports(s, base string, depth int) string {
	if depth >= 3 {
		return s
	}
	return importRe.ReplaceAllStringFunc(s, func(line string) string {
		p := strings.TrimSpace(line[1:])
		if strings.HasPrefix(p, "~/") {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, p[2:])
		} else if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		if keyFileRe.MatchString(strings.ToLower(p)) {
			return "[import of key material refused: " + line[1:] + "]"
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return line
		}
		return expandImports(string(b), filepath.Dir(p), depth+1)
	})
}

// RenderMemory formats memory for the system prompt.
func RenderMemory(ms []Memory) string {
	if len(ms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("MEMORY — standing instructions from the operator's COMET.md files; follow them:\n")
	for _, m := range ms {
		fmt.Fprintf(&b, "### %s (%s)\n%s\n", m.Path, m.Scope, strings.TrimSpace(m.Content))
	}
	s := b.String()
	if len(s) > maxMemory {
		s = s[:maxMemory] + "\n…[memory truncated — trim your COMET.md files]\n"
	}
	return s + "\n"
}

// AppendMemory adds a bullet to a memory file, creating it if needed.
func AppendMemory(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, _ := f.Stat(); fi != nil && fi.Size() > 0 {
		if b, _ := os.ReadFile(path); len(b) > 0 && b[len(b)-1] != '\n' {
			if _, err := f.WriteString("\n"); err != nil {
				return err
			}
		}
	}
	_, err = f.WriteString("- " + strings.TrimSpace(text) + "\n")
	return err
}

// --- frontmatter -------------------------------------------------------------

// splitFrontmatter separates a leading "---" YAML block from the body.
func splitFrontmatter(s string) (map[string]any, string) {
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return map[string]any{}, s
	}
	rest := s[strings.Index(s, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return map[string]any{}, s
	}
	meta := map[string]any{}
	_ = yaml.Unmarshal([]byte(rest[:end]), &meta)
	body := rest[end+4:]
	body = strings.TrimPrefix(strings.TrimPrefix(body, "\r"), "\n")
	return meta, body
}

func metaStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return strings.TrimSpace(fmt.Sprint(v))
		}
	}
	return ""
}

func metaList(m map[string]any, keys ...string) []string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			var out []string
			for _, p := range splitRules(v) {
				if p != "" {
					out = append(out, p)
				}
			}
			return out
		case []any:
			var out []string
			for _, x := range v {
				out = append(out, strings.TrimSpace(fmt.Sprint(x)))
			}
			return out
		}
	}
	return nil
}

// splitRules splits "Bash(git status:*), Read" on commas outside parens.
func splitRules(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

// mdFiles collects *.md under dir, keyed by name: the relative path
// without extension, subdirectories joined with ":".
func mdFiles(dir string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(dir, strings.TrimSuffix(p, ".md"))
		out[strings.ReplaceAll(rel, string(filepath.Separator), ":")] = p
		return nil
	})
	return out
}

// --- custom slash commands ---------------------------------------------------

// Command is a markdown-defined slash command.
type Command struct {
	Name         string
	Description  string
	ArgHint      string
	AllowedTools []string // permission rules allowed while it runs
	Body         string
	Path         string
	Scope        string // user | project
}

// LoadCommands reads ~/.cometcli/commands and <root>/.cometcli/commands;
// a project command shadows a user one of the same name.
func LoadCommands(root string) map[string]*Command {
	out := map[string]*Command{}
	load := func(dir, scope string) {
		for name, p := range mdFiles(dir) {
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			meta, body := splitFrontmatter(string(raw))
			c := &Command{
				Name: name, Path: p, Scope: scope, Body: body,
				Description:  metaStr(meta, "description"),
				ArgHint:      metaStr(meta, "argument-hint", "argument_hint"),
				AllowedTools: metaList(meta, "allowed-tools", "allowed_tools"),
			}
			if c.Description == "" {
				c.Description = firstLine(body)
			}
			out[name] = c
		}
	}
	if d, err := config.Dir(); err == nil {
		load(filepath.Join(d, "commands"), "user")
	}
	if root != "" {
		load(filepath.Join(root, ".cometcli", "commands"), "project")
	}
	return out
}

// SortedCommands returns commands by name.
func SortedCommands(m map[string]*Command) []*Command {
	out := make([]*Command, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var (
	bangRe = regexp.MustCompile("!`([^`]+)`")
	atRe   = regexp.MustCompile(`(^|\s)@([\w./~-][\w./~:-]*)`)
	posRe  = regexp.MustCompile(`\$([1-9])`)
)

// Expand fills the command template: $ARGUMENTS and $1..$9, then
// !`cmd` (replaced by run's output) and @path (replaced by read's
// content; unreadable paths are left as written).
func (c *Command) Expand(args string, run func(cmd string) string, read func(path string) (string, bool)) string {
	pos := shellSplit(args)
	s := strings.ReplaceAll(c.Body, "$ARGUMENTS", args)
	s = posRe.ReplaceAllStringFunc(s, func(m string) string {
		i := int(m[1] - '1')
		if i < len(pos) {
			return pos[i]
		}
		return ""
	})
	if run != nil {
		s = bangRe.ReplaceAllStringFunc(s, func(m string) string {
			return run(bangRe.FindStringSubmatch(m)[1])
		})
	}
	if read != nil {
		s = atRe.ReplaceAllStringFunc(s, func(m string) string {
			sub := atRe.FindStringSubmatch(m)
			if body, ok := read(sub[2]); ok {
				return sub[1] + "\n<file path=\"" + sub[2] + "\">\n" + body + "\n</file>\n"
			}
			return m
		})
	}
	return strings.TrimSpace(s)
}

// shellSplit splits on spaces, honoring single and double quotes.
func shellSplit(s string) []string {
	var out []string
	var cur strings.Builder
	var q rune
	in := false
	for _, c := range s {
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			q, in = c, true
		case c == ' ' || c == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(l, "# ")); l != "" {
			if len(l) > 80 {
				l = l[:80] + "…"
			}
			return l
		}
	}
	return ""
}

// --- subagents ---------------------------------------------------------------

// AgentDef is a markdown-defined subagent.
type AgentDef struct {
	Name        string
	Description string
	Tools       []string // tool names it may use (empty = the general tools)
	Prompt      string   // its system prompt
	Path        string
}

// LoadAgents reads ~/.cometcli/agents and <root>/.cometcli/agents.
func LoadAgents(root string) map[string]*AgentDef {
	out := map[string]*AgentDef{}
	load := func(dir string) {
		for name, p := range mdFiles(dir) {
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			meta, body := splitFrontmatter(string(raw))
			if n := metaStr(meta, "name"); n != "" {
				name = n
			}
			out[name] = &AgentDef{
				Name: name, Path: p, Prompt: strings.TrimSpace(body),
				Description: metaStr(meta, "description"),
				Tools:       metaList(meta, "tools"),
			}
		}
	}
	if d, err := config.Dir(); err == nil {
		load(filepath.Join(d, "agents"))
	}
	if root != "" {
		load(filepath.Join(root, ".cometcli", "agents"))
	}
	return out
}
