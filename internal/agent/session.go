package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// SessionFile is a saved conversation, written after every turn to
// ~/.cometcli/sessions/<id>.json (0600). Everything in it already passed
// the redactor on its way to the model.
type SessionFile struct {
	Version  int       `json:"version"`
	ID       string    `json:"id"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Title    string    `json:"title"`
	Cwd      string    `json:"cwd"`
	Profile  string    `json:"profile,omitempty"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Usage    Usage     `json:"usage"`
	// System is the frozen system prompt, restored verbatim so a resumed
	// session keeps its cached prefix (and replayable thinking) intact.
	System  string `json:"system"`
	ToolSig string `json:"tool_sig,omitempty"`
	Carry   string `json:"carry,omitempty"`
	History []Msg  `json:"history"`
	// ShellCwd is the bash tool's working directory; Todos the checklist.
	ShellCwd string   `json:"shell_cwd,omitempty"`
	Todos    []Todo   `json:"todos,omitempty"`
	Loaded   []string `json:"loaded_tools,omitempty"`
}

const sessionVersion = 1

// ErrNoSession is returned when there is nothing to resume.
var ErrNoSession = errors.New("no saved session to resume")

func sessionDir() (string, error) {
	d, err := config.Path("sessions")
	if err != nil {
		return "", err
	}
	return d, os.MkdirAll(d, 0o700)
}

// sessionPath maps an id to its file, rejecting anything path-like.
func sessionPath(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", fmt.Errorf("invalid session id %q", id)
	}
	d, err := sessionDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, id+".json"), nil
}

// Save writes the session atomically. A session with no turns is skipped.
func (a *Agent) Save() error {
	if len(a.history) == 0 && a.carry == "" {
		return nil
	}
	p, err := sessionPath(a.ID())
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	sf := SessionFile{
		Version: sessionVersion, ID: a.ID(), Created: a.created, Updated: time.Now(),
		Title: a.title(), Cwd: cwd, Profile: a.profileName(),
		Provider: a.Provider.Name(), Model: a.Model, Usage: a.total,
		System: a.sys, ToolSig: a.toolSig, Carry: a.carry, History: a.history,
		ShellCwd: a.Tools.Cwd(""), Todos: a.todos, Loaded: a.LoadedTools(),
	}
	raw, err := json.Marshal(sf)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// title is the first user prompt, minus injected prefixes, on one line.
func (a *Agent) title() string {
	for _, m := range a.history {
		if m.Role != "user" {
			continue
		}
		t := m.Text
		for _, tag := range []string{"</conversation-summary>", "</refreshed-snapshot>"} {
			if i := strings.LastIndex(t, tag); i >= 0 {
				t = t[i+len(tag):]
			}
		}
		t = strings.Join(strings.Fields(t), " ")
		if len(t) > 80 {
			t = safeCut(t, 80) + "…"
		}
		if t != "" {
			return t
		}
	}
	return "(compacted session)"
}

// Restore loads a saved session into the agent. The conversation is kept
// even if the provider or model changed since; raw output from another
// provider is simply not replayed.
func (a *Agent) Restore(sf *SessionFile) {
	a.Reset()
	a.id, a.created = sf.ID, sf.Created
	a.history, a.sys, a.toolSig, a.carry = sf.History, sf.System, sf.ToolSig, sf.Carry
	a.total = sf.Usage
	a.todos = sf.Todos
	a.loaded = map[string]bool{}
	for _, n := range sf.Loaded {
		a.loaded[n] = true
	}
	a.Tools = toolkit.NewSession(sf.ShellCwd)
	if a.ext != nil {
		a.ext.resumed = true
	}
	// the frozen system prompt carries the old snapshot: force a refresh
	// onto the next turn
	a.snapAt = time.Time{}
}

// LoadSession reads one saved session by id (a unique prefix works).
func LoadSession(id string) (*SessionFile, error) {
	p, err := sessionPath(id)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		all, lerr := ListSessions()
		if lerr != nil {
			return nil, lerr
		}
		var hit *SessionFile
		for _, s := range all {
			if strings.HasPrefix(s.ID, id) {
				if hit != nil {
					return nil, fmt.Errorf("session id %q is ambiguous", id)
				}
				hit = s
			}
		}
		if hit == nil {
			return nil, fmt.Errorf("no session %q", id)
		}
		return LoadSession(hit.ID)
	}
	if err != nil {
		return nil, err
	}
	var sf SessionFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return nil, fmt.Errorf("session %s: %w", id, err)
	}
	return &sf, nil
}

// ListSessions returns saved sessions, most recently updated first, with
// History omitted.
func ListSessions() ([]*SessionFile, error) {
	d, err := sessionDir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(d)
	if err != nil {
		return nil, err
	}
	var out []*SessionFile
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(d, e.Name()))
		if err != nil {
			continue
		}
		var sf SessionFile
		if json.Unmarshal(raw, &sf) != nil || sf.ID == "" {
			continue
		}
		sf.History, sf.System = nil, ""
		out = append(out, &sf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// LatestSession returns the most recent session started in cwd (and on
// profile, when set) — what `--continue` resumes.
func LatestSession(cwd, profile string) (*SessionFile, error) {
	all, err := ListSessions()
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.Cwd == cwd && (profile == "" || s.Profile == profile) {
			return LoadSession(s.ID)
		}
	}
	return nil, ErrNoSession
}
