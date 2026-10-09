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
		"restart the node",
		"how do I add a peer",
		"vote yes on proposal 3",
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
