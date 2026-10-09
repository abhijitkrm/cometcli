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
	"strings"
	"time"

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
