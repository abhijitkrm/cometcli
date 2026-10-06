package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) (home, proj string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COMETCLI_HOME", filepath.Join(home, ".cometcli"))
	proj = filepath.Join(home, "work", "node-setup")
	os.MkdirAll(filepath.Join(proj, ".cometcli", "commands", "ops"), 0o755)
	os.MkdirAll(filepath.Join(proj, "run-validator"), 0o755)
	os.MkdirAll(filepath.Join(home, ".cometcli"), 0o700)
	return home, proj
}

func write(t *testing.T, p, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMergesScopesAndGatesTrust(t *testing.T) {
	home, proj := setup(t)
	write(t, filepath.Join(home, ".cometcli", "settings.json"), `{"model":"m-user","permissions":{"allow":["bash(git status:*)"]},
		"env":{"FOO":"user","KEEP":"x"},"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"true"}]}]},
		"mcpServers":{"u":{"command":"u-srv"}}}`)
	write(t, filepath.Join(proj, ".cometcli", "settings.json"), `{"effort":"low","permissions":{"deny":["bash(rm:*)"],"defaultMode":"acceptEdits"},
		"hooks":{"Stop":[{"hooks":[{"type":"command","command":"evil"}]}]},"mcpServers":{"p":{"command":"p-srv"}}}`)
	write(t, filepath.Join(proj, ".cometcli", "settings.local.json"), `{"model":"m-local","env":{"FOO":"local"}}`)
	write(t, filepath.Join(proj, ".mcp.json"), `{"mcpServers":{"shared":{"type":"http","url":"https://x/mcp"}}}`)

	s, err := Load(filepath.Join(proj, "run-validator"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Root != proj || s.Model != "m-local" || s.Effort != "low" || s.Permissions.DefaultMode != "acceptEdits" {
		t.Fatalf("merge: root=%s model=%s effort=%s mode=%s", s.Root, s.Model, s.Effort, s.Permissions.DefaultMode)
	}
	if len(s.Permissions.Allow) != 1 || len(s.Permissions.Deny) != 1 || s.Env["FOO"] != "local" {
		t.Fatalf("accumulate: %+v %v", s.Permissions, s.Env)
	}
	// untrusted: project hooks and servers skipped, user ones kept
	if len(s.Hooks["Stop"]) != 0 || len(s.Hooks["PreToolUse"]) != 1 {
		t.Fatalf("hooks = %+v", s.Hooks)
	}
	if _, ok := s.MCPServers["p"]; ok {
		t.Fatal("untrusted project server loaded")
	}
	if _, ok := s.MCPServers["u"]; !ok || len(s.Ignored) != 3 {
		t.Fatalf("servers=%v ignored=%v", s.MCPServers, s.Ignored)
	}

	if err := Trust(proj, true); err != nil {
		t.Fatal(err)
	}
	s, _ = Load(filepath.Join(proj, "run-validator"))
	if !s.Trusted || len(s.Hooks["Stop"]) != 1 || s.MCPServers["shared"].URL != "https://x/mcp" || s.MCPServers["p"].Scope != ScopeProject {
		t.Fatalf("trusted load: %+v", s)
	}
	Trust(proj, false)
	if IsTrusted(proj) {
		t.Fatal("trust not revoked")
	}

	os.Setenv("KEEP", "already")
	s.ApplyEnv()
	if os.Getenv("KEEP") != "already" || os.Getenv("FOO") != "local" {
		t.Fatal("ApplyEnv overrode an existing var or missed one")
	}
	os.Unsetenv("FOO")
}

func TestEditFileKeepsUnknownKeys(t *testing.T) {
	home, _ := setup(t)
	p := filepath.Join(home, ".cometcli", "settings.json")
	write(t, p, `{"statusLine":{"type":"command"},"model":"x"}`)
	if err := EditFile(p, func(f *File) error {
		f.MCPServers = map[string]MCPServer{"g": {Command: "grafana"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "statusLine") || !strings.Contains(string(b), `"grafana"`) || !strings.Contains(string(b), `"model": "x"`) {
		t.Fatalf("file = %s", b)
	}
}

func TestMemoryOrderAndImports(t *testing.T) {
	home, proj := setup(t)
	write(t, filepath.Join(home, ".cometcli", "COMET.md"), "user rule")
	write(t, filepath.Join(proj, "COMET.md"), "project rule\n@docs/ports.md\n@config/priv_validator_key.json")
	write(t, filepath.Join(proj, "docs", "ports.md"), "p2p is 26656")
	write(t, filepath.Join(proj, "run-validator", "AGENTS.md"), "validator dir rule")
	write(t, filepath.Join(home, ".cometcli", "memory", "val01.md"), "val01 runs in docker")
	ms := LoadMemory(proj, filepath.Join(proj, "run-validator"), "val01")
	var scopes []string
	for _, m := range ms {
		scopes = append(scopes, m.Scope)
	}
	if strings.Join(scopes, ",") != "user,project,project,node" {
		t.Fatalf("scopes = %v", scopes)
	}
	r := RenderMemory(ms)
	for _, want := range []string{"user rule", "p2p is 26656", "validator dir rule", "val01 runs in docker", "import of key material refused"} {
		if !strings.Contains(r, want) {
			t.Errorf("memory missing %q", want)
		}
	}
	p := filepath.Join(proj, "COMET.md")
	AppendMemory(p, "always check peers first")
	b, _ := os.ReadFile(p)
	if !strings.HasSuffix(string(b), "\n- always check peers first\n") {
		t.Fatalf("append = %q", b)
	}
}

func TestCommandsLoadAndExpand(t *testing.T) {
	home, proj := setup(t)
	write(t, filepath.Join(home, ".cometcli", "commands", "status.md"), "user version")
	write(t, filepath.Join(proj, ".cometcli", "commands", "status.md"), "---\ndescription: Node status\nargument-hint: [profile]\nallowed-tools: Bash(docker ps:*), Read\n---\nCheck $ARGUMENTS ($1 then $2).\nContainers: !`docker ps`\nSee @notes.md")
	write(t, filepath.Join(proj, ".cometcli", "commands", "ops", "vote.md"), "# Vote on upgrade $1")
	cs := LoadCommands(proj)
	st := cs["status"]
	if st == nil || st.Scope != "project" || st.Description != "Node status" || st.ArgHint != "[profile]" {
		t.Fatalf("status = %+v", st)
	}
	if len(st.AllowedTools) != 2 || st.AllowedTools[0] != "Bash(docker ps:*)" {
		t.Fatalf("allowed-tools = %q", st.AllowedTools)
	}
	if cs["ops:vote"] == nil || cs["ops:vote"].Description != "Vote on upgrade $1" {
		t.Fatalf("namespaced = %+v", cs["ops:vote"])
	}
	out := st.Expand(`val01 "two words"`,
		func(c string) string { return "[ran " + c + "]" },
		func(p string) (string, bool) { return "NOTES", p == "notes.md" })
	for _, want := range []string{`Check val01 "two words" (val01 then two words).`, "[ran docker ps]", `<file path="notes.md">`, "NOTES"} {
		if !strings.Contains(out, want) {
			t.Errorf("expansion missing %q in %q", want, out)
		}
	}
}

func TestAgentsLoad(t *testing.T) {
	_, proj := setup(t)
	write(t, filepath.Join(proj, ".cometcli", "agents", "log-digger.md"), "---\nname: logs\ndescription: trawls logs\ntools: bash, grep, read\n---\nYou read logs and report anomalies.")
	as := LoadAgents(proj)
	d := as["logs"]
	if d == nil || d.Description != "trawls logs" || len(d.Tools) != 3 || !strings.Contains(d.Prompt, "anomalies") {
		t.Fatalf("agent = %+v", d)
	}
}
