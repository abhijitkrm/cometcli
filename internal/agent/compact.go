package agent

import (
	"context"
	"fmt"
	"strings"
)

// compactPrompt asks the model to summarize the session so far. The
// summary replaces the whole history ("simple compaction"): nothing from
// before it is replayed, so no provider-bound state (thinking blocks,
// thought signatures) survives to be invalidated.
const compactPrompt = `Summarize this conversation so it can continue from the summary alone — earlier messages and tool output will be discarded.

Do not call any tools. Reply with the summary only, using these sections:
1. Goal — what the operator asked for, in their words where it matters.
2. Findings — facts established, with exact values (heights, versions, hosts, paths, error strings, tx hashes).
3. Actions taken — commands and tools run, changes made, and their results.
4. Open items — what is still pending, blocked, or awaiting approval.
5. Next step — what you were about to do.`

// contextWindow returns the model's context size in tokens.
func (a *Agent) contextWindow() int {
	if a.conf.ContextWindow > 0 {
		return a.conf.ContextWindow
	}
	m := strings.ToLower(a.Model)
	switch a.Provider.Name() {
	case "anthropic":
		if strings.Contains(m, "haiku") || strings.Contains(m, "-3-") || strings.Contains(m, "-4-5") || strings.Contains(m, "-4-1") || strings.Contains(m, "-4-0") {
			return 200_000
		}
		return 1_000_000
	case "gemini":
		return 1_000_000
	case "openai":
		if strings.HasPrefix(m, "gpt-4.1") {
			return 1_000_000
		}
		return 128_000
	case "groq":
		return 128_000
	}
	return 32_000 // local models: conservative
}

// compactThreshold is the prompt size that triggers auto-compaction.
func (a *Agent) compactThreshold() int {
	if a.CompactAt > 0 {
		return a.CompactAt
	}
	t := a.contextWindow() * 8 / 10
	if t > 200_000 {
		t = 200_000 // bound per-request cost on 1M-token models
	}
	return t
}

// promptTokens is the latest prompt size: reported by the provider when
// available, otherwise estimated at ~4 bytes per token.
func (a *Agent) promptTokens() int {
	if a.lastIn > 0 {
		return a.lastIn
	}
	n := len(a.sys)
	for _, m := range a.history {
		n += len(m.Text) + len(m.RawSteps)
		for _, c := range m.Calls {
			n += len(c.Args) + len(c.Name)
		}
	}
	return n / 4
}

func (a *Agent) needsCompact() bool {
	return len(a.history) >= 2 && a.promptTokens() >= a.compactThreshold()
}

// Compact summarizes the conversation and replaces the history with the
// summary, which is attached to the next user turn. instructions, when
// set, tell the summarizer what to focus on (/compact <instructions>).
func (a *Agent) Compact(ctx context.Context, instructions string) error {
	return a.compactNow(ctx, instructions, false)
}

// compactNow does the work. midTurn means a tool round is in progress:
// the summary becomes a user message asking the model to carry on.
func (a *Agent) compactNow(ctx context.Context, instructions string, midTurn bool) error {
	if len(a.history) == 0 {
		return fmt.Errorf("nothing to compact")
	}
	prompt := compactPrompt
	if instructions != "" {
		prompt += "\n\nOperator focus for this summary: " + a.Redact.Text(instructions)
	}
	hist := append([]Msg(nil), a.history...)
	// a summarizer turn must follow a complete exchange: a trailing user
	// message would make two user turns in a row on some providers, so
	// fold the prompt into it
	if n := len(hist); n > 0 && hist[n-1].Role == "user" {
		hist[n-1].Text += "\n\n" + prompt
	} else {
		hist = append(hist, Msg{Role: "user", Text: prompt})
	}
	// same system + tools as the session, so the summarizer reads the
	// cached prefix; the prompt forbids tool calls and any are ignored
	req := &Request{
		Model: a.Model, System: a.system(), Messages: hist,
		Tools: a.toolDefs(), MaxTok: 8192, Effort: a.Effort,
	}
	resp, err := a.Provider.Chat(ctx, req)
	if err != nil {
		return err
	}
	a.total.Add(resp.Usage)
	summary := strings.TrimSpace(a.Redact.Text(resp.Text))
	if summary == "" {
		return fmt.Errorf("model returned an empty summary")
	}
	block := "<conversation-summary>\n" + summary + "\n</conversation-summary>"
	a.history, a.lastIn = nil, 0
	if midTurn {
		a.history = []Msg{{Role: "user", Text: block + "\n\nContinue the task in progress from this summary."}}
	} else {
		a.carry = block
	}
	if lg := a.Audit(); lg != nil {
		_ = lg.Log("compact", a.profileName(), map[string]any{"summary": summary, "mid_turn": midTurn})
	}
	a.emit(Event{Kind: EvNotice, Text: fmt.Sprintf("conversation compacted (%d-char summary)", len(summary))})
	return nil
}

// isContextOverflow recognizes "prompt too long" errors across providers.
func isContextOverflow(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "prompt is too long") ||
		strings.Contains(s, "context_length_exceeded") ||
		strings.Contains(s, "context length") ||
		strings.Contains(s, "context window") ||
		strings.Contains(s, "maximum context") ||
		strings.Contains(s, "request too large") || // Groq 413: prompt exceeds the per-minute token budget
		(strings.Contains(s, "too many tokens") && strings.Contains(s, "input"))
}
