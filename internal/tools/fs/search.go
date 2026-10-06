package fs

import (
	"fmt"
	iofs "io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

const maxGlob = 500

// skipDirs are never descended into by glob, nor searched by grep.
var skipDirs = map[string]bool{".git": true, "node_modules": true, ".build": true, "vendor": true, ".cache": true, "__pycache__": true}

// Glob finds files by name pattern.
type Glob struct{ base }

func (Glob) Name() string { return "glob" }
func (Glob) Desc() string {
	return "Find files by glob pattern (e.g. **/*.toml, config/*.json) under a directory on the node's host, newest first. Skips .git, node_modules and vendor."
}
func (Glob) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"pattern": toolkit.Str("glob; ** spans directories"),
		"path":    toolkit.Str("directory to search (default: working directory)"),
	}, "pattern")
}
func (Glob) Tier() toolkit.Tier { return toolkit.TierDiagnose }

func (Glob) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	pat := strings.TrimSpace(a.String("pattern", ""))
	if pat == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	t, err := resolve(c, a.String("path", "."))
	if err != nil {
		return nil, err
	}
	if err := t.gate(c, "glob", toolkit.TierDiagnose, "list files under "+t.path, nil, false); err != nil {
		return nil, err
	}
	full := pat
	if !path.IsAbs(pat) {
		full = path.Join(t.path, pat)
	}
	type hit struct {
		p string
		m time.Time
	}
	var hits []hit
	more := false
	if _, local := t.h.(*host.Local); local {
		err = filepath.WalkDir(t.path, func(p string, d iofs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != t.path && skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if toolkit.MatchPathGlob(full, p) {
				if len(hits) >= 5*maxGlob {
					more = true
					return filepath.SkipAll
				}
				var m time.Time
				if fi, err := d.Info(); err == nil {
					m = fi.ModTime()
				}
				hits = append(hits, hit{p, m})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		cmd := fmt.Sprintf("find %s -type f -not -path '*/.git/*' -not -path '*/node_modules/*' -not -path '*/vendor/*' 2>/dev/null | head -n 50000", quote(t.path))
		out, _, _ := t.h.Run(c, cmd)
		for _, p := range strings.Split(out, "\n") {
			if p != "" && toolkit.MatchPathGlob(full, p) {
				hits = append(hits, hit{p: p})
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].m.After(hits[j].m) })
	if len(hits) == 0 {
		return &toolkit.Result{Text: "no files match " + pat + " under " + t.path}, nil
	}
	if len(hits) > maxGlob {
		hits, more = hits[:maxGlob], true
	}
	var b strings.Builder
	for _, h := range hits {
		b.WriteString(h.p + "\n")
	}
	if more {
		fmt.Fprintf(&b, "[first %d matches — narrow the pattern or path]\n", maxGlob)
	}
	return &toolkit.Result{Text: strings.TrimRight(b.String(), "\n")}, nil
}

// Grep searches file contents with a regular expression.
type Grep struct{ base }

func (Grep) Name() string { return "grep" }
func (Grep) Desc() string {
	return "Search file contents by regular expression on the node's host (ripgrep when installed, else grep -E). output_mode: files_with_matches (default), content (matching lines with numbers), or count. Key files are never searched."
}
func (Grep) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"pattern":          toolkit.Str("extended regular expression"),
		"path":             toolkit.Str("file or directory to search (default: working directory)"),
		"glob":             toolkit.Str("only files matching this name glob, e.g. *.toml"),
		"output_mode":      toolkit.Enum("what to return", "files_with_matches", "content", "count"),
		"case_insensitive": toolkit.Bool("ignore case"),
		"context":          toolkit.Int("lines of context around each match (content mode)"),
		"head_limit":       toolkit.Int("max output lines (default 250)"),
	}, "pattern")
}
func (Grep) Tier() toolkit.Tier { return toolkit.TierDiagnose }

// grepExcludes keeps key material out of results.
var grepExcludes = []string{"priv_validator_key.json", "node_key.json", "*mnemonic*", "id_rsa*", "id_ed25519*", "id_ecdsa*"}
var grepExcludeDirs = []string{".git", "node_modules", "vendor", "keyring-file", "keyring-test", "keyring-os", ".mnemonics", ".gnupg"}

func (Grep) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	pat := a.String("pattern", "")
	if pat == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	t, err := resolve(c, a.String("path", "."))
	if err != nil {
		return nil, err
	}
	if err := t.gate(c, "grep", toolkit.TierDiagnose, "search "+t.path, nil, false); err != nil {
		return nil, err
	}
	mode := a.String("output_mode", "files_with_matches")
	limit := int(a.Int("head_limit", 250))
	if limit <= 0 {
		limit = 250
	}
	var rg, gr []string
	rg = append(rg, "rg", "--no-heading", "--color=never", "--no-messages")
	gr = append(gr, "grep", "-rEI", "--color=never", "-s")
	if a.Bool("case_insensitive", false) {
		rg, gr = append(rg, "-i"), append(gr, "-i")
	}
	switch mode {
	case "content":
		rg, gr = append(rg, "-n"), append(gr, "-n")
		if n := a.Int("context", 0); n > 0 {
			rg, gr = append(rg, fmt.Sprintf("-C%d", n)), append(gr, fmt.Sprintf("-C%d", n))
		}
	case "count":
		rg, gr = append(rg, "-c"), append(gr, "-c")
	default:
		rg, gr = append(rg, "-l"), append(gr, "-l")
	}
	if g := a.String("glob", ""); g != "" {
		rg, gr = append(rg, "--glob", quote(g)), append(gr, "--include="+quote(g))
	}
	for _, x := range grepExcludes {
		rg, gr = append(rg, "--glob", quote("!"+x)), append(gr, "--exclude="+quote(x))
	}
	for _, x := range grepExcludeDirs {
		rg, gr = append(rg, "--glob", quote("!"+x+"/")), append(gr, "--exclude-dir="+quote(x))
	}
	tail := fmt.Sprintf(" -- %s %s", quote(pat), quote(t.path))
	script := fmt.Sprintf("if command -v rg >/dev/null 2>&1; then %s%s; else %s%s; fi | head -n %d",
		strings.Join(rg, " "), tail, strings.Join(gr, " "), tail, limit+1)
	res, err := host.Exec(c, t.h, script, 64<<10)
	if err != nil {
		return nil, err
	}
	out := strings.TrimRight(res.Output, "\n")
	if mode == "count" { // grep -c lists every file; keep the hits
		var keep []string
		for _, l := range strings.Split(out, "\n") {
			if !strings.HasSuffix(l, ":0") && l != "" {
				keep = append(keep, l)
			}
		}
		out = strings.Join(keep, "\n")
	}
	if out == "" {
		return &toolkit.Result{Text: "no matches for " + pat + " under " + t.path}, nil
	}
	lines := strings.Split(out, "\n")
	if len(lines) > limit {
		out = strings.Join(lines[:limit], "\n") + fmt.Sprintf("\n[first %d lines — narrow the search or raise head_limit]", limit)
	}
	return &toolkit.Result{Text: out}, nil
}
