// Package host abstracts host-plane operations: running commands and
// reading/writing files either locally or over SSH. Tools never call
// os/exec directly — they go through this interface so every command
// can be audited and every transport works identically.
package host

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// Host is a remote or local machine running the node.
type Host interface {
	// Run executes a shell command, returning stdout and the exit code.
	// stderr is included in err on non-zero exit.
	Run(ctx context.Context, cmd string) (stdout string, code int, err error)
	// ReadFile reads a file (via sudo-free cat; used for configs/logs only).
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile writes a file with a mode, atomically where possible.
	WriteFile(ctx context.Context, path string, data []byte, perm os.FileMode) error
	// Stat returns os.FileInfo for permission auditing.
	Stat(ctx context.Context, path string) (os.FileInfo, error)
	// String describes the host for logs.
	String() string
}

// Connect builds the transport declared by the profile.
func Connect(ctx context.Context, p *config.Profile) (Host, error) {
	switch p.Transport.Type {
	case "", "local":
		return &Local{}, nil
	case "ssh":
		return dialSSH(ctx, p.Transport)
	default:
		return nil, fmt.Errorf("unknown transport %q", p.Transport.Type)
	}
}

// Local executes on the machine cometcli runs on.
type Local struct{}

func (l *Local) String() string { return "local" }

func (l *Local) Run(ctx context.Context, cmd string) (string, int, error) {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	var out, serr bytes.Buffer
	c.Stdout = &out
	c.Stderr = &serr
	err := c.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if err != nil && code == 0 {
		return out.String(), code, err
	}
	if code != 0 {
		return out.String(), code, fmt.Errorf("exit %d: %s", code, strings.TrimSpace(serr.String()))
	}
	return out.String(), code, nil
}

func (l *Local) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (l *Local) WriteFile(ctx context.Context, path string, data []byte, perm os.FileMode) error {
	tmp := path + ".cometcli-tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (l *Local) Stat(ctx context.Context, path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// SSH executes on a remote machine over a pooled ssh connection.
type SSH struct {
	client *ssh.Client
	target string
}

func (s *SSH) String() string { return "ssh://" + s.target }

func dialSSH(ctx context.Context, t config.Transport) (*SSH, error) {
	auth, err := sshAuth(t)
	if err != nil {
		return nil, err
	}
	user := t.User
	if user == "" {
		user = os.Getenv("USER")
	}
	port := t.Port
	if port == 0 {
		port = 22
	}
	target := fmt.Sprintf("%s:%d", t.Host, port)
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO: known_hosts pinning
		Timeout:         10 * time.Second,
	}
	d := ssh.Dial
	cli, err := d("tcp", target, cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w", target, err)
	}
	return &SSH{client: cli, target: target}, nil
}

func sshAuth(t config.Transport) ([]ssh.AuthMethod, error) {
	keyPath := t.KeyFile
	if keyPath == "" {
		home, _ := os.UserHomeDir()
		for _, cand := range []string{"id_ed25519", "id_rsa"} {
			p := filepath.Join(home, ".ssh", cand)
			if _, err := os.Stat(p); err == nil {
				keyPath = p
				break
			}
		}
	}
	if keyPath == "" {
		return nil, fmt.Errorf("no ssh key found; set transport.key_file in profile")
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", keyPath, err)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
}

func (s *SSH) Run(ctx context.Context, cmd string) (string, int, error) {
	return s.runStdin(ctx, cmd, nil)
}

func (s *SSH) runStdin(ctx context.Context, cmd string, stdin *bytes.Reader) (string, int, error) {
	sess, err := s.client.NewSession()
	if err != nil {
		return "", -1, err
	}
	defer sess.Close()
	var out, serr bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &serr
	if stdin != nil {
		sess.Stdin = stdin
	}

	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		return out.String(), -1, ctx.Err()
	case err := <-done:
		code := 0
		if ee, ok := err.(*ssh.ExitError); ok {
			code = ee.ExitStatus()
		}
		if err != nil && code == 0 {
			return out.String(), code, err
		}
		if code != 0 {
			return out.String(), code, fmt.Errorf("exit %d: %s", code, strings.TrimSpace(serr.String()))
		}
		return out.String(), code, nil
	}
}

func (s *SSH) ReadFile(ctx context.Context, path string) ([]byte, error) {
	out, code, err := s.Run(ctx, "cat -- "+shellQuote(path))
	if err != nil {
		return nil, fmt.Errorf("read %s (exit %d): %w", path, code, err)
	}
	return []byte(out), nil
}

func (s *SSH) WriteFile(ctx context.Context, path string, data []byte, perm os.FileMode) error {
	tmp := path + ".cometcli-tmp"
	cmd := fmt.Sprintf("umask 077 && cat > %s && chmod %o %s && mv %s %s",
		shellQuote(tmp), uint32(perm), shellQuote(tmp), shellQuote(tmp), shellQuote(path))
	_, code, err := s.runStdin(ctx, cmd, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("write %s (exit %d): %w", path, code, err)
	}
	return nil
}

func (s *SSH) Stat(ctx context.Context, path string) (os.FileInfo, error) {
	out, code, err := s.Run(ctx, "stat -c '%s %a %U %G %n' -- "+shellQuote(path))
	if err != nil {
		return nil, fmt.Errorf("stat %s (exit %d): %w", path, code, err)
	}
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) < 5 {
		return nil, fmt.Errorf("bad stat output for %s", path)
	}
	var size int64
	fmt.Sscan(f[0], &size)
	var mode uint64
	fmt.Sscanf(f[1], "%o", &mode)
	return &fileInfo{name: f[4], size: size, mode: os.FileMode(mode)}, nil
}

// Close shuts the connection.
func (s *SSH) Close() error { return s.client.Close() }

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type fileInfo struct {
	name string
	size int64
	mode os.FileMode
}

func (f *fileInfo) Name() string       { return f.name }
func (f *fileInfo) Size() int64        { return f.size }
func (f *fileInfo) Mode() os.FileMode  { return f.mode }
func (f *fileInfo) ModTime() time.Time { return time.Time{} }
func (f *fileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f *fileInfo) Sys() any           { return nil }
