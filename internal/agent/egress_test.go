package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cosmos/go-bip39"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// providerSaw is everything sent to the provider, as one string.
func providerSaw(p *mockProvider) string { return strings.Join(p.sent, "\n") }

func TestStrictProfilesSendSummariesNotRawOutput(t *testing.T) {
	tool := stubTool{name: "node.logs", tier: toolkit.TierDiagnose, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		return &toolkit.Result{Text: "ERR dial tcp 10.20.30.40:26656: i/o timeout\nERR dial tcp 10.20.30.41:26656: i/o timeout\n"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "node__logs", Args: json.RawMessage(`{}`)}}},
		{Text: "ok", Done: true},
	}}
	a := newTestAgent(t, prov, tool) // a validator profile: strict by default
	var shown string
	a.OnEvent = func(e Event) {
		if e.Kind == EvToolResult {
			shown = e.Output
		}
	}
	a.Run(context.Background(), "logs")
	sent := providerSaw(prov)
	if strings.Contains(sent, "10.20.30.4") || !strings.Contains(sent, "local summary") || !strings.Contains(sent, "(×2)") {
		t.Fatalf("model should see only the local summary:\n%s", sent)
	}
	if !strings.Contains(shown, "10.20.30.40") {
		t.Errorf("the operator's UI keeps the raw output, got %q", shown)
	}
}

func TestKeyMaterialNeverReachesTheProvider(t *testing.T) {
	ent, _ := bip39.NewEntropy(256)
	mn, _ := bip39.NewMnemonic(ent)
	tool := stubTool{name: "node.config", tier: toolkit.TierDiagnose, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		// redaction doesn't know this shape; the gate must stop it
		return &toolkit.Result{Text: "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ\n-----END OPENSSH PRIVATE KEY-----"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "node__config", Args: json.RawMessage(`{}`)}}},
		{Text: "ok", Done: true},
	}}
	a := newTestAgent(t, prov, tool)
	a.conf.Egress = "filtered" // the gate applies in every mode
	var notices []string
	a.OnEvent = func(e Event) {
		if e.Kind == EvNotice {
			notices = append(notices, e.Text)
		}
	}
	if _, err := a.Run(context.Background(), "show the deploy key"); err != nil {
		t.Fatal(err)
	}
	sent := providerSaw(prov)
	if strings.Contains(sent, "b3BlbnNzaC1rZXktdjEAAAAABG5vbmU") || !strings.Contains(sent, "withheld by cometcli") {
		t.Fatalf("private key reached the provider:\n%s", sent)
	}
	if len(notices) == 0 {
		t.Error("the operator should be told something was withheld")
	}

	// a mnemonic typed by the operator isn't sent at all
	before := len(prov.sent)
	if _, err := a.Run(context.Background(), "restore my key: "+mn); err == nil || !strings.Contains(err.Error(), "key material") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if len(prov.sent) != before || strings.Contains(providerSaw(prov), strings.Fields(mn)[5]+" "+strings.Fields(mn)[6]) {
		t.Fatal("a message with a mnemonic must not be sent")
	}
}
