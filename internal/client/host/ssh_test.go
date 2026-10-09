package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/testutil/sshd"
)

// isolate points HOME (known_hosts, ~/.ssh/config, default keys) at a
// temp dir and hides any real ssh-agent.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	_ = os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	trust, pass := TrustHost, Passphrase
	t.Cleanup(func() { TrustHost, Passphrase = trust, pass })
	TrustHost, Passphrase = nil, nil
	return home
}

func knownHost(t *testing.T, home string, s *sshd.Server, key ssh.PublicKey) {
	t.Helper()
	line := knownhosts.Line([]string{knownhosts.Normalize(s.Addr)}, key)
	f, _ := os.OpenFile(filepath.Join(home, ".ssh", "known_hosts"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	fmt.Fprintln(f, line)
	f.Close()
}

func transport(s *sshd.Server, key string) config.Transport {
	return config.Transport{Type: "ssh", Host: s.Host, Port: s.Port, KeyFile: key, User: "tester"}
}

func dial(t *testing.T, tr config.Transport) *SSH {
	t.Helper()
	h, err := dialSSH(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func TestUnknownHostIsRefusedUntilTrusted(t *testing.T) {
	home := isolate(t)
	key, pub := sshd.ClientKey(t, home, "")
	s := sshd.Start(t, pub)

	_, err := dialSSH(context.Background(), transport(s, key))
	var unknown *UnknownHostError
	if !errors.As(err, &unknown) || !strings.Contains(err.Error(), "SHA256:") {
		t.Fatalf("want an unknown-host error with the fingerprint, got %v", err)
	}

	var asked string
	TrustHost = func(h, fp string) bool { asked = h + " " + fp; return true }
	h := dial(t, transport(s, key))
	if !strings.Contains(asked, s.Addr) && !strings.Contains(asked, s.Host) {
		t.Errorf("trust prompt didn't name the host: %q", asked)
	}
	h.Close()

	// remembered: no prompt the second time
	TrustHost = nil
	dial(t, transport(s, key))
}

func TestChangedHostKeyIsRefused(t *testing.T) {
	home := isolate(t)
	key, pub := sshd.ClientKey(t, home, "")
	s := sshd.Start(t, pub)
	other := sshd.Start(t, pub) // a different server's key, same algorithms
	knownHost(t, home, s, other.HostKeys[0].PublicKey())
	TrustHost = func(string, string) bool { t.Error("must not offer to trust a changed key"); return true }

	_, err := dialSSH(context.Background(), transport(s, key))
	var changed *ChangedHostKeyError
	if !errors.As(err, &changed) || !strings.Contains(err.Error(), "ssh-keygen -R") {
		t.Fatalf("want a changed-host-key error, got %v", err)
	}
}

func TestKnownKeyTypeIsNegotiated(t *testing.T) {
	// the server has ed25519 and ECDSA keys; Go's client would pick ECDSA
	// and take a host known by its ed25519 key for a changed one
	for i, name := range []string{"ed25519", "ecdsa"} {
		t.Run(name, func(t *testing.T) {
			home := isolate(t)
			key, pub := sshd.ClientKey(t, home, "")
			s := sshd.Start(t, pub)
			knownHost(t, home, s, s.HostKeys[i].PublicKey())
			dial(t, transport(s, key))
		})
	}
}

func TestSSHConfigAlias(t *testing.T) {
	home := isolate(t)
	key, pub := sshd.ClientKey(t, home, "")
	s := sshd.Start(t, pub)
	knownHost(t, home, s, s.HostKeys[0].PublicKey())
	cfg := fmt.Sprintf("Host val1\n  HostName %s\n  Port %d\n  User tester\n  IdentityFile %s\n  IdentitiesOnly yes\n", s.Host, s.Port, key)
	_ = os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(cfg), 0o600)

	tg := Resolve(config.Transport{Type: "ssh", Host: "val1"})
	if tg.HostName != s.Host || tg.Port != s.Port || tg.User != "tester" || len(tg.Keys) != 1 || tg.Keys[0] != key || !tg.OnlyKeys {
		t.Fatalf("alias not applied: %+v", tg)
	}
	// the profile's own settings win over ~/.ssh/config
	if tg := Resolve(config.Transport{Type: "ssh", Host: "val1", User: "root", Port: 2200}); tg.User != "root" || tg.Port != 2200 {
		t.Fatalf("profile settings should win: %+v", tg)
	}

	h := dial(t, config.Transport{Type: "ssh", Host: "val1"})
	out, _, err := h.Run(context.Background(), "echo alias-ok")
	if err != nil || strings.TrimSpace(out) != "alias-ok" {
		t.Fatalf("run over alias: %q %v", out, err)
	}
}

func TestJumpHost(t *testing.T) {
	home := isolate(t)
	key, pub := sshd.ClientKey(t, home, "")
	bastion := sshd.Start(t, pub)
	node := sshd.Start(t, pub)
	knownHost(t, home, bastion, bastion.HostKeys[0].PublicKey())
	knownHost(t, home, node, node.HostKeys[0].PublicKey())
	cfg := fmt.Sprintf("Host bastion\n  HostName %s\n  Port %d\n  User tester\n  IdentityFile %s\n", bastion.Host, bastion.Port, key)
	_ = os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(cfg), 0o600)

	tr := transport(node, key)
	tr.Jump = "bastion"
	h := dial(t, tr)
	if _, _, err := h.Run(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	if bastion.Forwards.Load() != 1 {
		t.Errorf("the node should be reached through the bastion (forwards=%d)", bastion.Forwards.Load())
	}
}

func TestReconnectsAfterDrop(t *testing.T) {
	home := isolate(t)
	key, pub := sshd.ClientKey(t, home, "")
	s := sshd.Start(t, pub)
	knownHost(t, home, s, s.HostKeys[0].PublicKey())
	h := dial(t, transport(s, key))
	s.DropAll()
	out, _, err := h.Run(context.Background(), "echo back")
	if err != nil || strings.TrimSpace(out) != "back" {
		t.Fatalf("after a dropped connection: %q %v", out, err)
	}
	res, err := h.Exec(context.Background(), "echo exec-back", 1<<10)
	if err != nil || strings.TrimSpace(res.Output) != "exec-back" {
		t.Fatalf("exec after reconnect: %+v %v", res, err)
	}
}

func TestPassphraseProtectedKey(t *testing.T) {
	home := isolate(t)
	key, pub := sshd.ClientKey(t, home, "s3cret")
	s := sshd.Start(t, pub)
	knownHost(t, home, s, s.HostKeys[0].PublicKey())

	_, err := dialSSH(context.Background(), transport(s, key))
	if err == nil || !strings.Contains(err.Error(), "ssh-add "+key) {
		t.Fatalf("want a hint to load the key into ssh-agent, got %v", err)
	}
	Passphrase = func(string) ([]byte, error) { return []byte("s3cret"), nil }
	dial(t, transport(s, key))
}

func TestTunnelReachesNodeLocalPorts(t *testing.T) {
	home := isolate(t)
	key, pub := sshd.ClientKey(t, home, "")
	s := sshd.Start(t, pub)
	knownHost(t, home, s, s.HostKeys[0].PublicKey())
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	tr := transport(s, key)
	h := dial(t, tr)

	ep := "tcp://" + echo.Addr().String()
	if !OnNode(tr, ep) || OnNode(config.Transport{Type: "local"}, ep) || OnNode(tr, "https://rpc.example.org:443") {
		t.Fatal("OnNode: loopback endpoints of an ssh profile go through the tunnel, others don't")
	}
	conn, err := NodeDialer(h, tr)(context.Background(), "tcp", echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo through tunnel: %q %v", buf, err)
	}
	if s.Forwards.Load() != 1 {
		t.Errorf("forwards = %d, want 1", s.Forwards.Load())
	}
	// a closed port on the node is an error, not a dead connection
	if _, err := NodeDialer(h, tr)(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Error("dialing a closed port should fail")
	}
	if _, _, err := h.Run(context.Background(), "true"); err != nil {
		t.Errorf("connection should survive a refused forward: %v", err)
	}
}

func TestRemotePathKeepsHome(t *testing.T) {
	if got := rpath("~/.evmd/config/app.toml"); got != `"$HOME"/'.evmd/config/app.toml'` {
		t.Errorf("rpath = %s", got)
	}
	if got := rpath("/data/it's"); got != `'/data/it'\''s'` {
		t.Errorf("rpath = %s", got)
	}
}
