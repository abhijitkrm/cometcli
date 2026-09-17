package host

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// Live SSH transport test. Gated on COMETCLI_SSH_TEST_* env vars pointing
// at a reachable sshd (e.g. a local container).
//
//	COMETCLI_SSH_TEST_HOST=127.0.0.1 COMETCLI_SSH_TEST_PORT=2222 \
//	COMETCLI_SSH_TEST_USER=ops COMETCLI_SSH_TEST_KEY=/tmp/cometcli_testkey \
//	go test ./internal/client/host -run SSHLive -v
func sshTestTarget(t *testing.T) *config.Profile {
	t.Helper()
	h := os.Getenv("COMETCLI_SSH_TEST_HOST")
	if h == "" {
		t.Skip("set COMETCLI_SSH_TEST_HOST/PORT/USER/KEY for live ssh test")
	}
	p := &config.Profile{
		Name: "ssh-test",
		Transport: config.Transport{
			Type:    "ssh",
			Host:    h,
			User:    os.Getenv("COMETCLI_SSH_TEST_USER"),
			KeyFile: os.Getenv("COMETCLI_SSH_TEST_KEY"),
		},
	}
	if port := os.Getenv("COMETCLI_SSH_TEST_PORT"); port != "" {
		var n int
		if _, err := fmt.Sscan(port, &n); err == nil {
			p.Transport.Port = n
		}
	}
	return p
}

func TestSSHLive_Run(t *testing.T) {
	h, err := Connect(context.Background(), sshTestTarget(t))
	if err != nil {
		t.Fatal(err)
	}
	defer h.(interface{ Close() error }).Close()

	out, code, err := h.Run(context.Background(), "echo ssh-ok && id -u")
	if err != nil || code != 0 {
		t.Fatalf("run: code=%d err=%v", code, err)
	}
	if !strings.Contains(out, "ssh-ok") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestSSHLive_ReadWriteStat(t *testing.T) {
	h, err := Connect(context.Background(), sshTestTarget(t))
	if err != nil {
		t.Fatal(err)
	}
	defer h.(interface{ Close() error }).Close()
	ctx := context.Background()

	payload := []byte("cometcli ssh write test\nline2\n")
	if err := h.WriteFile(ctx, "/tmp/cometcli-wtest", payload, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := h.ReadFile(ctx, "/tmp/cometcli-wtest")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("read = %q, want %q", got, payload)
	}
	fi, err := h.Stat(ctx, "/tmp/cometcli-wtest")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600", fi.Mode().Perm())
	}
	if fi.Size() != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", fi.Size(), len(payload))
	}
}

func TestSSHLive_NonZeroExit(t *testing.T) {
	h, err := Connect(context.Background(), sshTestTarget(t))
	if err != nil {
		t.Fatal(err)
	}
	defer h.(interface{ Close() error }).Close()

	_, code, err := h.Run(context.Background(), "echo oops >&2; exit 3")
	if code != 3 {
		t.Fatalf("code = %d, want 3", code)
	}
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("stderr not surfaced: %v", err)
	}
}

func TestSSHLive_Timeout(t *testing.T) {
	h, err := Connect(context.Background(), sshTestTarget(t))
	if err != nil {
		t.Fatal(err)
	}
	defer h.(interface{ Close() error }).Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = h.Run(ctx, "sleep 30")
	if err == nil {
		t.Fatal("expected ctx deadline error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout not enforced: %v elapsed", time.Since(start))
	}
}

func TestSSHLive_BadAuth(t *testing.T) {
	p := sshTestTarget(t)
	// generate an unrelated key to force auth failure
	kf := t.TempDir() + "/wrongkey"
	if _, code, _ := (&Local{}).Run(context.Background(), "ssh-keygen -t ed25519 -f "+kf+" -N '' -q"); code != 0 {
		t.Skip("ssh-keygen unavailable")
	}
	p.Transport.KeyFile = kf
	h, err := Connect(context.Background(), p)
	if err == nil {
		h.(interface{ Close() error }).Close()
		t.Fatal("expected auth failure")
	}
}
