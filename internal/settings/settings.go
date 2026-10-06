// Package settings discovers and merges cometcli's file-based
// extensibility: settings.json at user, project and local scope, COMET.md
// memory files, custom slash commands, subagent definitions and MCP
// server configs. Formats follow Claude Code's, so its configs carry over.
//
// Project-scope hooks and MCP servers execute commands, so they only take
// effect once the project directory is trusted (`cometcli trust`).
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// Scope says which file a setting came from.
type Scope string

const (
	ScopeUser    Scope = "user"    // ~/.cometcli/settings.json
	ScopeProject Scope = "project" // <root>/.cometcli/settings.json
	ScopeLocal   Scope = "local"   // <root>/.cometcli/settings.local.json
)

// Permissions mirrors Claude Code's permissions block.
type Permissions struct {
	Allow       []string `json:"allow,omitempty"`
	Ask         []string `json:"ask,omitempty"`
	Deny        []string `json:"deny,omitempty"`
	DefaultMode string   `json:"defaultMode,omitempty"`
}

// HookCommand is one command run for a hook event.
type HookCommand struct {
	Type    string `json:"type"` // "command"
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds, default 60
}

// HookMatcher binds commands to tools whose name matches Matcher (a
// regular expression, case-insensitive; "" or "*" matches all).
type HookMatcher struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []HookCommand `json:"hooks"`
	// Scope is where the matcher was defined (set on load).
	Scope Scope `json:"-"`
}

// MCPServer is an MCP server definition (stdio or HTTP).
type MCPServer struct {
	Type    string            `json:"type,omitempty"` // stdio (default) | http
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Scope is where the server was defined (set on load).
	Scope Scope `json:"-"`
}

// File is one settings.json document.
type File struct {
	Model       string                   `json:"model,omitempty"`
	Effort      string                   `json:"effort,omitempty"`
	Permissions Permissions              `json:"permissions,omitempty"`
	Env         map[string]string        `json:"env,omitempty"`
	Hooks       map[string][]HookMatcher `json:"hooks,omitempty"`
	MCPServers  map[string]MCPServer     `json:"mcpServers,omitempty"`
}

// Settings is the merged view for one session.
type Settings struct {
	Root        string // project root ("" when none was found)
	Trusted     bool   // project hooks/MCP servers may run
	Model       string
	Effort      string
	Permissions Permissions
	Env         map[string]string
	Hooks       map[string][]HookMatcher
	MCPServers  map[string]MCPServer
	// Sources lists the files that were read, in precedence order.
	Sources []string
	// Ignored explains project hooks/servers skipped for lack of trust.
	Ignored []string
}

// FindRoot walks up from dir to the nearest directory holding a
// .cometcli/ folder, a COMET.md or a .git, stopping at the home directory.
func FindRoot(dir string) string {
	home, _ := os.UserHomeDir()
	d := filepath.Clean(dir)
	for {
		if d != home {
			for _, marker := range []string{".cometcli", "COMET.md", ".git"} {
				if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
					return d
				}
			}
		}
		parent := filepath.Dir(d)
		if parent == d || d == home {
			return ""
		}
		d = parent
	}
}

// Load merges user, project and local settings for a session started in
// cwd. Later scopes override scalar keys; permission rules, env, hooks
// and MCP servers accumulate (a later server definition of the same name
// wins).
func Load(cwd string) (*Settings, error) {
	s := &Settings{Root: FindRoot(cwd), Env: map[string]string{}, Hooks: map[string][]HookMatcher{}, MCPServers: map[string]MCPServer{}}
	s.Trusted = s.Root != "" && IsTrusted(s.Root)
	userDir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	type src struct {
		path  string
		scope Scope
	}
	srcs := []src{{filepath.Join(userDir, "settings.json"), ScopeUser}}
	if s.Root != "" {
		srcs = append(srcs,
			src{filepath.Join(s.Root, ".cometcli", "settings.json"), ScopeProject},
			src{filepath.Join(s.Root, ".cometcli", "settings.local.json"), ScopeLocal})
	}
	for _, sc := range srcs {
		f, err := readFile(sc.path)
		if err != nil {
			return nil, err
		}
		if f == nil {
			continue
		}
		s.Sources = append(s.Sources, sc.path)
		s.apply(f, sc.scope)
	}
	// a project's .mcp.json (Claude Code's file) adds project servers
	if s.Root != "" {
		var mj struct {
			MCPServers map[string]MCPServer `json:"mcpServers"`
		}
		p := filepath.Join(s.Root, ".mcp.json")
		if raw, err := os.ReadFile(p); err == nil {
			if err := json.Unmarshal(raw, &mj); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			s.Sources = append(s.Sources, p)
			for n, srv := range mj.MCPServers {
				s.addServer(n, srv, ScopeProject)
			}
		}
	}
	return s, nil
}

func readFile(p string) (*File, error) {
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &f, nil
}

func (s *Settings) apply(f *File, scope Scope) {
	if f.Model != "" {
		s.Model = f.Model
	}
	if f.Effort != "" {
		s.Effort = f.Effort
	}
	if f.Permissions.DefaultMode != "" {
		s.Permissions.DefaultMode = f.Permissions.DefaultMode
	}
	s.Permissions.Allow = append(s.Permissions.Allow, f.Permissions.Allow...)
	s.Permissions.Ask = append(s.Permissions.Ask, f.Permissions.Ask...)
	s.Permissions.Deny = append(s.Permissions.Deny, f.Permissions.Deny...)
	for k, v := range f.Env {
		s.Env[k] = v
	}
	for ev, ms := range f.Hooks {
		for _, m := range ms {
			if scope != ScopeUser && !s.Trusted {
				s.Ignored = append(s.Ignored, fmt.Sprintf("%s hook (%s settings)", ev, scope))
				continue
			}
			m.Scope = scope
			s.Hooks[ev] = append(s.Hooks[ev], m)
		}
	}
	for n, srv := range f.MCPServers {
		s.addServer(n, srv, scope)
	}
}

func (s *Settings) addServer(name string, srv MCPServer, scope Scope) {
	if scope != ScopeUser && !s.Trusted {
		s.Ignored = append(s.Ignored, fmt.Sprintf("MCP server %q (%s)", name, scope))
		return
	}
	srv.Scope = scope
	s.MCPServers[name] = srv
}

// ApplyEnv exports the settings' env vars into this process (before
// providers read their API keys). Existing variables are not overridden.
func (s *Settings) ApplyEnv() {
	for k, v := range s.Env {
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
}

// --- trust -----------------------------------------------------------------

func trustFile() (string, error) { return config.Path("trusted.json") }

func trustedList() []string {
	p, err := trustFile()
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var dirs []string
	_ = json.Unmarshal(raw, &dirs)
	return dirs
}

// IsTrusted reports whether dir (or a parent of it) was trusted.
func IsTrusted(dir string) bool {
	dir = filepath.Clean(dir)
	for _, t := range trustedList() {
		if dir == t || strings.HasPrefix(dir, t+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Trust marks dir as trusted (or untrusted when on is false).
func Trust(dir string, on bool) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	var out []string
	for _, t := range trustedList() {
		if t != dir {
			out = append(out, t)
		}
	}
	if on {
		out = append(out, dir)
	}
	sort.Strings(out)
	raw, _ := json.MarshalIndent(out, "", "  ")
	p, err := trustFile()
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o600)
}

// Trusted lists trusted directories.
func Trusted() []string { return trustedList() }

// --- user settings editing (cometcli mcp add/remove) -----------------------

// EditFile loads, mutates and saves one settings file.
func EditFile(path string, fn func(*File) error) error {
	f, err := readFile(path)
	if err != nil {
		return err
	}
	if f == nil {
		f = &File{}
	}
	// keep keys we don't model by round-tripping through a generic map
	raw, _ := os.ReadFile(path)
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	if err := fn(f); err != nil {
		return err
	}
	b, _ := json.Marshal(f)
	var mine map[string]any
	_ = json.Unmarshal(b, &mine)
	if generic == nil {
		generic = map[string]any{}
	}
	for _, k := range []string{"model", "effort", "permissions", "env", "hooks", "mcpServers"} {
		if v, ok := mine[k]; ok {
			generic[k] = v
		} else {
			delete(generic, k)
		}
	}
	out, err := json.MarshalIndent(generic, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o600)
}

// UserPath is the user settings file.
func UserPath() (string, error) { return config.Path("settings.json") }
