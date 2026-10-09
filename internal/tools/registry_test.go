package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/router"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Strict schema validators (Groq, OpenAI strict mode) reject null where the
// JSON-Schema metaschema expects an object or array — one bad tool fails
// every agent request, so check them all.
func TestToolSchemasHaveNoNulls(t *testing.T) {
	reg := toolkit.NewRegistry()
	RegisterAll(reg)
	for _, tl := range reg.All() {
		b, err := json.Marshal(tl.Schema())
		if err != nil {
			t.Fatalf("%s: schema not serializable: %v", tl.Name(), err)
		}
		if strings.Contains(string(b), "null") {
			t.Errorf("%s: schema contains null: %s", tl.Name(), b)
		}
		var s map[string]any
		json.Unmarshal(b, &s)
		if s["type"] != "object" {
			t.Errorf("%s: top-level schema type = %v, want object", tl.Name(), s["type"])
		}
		if _, ok := s["properties"].(map[string]any); !ok {
			t.Errorf("%s: properties must be an object", tl.Name())
		}
	}
}

// Playbooks and local answers name tools by string: every name must exist,
// and local answers may only read.
func TestPlaybooksAndIntentsUseRealTools(t *testing.T) {
	reg := toolkit.NewRegistry()
	RegisterAll(reg)
	base := kb.Load("", "")
	if len(base.Errs) > 0 {
		t.Fatalf("kb: %v", base.Errs)
	}
	n := 0
	for id, c := range base.Cases {
		for _, st := range c.Playbook {
			n++
			if _, ok := reg.Get(st.Do); !ok {
				t.Errorf("case %s: playbook step %q is not a tool", id, st.Do)
			}
		}
	}
	if n == 0 {
		t.Fatal("no playbooks found")
	}
	cat, err := router.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range cat.Intents {
		for _, st := range in.Steps {
			tl, ok := reg.Get(st.Tool)
			switch {
			case !ok:
				t.Errorf("intent %s: %q is not a tool", in.ID, st.Tool)
			case tl.Tier() > toolkit.TierDiagnose:
				t.Errorf("intent %s: %s is a %s tool; local answers only read", in.ID, st.Tool, tl.Tier())
			}
		}
	}
}
