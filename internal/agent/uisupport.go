package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/settings"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/shell"
)

// CmdInfo describes a slash command for completion menus.
type CmdInfo struct {
	Name string // with the leading slash
	Args string
	Desc string
}

var helpLineRe = regexp.MustCompile(`^\s+(/\S+)(.*?)\s{2,}(\S.*)$`)

// Commands lists the slash commands available in this session: the
// shared ones, front-end ones, and custom commands.
func Commands(a *Agent) []CmdInfo {
	var out []CmdInfo
	seen := map[string]bool{}
	add := func(c CmdInfo) {
		for _, n := range strings.Split(c.Name, ",") {
			n = strings.TrimSpace(n)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, CmdInfo{Name: n, Args: c.Args, Desc: c.Desc})
		}
	}
	for _, l := range strings.Split(HelpText(nil)+"\n  /exit                         quit", "\n") {
		l = strings.ReplaceAll(l, ", /", "|/") // "/reset, /clear" names two commands
		m := helpLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		names := strings.Split(m[1], "|")
		args := strings.TrimSpace(m[2])
		for _, n := range names {
			if !strings.HasPrefix(n, "/") {
				n = "/" + n
			}
			add(CmdInfo{Name: strings.TrimSuffix(n, ","), Args: args, Desc: m[3]})
		}
	}
	for _, c := range settings.SortedCommands(builtinCommands) {
		if a != nil && a.ext != nil && a.ext.Commands[c.Name] != nil {
			continue // overridden
		}
		add(CmdInfo{Name: "/" + c.Name, Args: c.ArgHint, Desc: c.Description})
	}
	if a != nil && a.ext != nil {
		for _, c := range settings.SortedCommands(a.ext.Commands) {
			add(CmdInfo{Name: "/" + c.Name, Args: c.ArgHint, Desc: c.Description + " (" + c.Scope + ")"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var mentionRe = regexp.MustCompile(`(^|\s)@([\w./~-][\w./~:-]*)`)

// ExpandMentions attaches the content of @path mentions (files on this
// machine, relative to the shell's working directory) to a prompt. Key
// material and large or unreadable files are left as plain text.
func (a *Agent) ExpandMentions(input string) string {
	cwd := a.Tools.Cwd(a.WorkRoot)
	var files []string
	for _, m := range mentionRe.FindAllStringSubmatch(input, 10) {
		p := m[2]
		full := p
		if strings.HasPrefix(p, "~/") {
			home, _ := os.UserHomeDir()
			full = filepath.Join(home, p[2:])
		} else if !filepath.IsAbs(p) {
			full = filepath.Join(cwd, p)
		}
		if shell.Classify("cat "+full, shell.Opts{}).Forbidden != "" {
			continue
		}
		fi, err := os.Stat(full)
		if err != nil || fi.IsDir() || fi.Size() > 100_000 {
			continue
		}
		b, err := os.ReadFile(full)
		if err != nil || strings.IndexByte(string(b[:min(len(b), 8192)]), 0) >= 0 {
			continue
		}
		if a.Tools != nil {
			h, _ := a.Ctx.Host()
			key := "local:" + full
			if h != nil {
				key = h.String() + ":" + full
			}
			a.Tools.MarkRead(key, b) // the model has seen it: edits may follow
		}
		files = append(files, fmt.Sprintf("<file path=%q>\n%s\n</file>", full, strings.TrimRight(string(b), "\n")))
	}
	if len(files) == 0 {
		return input
	}
	return input + "\n\n" + strings.Join(files, "\n")
}

// ContextPercent is how full the context window is (0 when unknown).
func (a *Agent) ContextPercent() int {
	_, last := a.Usage()
	if last == 0 {
		return 0
	}
	return min(100, last*100/a.contextWindow())
}

// AddNote tells the model something on its next turn (e.g. the operator
// ran a command in their own terminal).
func (a *Agent) AddNote(note string) {
	a.carry = strings.TrimSpace(a.carry + "\n\n" + a.Redact.Text(note))
}

// AllowAlways adds an allow rule to the session and saves it to the
// project's settings.local.json (or the user settings without a
// project). Returns where it was saved.
func (a *Agent) AllowAlways(rule string) (string, error) {
	if a.Rules == nil {
		a.Rules, _ = toolkit.NewRules(nil, nil, nil)
	}
	if err := a.Rules.Add("allow", rule); err != nil {
		return "", err
	}
	path, err := settings.UserPath()
	if a.ext != nil && a.ext.Settings.Root != "" {
		path, err = filepath.Join(a.ext.Settings.Root, ".cometcli", "settings.local.json"), nil
	}
	if err != nil {
		return "", err
	}
	return path, settings.EditFile(path, func(f *settings.File) error {
		for _, r := range f.Permissions.Allow {
			if r == rule {
				return nil
			}
		}
		f.Permissions.Allow = append(f.Permissions.Allow, rule)
		return nil
	})
}

// ProjectFiles lists files under the working directory for @ completion
// (skipping VCS, dependency and build directories), up to limit.
func (a *Agent) ProjectFiles(limit int) []string {
	root := a.Tools.Cwd(a.WorkRoot)
	var out []string
	skip := map[string]bool{".git": true, "node_modules": true, "vendor": true, ".build": true, "dist": true, "evm": true, ".mnemonics": true}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && (skip[d.Name()] || strings.HasPrefix(d.Name(), ".") && d.Name() != ".cometcli") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
		if len(out) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	return out
}
