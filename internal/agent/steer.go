package agent

import "strings"

// Steer queues a message the operator typed while a turn is running. It
// reaches the model at its next step — appended to the latest tool
// result, which keeps the message structure valid for every provider —
// instead of waiting for the whole turn to end. Safe to call from any
// goroutine.
func (a *Agent) Steer(text string) {
	if text = strings.TrimSpace(text); text == "" {
		return
	}
	a.steerMu.Lock()
	a.steer = append(a.steer, text)
	a.steerMu.Unlock()
}

// TakeSteer returns (and clears) messages not yet delivered — the front
// end sends them as the next turn when the current one ends first.
func (a *Agent) TakeSteer() []string {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	s := a.steer
	a.steer = nil
	return s
}

// deliverSteer folds queued operator messages into the last tool result.
func (a *Agent) deliverSteer() {
	msgs := a.TakeSteer()
	if len(msgs) == 0 {
		return
	}
	i := len(a.history) - 1
	if i < 0 || a.history[i].Role != "tool" {
		a.Steer(strings.Join(msgs, "\n\n")) // nothing to attach to: keep for the next turn
		return
	}
	note := "\n\n[The operator, while you were working: " + a.Redact.Text(strings.Join(msgs, "\n\n")) +
		"]\nTake this into account now — it may change or stop what you were doing."
	a.history[i].Text += note
	a.emit(Event{Kind: EvNotice, Text: "your message reached the agent"})
}
