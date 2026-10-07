package kb

import (
	"fmt"
	"strconv"
	"strings"
)

// Signals are the facts triage collected, by name: "node.peers",
// "val.jailed", "host.disk_used_pct"… Values are bool, float64 or string.
type Signals map[string]any

// Cond is one comparison: <signal> <op> <value>.
type Cond struct {
	Raw    string
	Signal string
	Op     string // == != > >= < <= contains exists missing
	Value  any    // bool, float64 or string
}

var ops = []string{">=", "<=", "==", "!=", ">", "<", " contains ", " exists", " missing"}

// ParseCond parses "val.jailed == true", "host.disk_used_pct >= 90",
// `logs.panic > 0`, "evm.endpoint exists", `config.mempool_type != "app"`.
func ParseCond(s string) (Cond, error) {
	raw := strings.TrimSpace(s)
	for _, op := range ops {
		i := strings.Index(raw, op)
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(raw[:i])
		rest := strings.TrimSpace(raw[i+len(op):])
		op = strings.TrimSpace(op)
		if strings.ContainsAny(name, " \t") {
			return Cond{}, fmt.Errorf("condition %q: bad signal name %q", raw, name)
		}
		c := Cond{Raw: raw, Signal: name, Op: op}
		if op == "exists" || op == "missing" {
			if rest != "" {
				return Cond{}, fmt.Errorf("condition %q: %s takes no value", raw, op)
			}
			return c, nil
		}
		if rest == "" {
			return Cond{}, fmt.Errorf("condition %q: missing value", raw)
		}
		c.Value = literal(rest)
		return c, nil
	}
	return Cond{}, fmt.Errorf("condition %q: want <signal> <op> <value> with op one of == != > >= < <= contains exists missing", raw)
}

func literal(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return strings.Trim(s, `"'`)
}

// Eval reports whether the condition holds. A signal that wasn't
// collected never holds (except for "missing").
func (c Cond) Eval(s Signals) bool {
	v, ok := s[c.Signal]
	switch c.Op {
	case "exists":
		return ok
	case "missing":
		return !ok
	}
	if !ok || v == nil {
		return false
	}
	switch c.Op {
	case "contains":
		return strings.Contains(strings.ToLower(fmt.Sprint(v)), strings.ToLower(fmt.Sprint(c.Value)))
	case "==", "!=":
		eq := equal(v, c.Value)
		if c.Op == "==" {
			return eq
		}
		return !eq
	}
	a, aok := num(v)
	b, bok := num(c.Value)
	if !aok || !bok {
		return false
	}
	switch c.Op {
	case ">":
		return a > b
	case ">=":
		return a >= b
	case "<":
		return a < b
	case "<=":
		return a <= b
	}
	return false
}

func equal(a, b any) bool {
	if x, ok := num(a); ok {
		if y, ok := num(b); ok {
			return x == y
		}
	}
	if x, ok := a.(bool); ok {
		y, ok := b.(bool)
		return ok && x == y
	}
	return strings.EqualFold(fmt.Sprint(a), fmt.Sprint(b))
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}
