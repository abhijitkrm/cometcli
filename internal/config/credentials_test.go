package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COMETCLI_HOME", dir)
	if Credential("X_KEY") != "" {
		t.Fatal("unexpected key")
	}
	if err := SetCredential("X_KEY", "abc=123"); err != nil {
		t.Fatal(err)
	}
	_ = SetCredential("Y_KEY", "y")
	if got := Credential("X_KEY"); got != "abc=123" {
		t.Fatalf("got %q", got)
	}
	fi, _ := os.Stat(filepath.Join(dir, "credentials"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	_ = SetCredential("X_KEY", "")
	if Credential("X_KEY") != "" || Credential("Y_KEY") != "y" {
		t.Fatal("remove")
	}
	if SetCredential("BAD NAME", "v") == nil || SetCredential("K", "a\nb") == nil {
		t.Fatal("invalid accepted")
	}
}
