package agent

import (
	"fmt"
	"strings"
)

// Todo is one item of the agent's task list (todo_write).
type Todo struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

const todoToolName = "todo_write"

// todoToolDef is a built-in tool: the agent keeps a visible checklist for
// multi-step work (an upgrade, a migration) and updates it as it goes.
var todoToolDef = ToolDef{
	Name: todoToolName,
	Desc: "Replace the session's task checklist, shown to the operator. Use it for work with 3+ steps: list the steps, keep exactly one in_progress, mark each completed as soon as it is done. Send the full list every time.",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"todos": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{"type": "string", "description": "the step, imperative"},
						"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
					},
					"required": []string{"content", "status"},
				},
			},
		},
		"required": []string{"todos"},
	},
}

// Todos returns the current checklist.
func (a *Agent) Todos() []Todo { return append([]Todo(nil), a.todos...) }

func (a *Agent) todoWrite(call Call, args map[string]any) Msg {
	raw, _ := args["todos"].([]any)
	var todos []Todo
	for _, r := range raw {
		m, _ := r.(map[string]any)
		c, _ := m["content"].(string)
		s, _ := m["status"].(string)
		if strings.TrimSpace(c) == "" {
			continue
		}
		switch s {
		case "pending", "in_progress", "completed":
		default:
			return a.toolErr(call, todoToolName, fmt.Sprintf("bad status %q — want pending, in_progress or completed", s))
		}
		todos = append(todos, Todo{Content: a.Redact.Text(c), Status: s})
	}
	a.todos = todos
	a.emit(Event{Kind: EvTodos, Todos: a.Todos()})
	done := 0
	for _, t := range todos {
		if t.Status == "completed" {
			done++
		}
	}
	return Msg{Role: "tool", CallID: call.ID, ToolName: todoToolName, Text: fmt.Sprintf("checklist updated: %d/%d done", done, len(todos))}
}

// RenderTodos draws the checklist as plain lines.
func RenderTodos(todos []Todo) string {
	var b strings.Builder
	for _, t := range todos {
		mark := "☐"
		switch t.Status {
		case "in_progress":
			mark = "▸"
		case "completed":
			mark = "☒"
		}
		fmt.Fprintf(&b, "%s %s\n", mark, t.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}
