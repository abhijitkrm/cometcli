// Package fs implements the agent's file tools — read, write, edit, glob
// and grep — over the profile's host transport, so they work the same on
// the local machine and over SSH.
package fs

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Register adds the file tools.
func Register(r *toolkit.Registry) {
	r.Register(Read{})
	r.Register(Write{})
	r.Register(Edit{})
	r.Register(Glob{})
	r.Register(Grep{})
}

const (
	maxReadBytes = 10 << 20
	defaultLines = 2000
	maxLineLen   = 2000
)

// base is embedded by every file tool.
type base struct{}

func (base) AgentOnly() bool  { return true }
func (base) OutputLimit() int { return 30_000 }

// target resolves a tool path against the session's working directory.
type target struct {
	h    host.Host
	path string // absolute, cleaned
	key  string // host-qualified, for read tracking
}

func resolve(c *toolkit.Context, p string) (*target, error) {
	if strings.TrimSpace(p) == "" {
		return nil, fmt.Errorf("file_path is required")
	}
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	_, local := h.(*host.Local)
	if strings.HasPrefix(p, "~/") && local {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if !path.IsAbs(p) {
		cwd := c.Session.Cwd("")
		if cwd == "" && local {
			cwd, _ = os.Getwd()
		}
		if cwd == "" {
			return nil, fmt.Errorf("use an absolute path on %s", h)
		}
		p = path.Join(cwd, p)
	}
	p = path.Clean(p)
	return &target{h: h, path: p, key: h.String() + ":" + p}, nil
}

// inRoot reports whether p lies under the session's work root (for
// accept-edits); only meaningful on the local host.
func (t *target) inRoot(c *toolkit.Context) bool {
	if _, local := t.h.(*host.Local); !local || c.WorkRoot == "" {
		return false
	}
	rel, err := filepath.Rel(c.WorkRoot, t.path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

var protectedRe = regexp.MustCompile(`priv_validator_key\.json$|node_key\.json$|(^|/)\.?mnemonics?(\.txt)?(/|$)|mnemonic|(^|/)keyring-(file|test|os)(/|$)|/\.ssh/id_|/\.cometcli/keys(/|$)|/\.gnupg/|(^|/)keystore(/|$)|(^|/)utc--[0-9]|(^|/)key_seed\.json$`)

// forbidden explains why a path may never pass through the agent.
func forbidden(p string, write bool) string {
	if m := protectedRe.FindString(strings.ToLower(p)); m != "" {
		return "protected key material (" + strings.Trim(m, "/") + ") never passes through the agent"
	}
	if write && strings.HasSuffix(p, "priv_validator_state.json") {
		return "rewriting priv_validator_state.json risks double-signing"
	}
	return ""
}

func (t *target) gate(c *toolkit.Context, tool string, tier toolkit.Tier, prompt string, detail map[string]any, write bool) error {
	return c.Check(toolkit.Gate{
		Request:   toolkit.Request{Tool: tool, Specs: []string{t.path}, Kind: "path"},
		Tier:      tier,
		Prompt:    prompt,
		Detail:    detail,
		Forbidden: forbidden(t.path, write),
		InRoot:    write && t.inRoot(c),
	})
}

// readFile fetches content with size and binary checks.
func (t *target) readFile(c *toolkit.Context) ([]byte, error) {
	fi, err := t.h.Stat(c, t.path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory — use glob or bash ls", t.path)
	}
	if fi.Size() > maxReadBytes {
		return nil, fmt.Errorf("%s is %d MB — too large; use bash with head/tail/grep", t.path, fi.Size()>>20)
	}
	b, err := t.h.ReadFile(c, t.path)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(b[:min(len(b), 8192)], 0) >= 0 {
		return nil, fmt.Errorf("%s looks binary — use bash (xxd, file, strings)", t.path)
	}
	return b, nil
}

func (t *target) exists(c *toolkit.Context) (os.FileInfo, bool) {
	fi, err := t.h.Stat(c, t.path)
	return fi, err == nil
}

// writeFile writes atomically, creating parent directories and keeping
// an existing file's mode.
func (t *target) writeFile(c *toolkit.Context, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, ok := t.exists(c); ok {
		mode = fi.Mode().Perm()
	} else if _, local := t.h.(*host.Local); local {
		if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
			return err
		}
	} else if _, _, err := t.h.Run(c, "mkdir -p -- "+quote(path.Dir(t.path))); err != nil {
		return err
	}
	return t.h.WriteFile(c, t.path, data, mode)
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// --- read ------------------------------------------------------------------

// Read shows a text file with line numbers.
type Read struct{ base }

func (Read) Name() string { return "read" }
func (Read) Desc() string {
	return "Read a text file on the node's host with line numbers (relative paths resolve against the shell's working directory). Reads up to 2000 lines; use offset/limit for more. Required before write/edit of an existing file. Key material is refused."
}
func (Read) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"file_path": toolkit.Str("path to the file"),
		"offset":    toolkit.Int("first line to show, 1-based (default 1)"),
		"limit":     toolkit.Int("number of lines to show (default 2000)"),
	}, "file_path")
}
func (Read) Tier() toolkit.Tier { return toolkit.TierDiagnose }

func (Read) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	t, err := resolve(c, a.String("file_path", ""))
	if err != nil {
		return nil, err
	}
	if err := t.gate(c, "read", toolkit.TierDiagnose, "read "+t.path, nil, false); err != nil {
		return nil, err
	}
	b, err := t.readFile(c)
	if err != nil {
		return nil, err
	}
	c.Session.MarkRead(t.key, b)
	if len(b) == 0 {
		return &toolkit.Result{Text: "(empty file)"}, nil
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	off := max(int(a.Int("offset", 1)), 1)
	lim := int(a.Int("limit", defaultLines))
	if lim <= 0 {
		lim = defaultLines
	}
	if off > len(lines) {
		return nil, fmt.Errorf("offset %d is past the end (%d lines)", off, len(lines))
	}
	end := min(off-1+lim, len(lines))
	var out strings.Builder
	for i := off - 1; i < end; i++ {
		l := lines[i]
		if len(l) > maxLineLen {
			l = l[:maxLineLen] + "…[line truncated]"
		}
		fmt.Fprintf(&out, "%6d\t%s\n", i+1, l)
	}
	if off > 1 || end < len(lines) {
		fmt.Fprintf(&out, "[lines %d-%d of %d", off, end, len(lines))
		if end < len(lines) {
			fmt.Fprintf(&out, "; pass offset=%d for more", end+1)
		}
		out.WriteString("]\n")
	}
	return &toolkit.Result{Text: strings.TrimRight(out.String(), "\n")}, nil
}

// --- write -----------------------------------------------------------------

// Write creates or replaces a file.
type Write struct{ base }

func (Write) Name() string { return "write" }
func (Write) Desc() string {
	return "Create or overwrite a file on the node's host. Overwriting requires reading the file first in this session. Prefer edit for changes to existing files. The operator approves the diff."
}
func (Write) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"file_path": toolkit.Str("path to the file"),
		"content":   toolkit.Str("the complete new file content"),
	}, "file_path", "content")
}
func (Write) Tier() toolkit.Tier { return toolkit.TierLocalChange }

func (Write) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	t, err := resolve(c, a.String("file_path", ""))
	if err != nil {
		return nil, err
	}
	content, _ := a["content"].(string)
	detail := map[string]any{"path": t.path, "host": t.h.String()}
	verb := "create"
	if _, ok := t.exists(c); ok {
		if f := forbidden(t.path, true); f != "" {
			return nil, fmt.Errorf("refused: %s", f)
		}
		old, err := t.readFile(c)
		if err != nil {
			return nil, err
		}
		if err := c.Session.CheckFresh(t.key, old); err != nil {
			return nil, err
		}
		if string(old) == content {
			return &toolkit.Result{Text: "no change — file already has this content"}, nil
		}
		detail["diff"] = toolkit.Diff(t.path, string(old), content)
		verb = "overwrite"
	} else {
		detail["new_file"] = fmt.Sprintf("%d lines", strings.Count(content, "\n")+1)
	}
	if err := t.gate(c, "write", toolkit.TierLocalChange, verb+" "+t.path+" on "+t.h.String(), detail, true); err != nil {
		return nil, err
	}
	if err := t.writeFile(c, []byte(content)); err != nil {
		return nil, err
	}
	c.Session.MarkRead(t.key, []byte(content))
	return &toolkit.Result{Text: fmt.Sprintf("%sd %s (%d bytes)", strings.TrimSuffix(verb, "e"), t.path, len(content))}, nil
}

// --- edit ------------------------------------------------------------------

// Edit replaces exact text in a file.
type Edit struct{ base }

func (Edit) Name() string { return "edit" }
func (Edit) Desc() string {
	return "Replace an exact string in a file on the node's host. old_string must match the file exactly (including indentation) and be unique unless replace_all is set. Read the file first. The operator approves the diff."
}
func (Edit) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"file_path":   toolkit.Str("path to the file"),
		"old_string":  toolkit.Str("exact text to replace"),
		"new_string":  toolkit.Str("replacement text"),
		"replace_all": toolkit.Bool("replace every occurrence (default false)"),
	}, "file_path", "old_string", "new_string")
}
func (Edit) Tier() toolkit.Tier { return toolkit.TierLocalChange }

func (Edit) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	t, err := resolve(c, a.String("file_path", ""))
	if err != nil {
		return nil, err
	}
	if f := forbidden(t.path, true); f != "" {
		return nil, fmt.Errorf("refused: %s", f)
	}
	oldS, _ := a["old_string"].(string)
	newS, _ := a["new_string"].(string)
	if oldS == "" {
		return nil, fmt.Errorf("old_string is empty — use write to create a file")
	}
	if oldS == newS {
		return nil, fmt.Errorf("old_string and new_string are identical")
	}
	b, err := t.readFile(c)
	if err != nil {
		return nil, err
	}
	if err := c.Session.CheckFresh(t.key, b); err != nil {
		return nil, err
	}
	cur := string(b)
	n := strings.Count(cur, oldS)
	switch {
	case n == 0:
		return nil, fmt.Errorf("old_string not found in %s — it must match exactly, including whitespace; read the file again", t.path)
	case n > 1 && !a.Bool("replace_all", false):
		return nil, fmt.Errorf("old_string occurs %d times in %s — include more surrounding context to make it unique, or set replace_all", n, t.path)
	}
	count := 1
	if a.Bool("replace_all", false) {
		count = n
	}
	next := strings.Replace(cur, oldS, newS, count)
	detail := map[string]any{"path": t.path, "host": t.h.String(), "diff": toolkit.Diff(t.path, cur, next)}
	if err := t.gate(c, "edit", toolkit.TierLocalChange, "edit "+t.path+" on "+t.h.String(), detail, true); err != nil {
		return nil, err
	}
	if err := t.writeFile(c, []byte(next)); err != nil {
		return nil, err
	}
	c.Session.MarkRead(t.key, []byte(next))
	return &toolkit.Result{Text: fmt.Sprintf("edited %s (%d replacement(s))\n%s", t.path, count, detail["diff"])}, nil
}
