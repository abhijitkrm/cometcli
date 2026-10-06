package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Deferred tool loading. Sending all ~65 tool schemas costs ~6.5k tokens
// per request — more than some providers' whole per-minute budget — so
// by default each request carries a core set, the system prompt lists the
// rest by name, and tool_search loads full definitions on demand. Loaded
// tools stay for the session (and are saved with it).

const toolSearchName = "tool_search"

// coreTools are always advertised (with the agent-only general tools).
var coreTools = map[string]bool{
	"node.status": true, "node.health": true, "node.logs": true,
	"val.status": true, "val.signing": true,
}

var toolSearchDef = ToolDef{
	Name: toolSearchName,
	Desc: "Load more tools so you can call them. Pass names from the tool catalog in the system prompt (\"select:val.unjail,chain.gov\") or keywords (\"governance proposals vote\"). Loaded tools are callable from your next step on.",
	Schema: toolkit.ObjSchema(map[string]any{
		"query": toolkit.Str(`"select:name1,name2" for exact tools, or keywords`),
	}, "query"),
}

// deferred reports whether this session loads tools on demand: node
// tools in node mode, MCP server tools in either mode.
func (a *Agent) deferred() bool {
	if a.conf.Tools == "all" {
		return false
	}
	if a.Node() {
		return true
	}
	for _, t := range a.Reg.All() {
		if isMCP(t) {
			return true
		}
	}
	return false
}

// deferrable reports whether t is listed in the catalog rather than sent.
func (a *Agent) deferrable(t toolkit.Tool) bool {
	if isMCP(t) {
		return true
	}
	return a.Node() && !toolkit.IsAgentOnly(t) && !coreTools[t.Name()] && !toolkit.IsLongRunning(t) && !toolkit.IsOperatorOnly(t) && strings.Contains(t.Name(), ".")
}

// advertised reports whether t goes into this request's tool list.
func (a *Agent) advertised(t toolkit.Tool) bool {
	if isMCP(t) {
		return a.conf.Tools == "all" || a.loaded[t.Name()]
	}
	if toolkit.IsAgentOnly(t) {
		return true
	}
	if !a.Node() {
		return false // node tools need a node: /one <profile>
	}
	if !a.deferred() || coreTools[t.Name()] {
		return true
	}
	return a.loaded[t.Name()]
}

// toolCatalog renders the not-yet-advertised tools by domain, names only,
// for the frozen system prompt.
func (a *Agent) toolCatalog() string {
	if !a.deferred() {
		return ""
	}
	groups := map[string][]string{}
	var order []string
	for _, t := range a.Reg.All() {
		if !a.deferrable(t) {
			continue
		}
		dom, verb, ok := strings.Cut(t.Name(), ".")
		if isMCP(t) {
			rest := strings.TrimPrefix(t.Name(), "mcp__")
			srv, tool, _ := strings.Cut(rest, "__")
			dom, verb, ok = "mcp__"+srv, tool, true
		}
		if !ok {
			continue
		}
		if _, seen := groups[dom]; !seen {
			order = append(order, dom)
		}
		groups[dom] = append(groups[dom], verb)
	}
	if len(order) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("TOOL CATALOG — load with tool_search before calling (e.g. select:val.unjail, select:mcp__server__tool):\n")
	for _, d := range order {
		fmt.Fprintf(&b, "- %s: %s\n", d, strings.Join(groups[d], ", "))
	}
	return b.String() + "\n"
}

func (a *Agent) toolSearch(call Call, args map[string]any) Msg {
	q := strings.TrimSpace(fmt.Sprint(args["query"]))
	if q == "" || q == "<nil>" {
		return a.toolErr(call, toolSearchName, "query is required")
	}
	var picks []toolkit.Tool
	var missing []string
	if names, ok := strings.CutPrefix(q, "select:"); ok {
		for _, n := range strings.Split(names, ",") {
			n = toolkit.ResolveName(strings.TrimSpace(n))
			if t, ok := a.Reg.Get(n); ok {
				picks = append(picks, t)
			} else if n != "" {
				missing = append(missing, n)
			}
		}
	} else {
		picks = a.searchTools(q, 5)
	}
	if a.loaded == nil {
		a.loaded = map[string]bool{}
	}
	var b strings.Builder
	var names []string
	for _, t := range picks {
		if toolkit.IsLongRunning(t) {
			continue
		}
		if !a.deferrable(t) && !coreTools[t.Name()] {
			missing = append(missing, t.Name()+" (node tool: needs node mode — /one <profile>)")
			continue
		}
		a.loaded[t.Name()] = true
		names = append(names, toolFnName(t.Name()))
		note := ""
		if !a.Policy.Allows(t.Tier()) && !toolkit.IsDynamic(t) {
			note = " (not callable in readonly mode)"
		}
		fmt.Fprintf(&b, "- %s [%s] %s%s\n", toolFnName(t.Name()), t.Tier(), t.Desc(), note)
	}
	if len(names) == 0 {
		msg := "no tools matched " + q + " — use names from the catalog"
		if len(missing) > 0 {
			msg = "unknown tools: " + strings.Join(missing, ", ")
		}
		return a.toolErr(call, toolSearchName, msg)
	}
	text := "loaded: " + strings.Join(names, ", ") + " — callable from your next step\n" + b.String()
	if len(missing) > 0 {
		text += "unknown: " + strings.Join(missing, ", ") + "\n"
	}
	a.emit(Event{Kind: EvToolResult, Tool: toolSearchName, Text: "loaded " + strings.Join(names, ", ")})
	return Msg{Role: "tool", CallID: call.ID, ToolName: toolSearchName, Text: strings.TrimRight(text, "\n")}
}

// searchTools ranks tools by keyword hits in name (weighted) and
// description.
func (a *Agent) searchTools(q string, n int) []toolkit.Tool {
	words := strings.Fields(strings.ToLower(q))
	type hit struct {
		t     toolkit.Tool
		score int
	}
	var hits []hit
	for _, t := range a.Reg.All() {
		if !a.deferrable(t) && !coreTools[t.Name()] {
			continue
		}
		name, desc := strings.ToLower(t.Name()), strings.ToLower(t.Desc())
		s := 0
		for _, w := range words {
			if len(w) < 3 && name != w {
				continue
			}
			if strings.Contains(name, w) {
				s += 3
			}
			if strings.Contains(desc, w) {
				s++
			}
		}
		if s > 0 {
			hits = append(hits, hit{t, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var out []toolkit.Tool
	for i := 0; i < len(hits) && i < n; i++ {
		out = append(out, hits[i].t)
	}
	return out
}

// LoadedTools returns the names loaded with tool_search, sorted.
func (a *Agent) LoadedTools() []string {
	var out []string
	for n := range a.loaded {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ToolNames lists the tools the next request would advertise.
func (a *Agent) ToolNames() []string {
	var out []string
	for _, d := range a.toolDefs() {
		out = append(out, d.Name)
	}
	return out
}
