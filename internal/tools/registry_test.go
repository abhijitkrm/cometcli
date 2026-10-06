package tools

import (
	"encoding/json"
	"strings"
	"testing"

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
