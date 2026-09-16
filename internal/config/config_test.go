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
