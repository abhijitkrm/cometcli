package runbook

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Custom runbooks live in ~/.cometcli/runbooks/*.yaml:
//
//	name: my-procedure
//	desc: what it does
//	steps:
//	  - name: check status
//	    tool: node.status            # any registered tool
//	    args: {verbose: "true"}       # optional
//	    note: printed before running  # optional
//	    optional: true                # failure doesn't abort
//	  - name: do the risky thing
//	    manual: "Instructions for a human; pauses for confirmation."
type yamlStep struct {
	Name     string         `yaml:"name"`
	Tool     string         `yaml:"tool"`
	Args     map[string]any `yaml:"args"`
	Note     string         `yaml:"note"`
	Optional bool           `yaml:"optional"`
	Manual   string         `yaml:"manual"`
}

type yamlRunbook struct {
	Name  string     `yaml:"name"`
	Desc  string     `yaml:"desc"`
	Steps []yamlStep `yaml:"steps"`
}

// LoadCustom reads user runbooks from ~/.cometcli/runbooks/*.yaml.
func LoadCustom() ([]Runbook, error) {
	dir, err := config.Path("runbooks")
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Runbook
	for _, e := range ents {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yaml") && !strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var yr yamlRunbook
		if err := yaml.Unmarshal(raw, &yr); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if yr.Name == "" || len(yr.Steps) == 0 {
			return nil, fmt.Errorf("%s: runbook needs a name and at least one step", e.Name())
		}
		rb := Runbook{Name: yr.Name, Desc: yr.Desc}
		for i, s := range yr.Steps {
			st := Step{
				Name: s.Name, Tool: s.Tool, Note: s.Note,
				Optional: s.Optional, Manual: s.Manual,
			}
			if st.Name == "" {
				st.Name = fmt.Sprintf("step %d", i+1)
			}
			if s.Args != nil {
				st.Args = toolkit.Args(s.Args)
			}
			if st.Tool == "" && st.Manual == "" {
				return nil, fmt.Errorf("%s step %q: needs tool or manual", e.Name(), st.Name)
			}
			rb.Steps = append(rb.Steps, st)
		}
		out = append(out, rb)
	}
	return out, nil
}

// All returns builtins plus any user-defined runbooks; custom entries with a
// colliding name override the builtin.
func All() []Runbook {
	out := Builtins()
	custom, err := LoadCustom()
	if err != nil || len(custom) == 0 {
		return out
	}
	seen := map[string]bool{}
	for _, rb := range custom {
		seen[rb.Name] = true
	}
	var merged []Runbook
	for _, rb := range out {
		if !seen[rb.Name] {
			merged = append(merged, rb)
		}
	}
	return append(merged, custom...)
}

// GetAll finds a playbook across builtins + custom runbooks.
func GetAll(name string) (*Runbook, error) {
	for _, rb := range All() {
		if rb.Name == name {
			r := rb
			return &r, nil
		}
	}
	return nil, fmt.Errorf("no runbook %q (see `cometcli runbook list`)", name)
}

// AllNames lists builtin + custom playbook names.
func AllNames() []string {
	var out []string
	for _, rb := range All() {
		out = append(out, rb.Name)
	}
	sort.Strings(out)
	return out
}
