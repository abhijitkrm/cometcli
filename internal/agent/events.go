package agent

import (
	"fmt"
	"sync/atomic"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// EventKind enumerates what a front-end can render from an agent turn.
type EventKind string

const (
	// EvDelta is a streamed assistant text chunk (best-effort, may be
	// followed by an EvText that supersedes it).
	EvDelta EventKind = "delta"
	// EvText is the complete, redacted assistant text of one model round.
	// Front-ends that rendered deltas replace them with this.
	EvText EventKind = "text"
	// EvToolStart fires before a tool runs.
	EvToolStart EventKind = "tool_start"
	// EvToolResult fires after a tool runs (Err set on failure).
	EvToolResult EventKind = "tool_result"
	// EvThinking is streamed model reasoning (a summary, where the
	// provider exposes one). Front-ends may show it dimmed or hide it.
	EvThinking EventKind = "thinking"
	// EvNotice is a status line from the loop itself: continuation after
	// an output cutoff, compaction, a refusal, a failed session save.
	EvNotice EventKind = "notice"
	// EvTodos carries the agent's updated task checklist (Todos).
	EvTodos EventKind = "todos"
	// EvApproval asks the human to approve a gated action; answer it via
	// Approval.Answer. Only emitted when the context uses EventApprover.
	EvApproval EventKind = "approval"
)

// Event is one renderable step of an agent turn. It is JSON-serializable
// so `cometcli serve` can forward it over SSE verbatim.
type Event struct {
	Kind     EventKind      `json:"kind"`
	Text     string         `json:"text,omitempty"`
	Tool     string         `json:"tool,omitempty"`
	Tier     string         `json:"tier,omitempty"`
	Args     map[string]any `json:"args,omitempty"`
	Err      string         `json:"error,omitempty"`
	Approval *Approval      `json:"approval,omitempty"`
	Todos    []Todo         `json:"todos,omitempty"`
}

// Approval is a pending human decision. Answer it exactly once.
type Approval struct {
	ID     string         `json:"id"`
	Prompt string         `json:"prompt"`
	Tier   string         `json:"tier"`
	Detail map[string]any `json:"detail,omitempty"`
	reply  chan bool
}

// Answer delivers the decision; extra calls are ignored.
func (a *Approval) Answer(ok bool) {
	select {
	case a.reply <- ok:
	default:
	}
}

var approvalSeq atomic.Int64

// EventApprover returns a toolkit.Approver that surfaces each request as an
// EvApproval event via emit and blocks until it is answered or the tool's
// context is cancelled (cancellation denies).
func EventApprover(emit func(Event)) toolkit.Approver {
	return func(c *toolkit.Context, prompt string, tier toolkit.Tier, detail map[string]any) (bool, error) {
		ap := &Approval{
			ID:     fmt.Sprintf("ap-%d", approvalSeq.Add(1)),
			Prompt: prompt, Tier: tier.String(), Detail: detail,
			reply: make(chan bool, 1),
		}
		emit(Event{Kind: EvApproval, Tier: ap.Tier, Approval: ap})
		select {
		case ok := <-ap.reply:
			return ok, nil
		case <-c.Done():
			return false, c.Err()
		}
	}
}
