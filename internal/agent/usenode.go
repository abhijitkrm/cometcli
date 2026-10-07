package agent

import (
	"fmt"
	"sort"
	"strings"
)

const useNodeName = "use_node"

// useNodeDef lets the model move the session onto a node profile itself
// (or back to general), instead of asking the operator to type /one.
func (a *Agent) useNodeDef() (ToolDef, bool) {
	if a.parent != nil || a.Ctx == nil || a.Ctx.Cfg == nil || len(a.Ctx.Cfg.Profiles) == 0 {
		return ToolDef{}, false
	}
	names := []string{"off"}
	for n := range a.Ctx.Cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names[1:])
	return ToolDef{
		Name: useNodeName,
		Desc: "[observe] Work on a node: switch this session to a node profile (adds its node, chain, validator and incident tools, " +
			"safety rules and a live snapshot), or 'off' for general mode. Call it yourself whenever a question is about a node, " +
			"validator or chain — never ask the operator to switch. Any profile on a chain answers chain-wide questions.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"profile": map[string]any{"type": "string", "enum": names, "description": "profile name, or off"},
		}, "required": []string{"profile"}},
	}, true
}

func (a *Agent) useNode(call Call, args map[string]any) Msg {
	name := strings.TrimSpace(fmt.Sprint(args["profile"]))
	a.emit(Event{Kind: EvToolStart, Tool: useNodeName, Args: map[string]any{"profile": name}})
	if name == "off" || name == "" {
		a.switchScope(nil, "")
		a.emit(Event{Kind: EvToolResult, Tool: useNodeName, Text: "general mode"})
		return Msg{Role: "tool", CallID: call.ID, ToolName: useNodeName, Text: "now in general mode: shell, files and web on the operator's machine"}
	}
	p, ok := a.Ctx.Cfg.Profiles[name]
	if !ok {
		return a.toolErr(call, useNodeName, "no profile "+name)
	}
	p.Name = name
	a.switchScope(p, "")
	text := fmt.Sprintf("now working on %s (role %s, chain %s): node tools are callable from your next step — "+
		"node.triage for health/incidents, chain.validators (status=jailed lists jailed validators), val.* for this validator; "+
		"load others with tool_search", name, orNone(p.Role), orNone(p.ChainID))
	a.emit(Event{Kind: EvToolResult, Tool: useNodeName, Text: "node mode: " + name})
	return Msg{Role: "tool", CallID: call.ID, ToolName: useNodeName, Text: text}
}
