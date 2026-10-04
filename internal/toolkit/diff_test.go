package toolkit

import (
	"strings"
	"testing"
)

func TestDiff(t *testing.T) {
	before := "a\nb\npersistent_peers = \"x\"\nc\nd\ne\nf"
	after := "a\nb\npersistent_peers = \"x,y\"\nc\nd\ne\nf"
	d := Diff("config.toml", before, after)
	for _, want := range []string{"--- config.toml", `- persistent_peers = "x"`, `+ persistent_peers = "x,y"`, "  b", "  c"} {
		if !strings.Contains(d, want) {
			t.Fatalf("diff missing %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "  f") {
		t.Fatalf("diff should trim distant context:\n%s", d)
	}
	if Diff("x", "same", "same") != "" {
		t.Fatal("no-op edit must produce empty diff")
	}
}

func TestObjSchemaNoNulls(t *testing.T) {
	s := ObjSchema(nil)
	if _, ok := s["required"]; ok {
		t.Fatal("empty required must be omitted, not null")
	}
	if p, ok := s["properties"].(map[string]any); !ok || p == nil {
		t.Fatal("nil properties must become {}")
	}
	if r := ObjSchema(map[string]any{"a": Str("x")}, "a")["required"].([]string); len(r) != 1 {
		t.Fatal("required lost")
	}
}
