package router

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	local := map[string]string{
		"is anyone jailed?":               "jailed-validators",
		"Any validators jailed":           "jailed-validators",
		"what is the validator count":     "validator-count",
		"how many validators?":            "validator-count",
		"How many peers do I have?":       "node-peers",
		"peers":                           "node-peers",
		"is my node synced?":              "node-status",
		"status":                          "node-status",
		"is my validator jailed?":         "val-me",
		"am I jailed":                     "val-me",
		"open proposals":                  "proposals",
		"are there any open proposals?":   "proposals",
		"is there an upgrade scheduled?":  "upgrade-plan",
		"what is my uptime":               "val-signing",
		"do I need to vote?":              "val-pending-votes",
		"what's the signed blocks window": "slashing-params",
		"am I signing":                    "val-consensus",
		"my balance":                      "balance",
	}
	for q, want := range local {
		m := c.Classify(q)
		if m.Intent == nil || m.Intent.ID != want {
			got := "model"
			if m.Intent != nil {
				got = m.Intent.ID
			}
			t.Errorf("%q → %s (%s), want %s", q, got, m.Why, want)
		}
	}
	for _, q := range []string{
		"why is my validator jailed?",
		"unjail my validator",
		"how do I add a peer",
		"is my validator jailed and why did it happen",
		"explain the slashing params",
		"the node keeps restarting every few seconds, nothing in the logs",
		"what's the weather like",
		"check disk usage on the host",
		"compare peers with yesterday",
		"is anyone jailed\nalso check peers",
	} {
		if m := c.Classify(q); m.Intent != nil {
			t.Errorf("%q should go to the model, routed to %s (%s)", q, m.Intent.ID, m.Why)
		}
	}
}

func TestRender(t *testing.T) {
	c, _ := Load()
	var in *Intent
	for _, x := range c.Intents {
		if x.ID == "jailed-validators" {
			in = x
		}
	}
	out, err := in.Render("val1", map[string]Result{"j": {Text: "no validator is jailed\ntotal: 0\n", Data: map[string]any{"count": 0}}})
	if err != nil || !strings.HasPrefix(out, "**No validator is jailed.**") {
		t.Fatalf("%q %v", out, err)
	}
	out, _ = in.Render("val1", map[string]Result{"j": {Text: "val3 JAILED\ntotal: 1\n", Data: map[string]any{"count": 1}}})
	if !strings.HasPrefix(out, "**1 validator(s) jailed:**") {
		t.Fatalf("%q", out)
	}
}

func TestCacheExpires(t *testing.T) {
	var c Cache
	now := time.Now()
	c.Put("k", "v", 30*time.Second, now)
	if v, ok := c.Get("k", now.Add(10*time.Second)); !ok || v != "v" {
		t.Fatal("fresh entry missing")
	}
	if _, ok := c.Get("k", now.Add(31*time.Second)); ok {
		t.Fatal("stale entry served")
	}
}

func TestUserIntentsOverride(t *testing.T) {
	d := t.TempDir()
	_ = writeFile(d+"/mine.yaml", "- id: node-peers\n  examples: [peer roster]\n  steps: [{tool: node.peers}]\n  answer: custom\n")
	c, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if m := c.Classify("peer roster"); m.Intent == nil || m.Intent.Answer != "custom" {
		t.Fatalf("user intent should override the built-in: %+v", m)
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o644) }

func TestCommands(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		prompt, id string
		args       map[string]string
	}{
		{"restart the node", "service", map[string]string{"action": "restart"}},
		{"Restart val3.", "service", map[string]string{"action": "restart", "node": "val3"}},
		{"stop sohan1", "service", map[string]string{"action": "stop", "node": "sohan1"}},
		{"restart val3 please", "service", map[string]string{"action": "restart", "node": "val3"}},
		{"Please restart val3 now, thanks", "service", map[string]string{"action": "restart", "node": "val3"}},
		{"unjail", "unjail", map[string]string{}},
		{"vote yes on proposal #6", "vote", map[string]string{"option": "yes", "proposal": "6"}},
		{"vote no_with_veto on 7 from val2", "vote", map[string]string{"option": "no_with_veto", "proposal": "7", "node": "val2"}},
		{"send 100adex to cosmos1mkns78vtva88s3mxclthvur56qssh22c3735cu", "send", map[string]string{"amount": "100adex", "to": "cosmos1mkns78vtva88s3mxclthvur56qssh22c3735cu"}},
		{"withdraw rewards and commission", "withdraw", map[string]string{"commission": " and commission"}},
		{"show last 200 logs of val4", "logs", map[string]string{"lines": "200", "node": "val4"}},
		{"set app mempool.max-txs 0", "set-config", map[string]string{"file": "app", "key": "mempool.max-txs", "value": "0"}},
		{"build v0.7.3 on val2", "build", map[string]string{"tag": "v0.7.3", "node": "val2"}},
		{"switch to primium-evm:v0.7.3", "switch", map[string]string{"image": "primium-evm:v0.7.3"}},
		{"check the network for production", "network-check", map[string]string{"prod": "for production"}},
	}
	for _, x := range cases {
		m := c.Classify(x.prompt)
		if m.Intent == nil || m.Intent.ID != x.id {
			t.Errorf("%q → %v (%s), want %s", x.prompt, m.Intent, m.Why, x.id)
			continue
		}
		for k, v := range x.args {
			if m.Args[k] != v {
				t.Errorf("%q: %s = %q, want %q", x.prompt, k, m.Args[k], v)
			}
		}
	}
	// questions can carry arguments too
	for q, id := range map[string]string{"show proposal 7": "7", "Did proposal #12 pass?": "12", "proposal 3 tally": "3"} {
		m := c.Classify(q)
		if m.Intent == nil || m.Intent.ID != "proposal" || m.Args["id"] != id {
			t.Errorf("%q → %v %v (%s), want proposal id %s", q, m.Intent, m.Args, m.Why, id)
		}
	}
	if m := c.Classify("open proposals"); m.Intent == nil || m.Intent.ID != "proposals" {
		t.Errorf("open proposals → %v", m.Intent)
	}
	// near-misses go to the model: it works out what was meant
	for _, q := range []string{"restart it if it's stuck", "vote the way the others did", "send some tokens to the treasury", "why did it stop"} {
		if m := c.Classify(q); m.Intent != nil && m.Intent.Kind == "command" {
			t.Errorf("%q must not be a command (matched %s)", q, m.Intent.ID)
		}
	}
	if got := Fill(map[string]any{"proposal": "{proposal}", "memo": "{memo}", "n": 3}, map[string]string{"proposal": "6"}); got["proposal"] != "6" || got["memo"] != nil || got["n"] != 3 {
		t.Errorf("Fill = %v", got)
	}
}
