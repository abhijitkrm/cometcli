package shell

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

const (
	defaultTimeout = 2 * time.Minute
	maxTimeout     = 10 * time.Minute
	maxOutput      = 30_000
	cwdMarker      = "__COMETCLI_CWD__"
)

// Bash runs shell commands on the profile's host.
type Bash struct{}

func (Bash) Name() string { return "bash" }
func (Bash) Desc() string {
	return "Run a shell command on the node's host (local or over SSH). The working directory persists between calls; environment changes do not. stdin is closed, so commands that prompt get EOF. Read-only commands run immediately; commands that change the host ask the operator; transactions always ask; consensus keys, key ceremonies and state resets are refused. Prefer the read/grep/glob tools for files."
}
func (Bash) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"command":     toolkit.Str("the shell command to run (bash syntax)"),
		"description": toolkit.Str("5-10 words on what this does, shown to the operator"),
		"timeout":     toolkit.Int("seconds before the command is killed (default 120, max 600)"),
	}, "command")
}
func (Bash) Tier() toolkit.Tier { return toolkit.TierLocalChange }
func (Bash) DynamicTier() bool  { return true }
func (Bash) AgentOnly() bool    { return true }
func (Bash) OutputLimit() int   { return maxOutput }

// Timeout covers the command plus time for the operator to approve it.
func (Bash) Timeout(a toolkit.Args) time.Duration { return cmdTimeout(a) + 15*time.Minute }

func cmdTimeout(a toolkit.Args) time.Duration {
	d := time.Duration(a.Int("timeout", 0)) * time.Second
	if d <= 0 {
		return defaultTimeout
	}
	return min(d, maxTimeout)
}

func (Bash) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	cmd := strings.TrimSpace(a.String("command", ""))
	if cmd == "" {
		return nil, fmt.Errorf("command is required")
	}
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	cwd := c.Session.Cwd(defaultCwd(h))
	binary := ""
	if c.Profile != nil {
		binary = c.Profile.Binary
	}
	v := Classify(cmd, Opts{Binary: binary, ReadScript: func(p string) ([]byte, bool) {
		return readScript(c, h, cwd, p)
	}})
	detail := map[string]any{"command": cmd, "host": h.String()}
	if d := a.String("description", ""); d != "" {
		detail["description"] = d
	}
	if cwd != "" {
		detail["cwd"] = cwd
	}
	if v.Reason != "" {
		detail["why"] = v.Reason
	}
	if len(v.Notes) > 0 {
		detail["notes"] = strings.Join(v.Notes, "\n")
	}
	prompt := "run on " + h.String() + ": " + cmd
	if err := c.Check(toolkit.Gate{
		Request:   toolkit.Request{Tool: "bash", Specs: v.Segments, Kind: "command"},
		Tier:      v.Tier,
		Prompt:    prompt,
		Detail:    detail,
		Forbidden: v.Forbidden,
	}); err != nil {
		return nil, err
	}

	ctx, cancel := toolkit.WithDeadline(c, cmdTimeout(a))
	defer cancel()
	defer ctx.Close()
	res, err := host.Exec(ctx, h, wrap(cmd, cwd), maxOutput)
	c.LogShell(cmd, res.Code)
	if err != nil {
		return nil, err
	}
	out, newCwd := splitCwd(res.Output)
	if res.Code == 97 && strings.TrimSpace(out) == "" && cwd != "" {
		c.Session.SetCwd(defaultCwd(h))
		return nil, fmt.Errorf("working directory %s no longer exists — reset to %s; run the command again", cwd, c.Session.Cwd(""))
	}
	if newCwd != "" {
		c.Session.SetCwd(newCwd)
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(out, "\n"))
	if res.TimedOut {
		fmt.Fprintf(&b, "\n[killed after %s — pass a larger timeout (max 600s) or narrow the command]", cmdTimeout(a))
	} else if res.Code != 0 {
		fmt.Fprintf(&b, "\n[exit code %d]", res.Code)
	}
	for _, n := range v.Notes {
		fmt.Fprintf(&b, "\n[note: %s]", n)
	}
	text := strings.TrimLeft(b.String(), "\n")
	if text == "" {
		text = "(no output)"
	}
	return &toolkit.Result{Text: text, Data: map[string]any{
		"exit_code": res.Code, "cwd": c.Session.Cwd(newCwd), "timed_out": res.TimedOut, "truncated": res.Truncated,
	}}, nil
}

// defaultCwd is where a fresh session starts: the process directory for a
// local host, the login directory over SSH ("" = don't cd).
func defaultCwd(h host.Host) string {
	if _, ok := h.(*host.Local); ok {
		if d, err := os.Getwd(); err == nil {
			return d
		}
	}
	return ""
}

// wrap runs cmd in cwd and reports the final working directory after it.
func wrap(cmd, cwd string) string {
	var b strings.Builder
	if cwd != "" {
		fmt.Fprintf(&b, "cd -- %s 2>/dev/null || exit 97\n", quote(cwd))
	}
	b.WriteString(cmd)
	fmt.Fprintf(&b, "\n__cometcli_rc=$?\nprintf '\\n%s%%s\\n' \"$(pwd)\"\nexit $__cometcli_rc\n", cwdMarker)
	return b.String()
}

// splitCwd removes the trailing cwd marker line (and the newline wrap
// printed before it) from the output.
func splitCwd(out string) (string, string) {
	i := strings.LastIndex(out, cwdMarker)
	if i < 0 || (i > 0 && out[i-1] != '\n') {
		return out, ""
	}
	dir, _, _ := strings.Cut(out[i+len(cwdMarker):], "\n")
	return strings.TrimSuffix(out[:i], "\n"), strings.TrimSpace(dir)
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// readScript fetches a script the command would run, for classification.
func readScript(c *toolkit.Context, h host.Host, cwd, p string) ([]byte, bool) {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if _, local := h.(*host.Local); local {
				p = path.Join(home, p[2:])
			}
		}
	} else if !path.IsAbs(p) && cwd != "" {
		p = path.Join(cwd, p)
	}
	fi, err := h.Stat(c, p)
	if err != nil || fi.IsDir() || fi.Size() > 1<<20 {
		return nil, false
	}
	b, err := h.ReadFile(c, p)
	return b, err == nil
}

// Register adds the bash tool.
func Register(r *toolkit.Registry) { r.Register(Bash{}) }
