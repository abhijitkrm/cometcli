package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAnthropicEffortOnlyWhereSupported(t *testing.T) {
	a := &anthropic{base: "https://api.anthropic.com"}
	for model, want := range map[string]bool{
		"claude-haiku-4-5-20251001": false, "claude-sonnet-4-5": false,
		"claude-opus-4-5": true, "claude-sonnet-4-6": true, "claude-opus-5-5": true, "claude-sonnet-5-5": true, "claude-fable-5-1": true,
	} {
		_, has := a.body(&Request{Model: model, Effort: "low"}, false)["output_config"]
		if has != want {
			t.Errorf("%s: effort sent = %v, want %v", model, has, want)
		}
	}
}

func TestAnthropicRetriesWithoutEffortOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if _, has := body["output_config"]; has {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"This model does not support the effort parameter."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"4 validators"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`))
	}))
	defer srv.Close()
	a := &anthropic{base: srv.URL}
	// a model the pattern thinks supports effort, but the API says no
	res, err := a.Chat(context.Background(), &Request{Model: "claude-opus-5-5", Effort: "low", Messages: []Msg{{Role: "user", Text: "hi"}}})
	if err != nil || !strings.Contains(res.Text, "4 validators") {
		t.Fatalf("%v %v", res, err)
	}
	if calls.Load() != 2 || !a.noEffort {
		t.Fatalf("calls=%d noEffort=%v", calls.Load(), a.noEffort)
	}
	// later requests skip it straight away
	_, _ = a.Chat(context.Background(), &Request{Model: "claude-opus-5-5", Effort: "low", Messages: []Msg{{Role: "user", Text: "hi"}}})
	if calls.Load() != 3 {
		t.Fatalf("retried again: calls=%d", calls.Load())
	}
}
