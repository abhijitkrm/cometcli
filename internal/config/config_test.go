package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COMETCLI_HOME", dir)

	cfg := &Config{Active: "v1", Profiles: map[string]*Profile{}}
	p := &Profile{
		Name: "v1", ChainID: "test_9000-1", EVMChainID: 9000,
		Bech32Prefix: "test", Role: "validator", Home: "~/.evmd", Binary: "evmd",
		Endpoints: Endpoints{Comet: "tcp://127.0.0.1:26657", GRPC: "127.0.0.1:9090"},
		Transport: Transport{Type: "ssh", Host: "10.0.0.1", User: "ops"},
		Service:   Service{Type: "systemd", Unit: "evmd.service"},
		Signer:    Signer{Backend: "file", Key: "ops"},
		Metadata:  map[string]string{"fee_denom": "atest"},
	}
	if err := cfg.UpsertProfile(p); err != nil {
		t.Fatal(err)
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	ap, err := got.ActiveProfile("")
	if err != nil {
		t.Fatal(err)
	}
	if ap.ChainID != "test_9000-1" || ap.Transport.Host != "10.0.0.1" || ap.Signer.Key != "ops" {
		t.Fatalf("roundtrip mismatch: %+v", ap)
	}
	// file perms
	fi, err := os.Stat(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %o, want 600", fi.Mode().Perm())
	}
}

func TestOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COMETCLI_HOME", dir)
	cfg := &Config{Active: "a", Profiles: map[string]*Profile{
		"a": {Name: "a", ChainID: "a-1"},
		"b": {Name: "b", ChainID: "b-1"},
	}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, _ := Load()
	p, err := got.ActiveProfile("b")
	if err != nil {
		t.Fatal(err)
	}
	if p.ChainID != "b-1" {
		t.Fatalf("override failed: %v", p.ChainID)
	}
}

func TestAgentForMergesAndFallsBack(t *testing.T) {
	c := &Config{
		Active: "v1",
		Agent:  AgentConf{Provider: "groq", Model: "openai/gpt-oss-120b", Effort: "low", Permissions: Permissions{Deny: []string{"bash(rm:*)"}}},
		Profiles: map[string]*Profile{
			"v1": {Agent: AgentConf{Model: "llama-3.3-70b-versatile", Permissions: Permissions{Allow: []string{"node.*"}}}},
			"v2": {Agent: AgentConf{Provider: "gemini"}},
		},
	}
	g := c.AgentFor(nil)
	if g.Provider != "groq" || g.Model != "openai/gpt-oss-120b" {
		t.Fatalf("general = %+v", g)
	}
	n := c.AgentFor(c.Profiles["v1"])
	if n.Provider != "groq" || n.Model != "llama-3.3-70b-versatile" || n.Effort != "low" {
		t.Fatalf("node override = %+v", n)
	}
	if len(n.Permissions.Deny) != 1 || len(n.Permissions.Allow) != 1 {
		t.Fatalf("rules not accumulated: %+v", n.Permissions)
	}
	if m := c.AgentFor(c.Profiles["v2"]); m.Provider != "gemini" || m.Model != "" {
		t.Fatalf("provider switch kept the old model: %+v", m)
	}
	// no global provider: general mode borrows the active profile's
	c.Agent = AgentConf{}
	c.Profiles["v1"].Agent.Provider = "gemini"
	if g := c.AgentFor(nil); g.Provider != "gemini" {
		t.Fatalf("fallback = %+v", g)
	}
}
