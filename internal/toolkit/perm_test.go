package toolkit

import (
	"context"
	"strings"
	"testing"
)

func TestRulesDecide(t *testing.T) {
	r, err := NewRules(
		[]string{"Bash(git status:*)", "bash(systemctl status *)", "read(./config/**)", "WebFetch(domain:github.com)", "node.*"},
		[]string{"bash(docker compose:*)"},
		[]string{"bash(rm:*)", "edit(/etc/**)", "val.unjail"},
	)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		req  Request
		want Decision
	}{
		{Request{Tool: "bash", Kind: "command", Specs: []string{"git status"}}, DecideAllow},
		{Request{Tool: "bash", Kind: "command", Specs: []string{"git status --short"}}, DecideAllow},
		{Request{Tool: "bash", Kind: "command", Specs: []string{"git statusx"}}, DecideDefault},
		{Request{Tool: "bash", Kind: "command", Specs: []string{"systemctl status evmd"}}, DecideAllow},
		// compound: allow must cover every segment
		{Request{Tool: "bash", Kind: "command", Specs: []string{"git status", "ls"}}, DecideDefault},
		// a single deny or ask segment decides
		{Request{Tool: "bash", Kind: "command", Specs: []string{"git status", "rm -rf x"}}, DecideDeny},
		{Request{Tool: "bash", Kind: "command", Specs: []string{"docker compose up -d"}}, DecideAsk},
		{Request{Tool: "read", Kind: "path", Root: "/srv/node", Specs: []string{"/srv/node/config/app.toml"}}, DecideAllow},
		{Request{Tool: "read", Kind: "path", Root: "/srv/node", Specs: []string{"/srv/node/data/x"}}, DecideDefault},
		{Request{Tool: "edit", Kind: "path", Specs: []string{"/etc/hosts"}}, DecideDeny},
		{Request{Tool: "web_fetch", Kind: "url", Specs: []string{"https://api.github.com/repos/x"}}, DecideAllow},
		{Request{Tool: "web_fetch", Kind: "url", Specs: []string{"https://evilgithub.com/"}}, DecideDefault},
		{Request{Tool: "node.status"}, DecideAllow},
		{Request{Tool: "val.unjail"}, DecideDeny},
		{Request{Tool: "val.status"}, DecideDefault},
	}
	for _, c := range cases {
		if got, rule := r.Decide(c.req); got != c.want {
			t.Errorf("%s %v: got %s (%s), want %s", c.req.Tool, c.req.Specs, got, rule, c.want)
		}
	}
	var nilRules *Rules
	if d, _ := nilRules.Decide(Request{Tool: "bash"}); d != DecideDefault {
		t.Fatal("nil rules must be default")
	}
	if _, err := ParseRule("bash(unclosed"); err == nil {
		t.Fatal("bad rule accepted")
	}
}

func TestMatchPathGlob(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"/a/**", "/a/b/c.toml", true},
		{"/a/**/*.toml", "/a/c.toml", true},
		{"/a/**/*.toml", "/a/b/c/d.toml", true},
		{"/a/*.toml", "/a/b/c.toml", false},
		{"/a/?.go", "/a/x.go", true},
		{"/a/b", "/a/b/../b", true},
	}
	for _, c := range cases {
		if got := MatchPathGlob(c.pat, c.path); got != c.want {
			t.Errorf("%s ~ %s = %v", c.pat, c.path, got)
		}
	}
}

func TestCheckPipeline(t *testing.T) {
	var asked []string
	appr := func(_ *Context, prompt string, tier Tier, _ map[string]any) (bool, error) {
		asked = append(asked, prompt)
		return true, nil
	}
	rules, _ := NewRules([]string{"bash(evmd tx:*)", "bash(make:*)"}, nil, []string{"bash(curl:*)"})
	c := &Context{Context: context.Background(), Approver: appr, AutoApproveBelow: TierLocalChange, Rules: rules}
	g := func(spec string, tier Tier) Gate {
		return Gate{Request: Request{Tool: "bash", Kind: "command", Specs: []string{spec}}, Tier: tier, Prompt: spec}
	}
	if err := c.Check(g("ls", TierDiagnose)); err != nil || len(asked) != 0 {
		t.Fatalf("read asked or failed: %v %v", err, asked)
	}
	if err := c.Check(g("make build", TierLocalChange)); err != nil || len(asked) != 0 {
		t.Fatalf("allow rule ignored: %v %v", err, asked)
	}
	// an allow rule never skips the tx prompt
	if err := c.Check(g("evmd tx gov vote 1 yes", TierOnChain)); err != nil || len(asked) != 1 {
		t.Fatalf("on-chain not asked: %v %v", err, asked)
	}
	if err := c.Check(g("curl x", TierDiagnose)); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("deny rule ignored: %v", err)
	}
	if err := c.Check(Gate{Tier: TierDiagnose, Forbidden: "keys"}); err == nil {
		t.Fatal("forbidden passed")
	}
	c.ReadOnly = true
	if err := c.Check(g("make build", TierLocalChange)); err == nil {
		t.Fatal("readonly allowed a change despite allow rule")
	}
	c.ReadOnly, c.AcceptEdits = false, true
	asked = nil
	if err := c.Check(Gate{Tier: TierLocalChange, InRoot: true, Prompt: "edit"}); err != nil || len(asked) != 0 {
		t.Fatalf("accept-edits asked: %v", asked)
	}
	if err := c.Check(Gate{Tier: TierLocalChange, Prompt: "edit outside"}); err != nil || len(asked) != 1 {
		t.Fatalf("out-of-root edit not asked: %v", asked)
	}
	asked = nil
	if err := c.Check(Gate{Tier: TierDiagnose, AskByDefault: true, Prompt: "fetch"}); err != nil || len(asked) != 1 {
		t.Fatalf("ask-by-default not asked: %v", asked)
	}
}

func TestSessionFreshness(t *testing.T) {
	s := NewSession("/x")
	if err := s.CheckFresh("k", []byte("a")); err == nil {
		t.Fatal("unread file passed")
	}
	s.MarkRead("k", []byte("a"))
	if err := s.CheckFresh("k", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckFresh("k", []byte("b")); err == nil {
		t.Fatal("changed file passed")
	}
	var nilS *Session
	if nilS.Cwd("d") != "d" || nilS.CheckFresh("k", nil) != nil {
		t.Fatal("nil session not permissive")
	}
}
