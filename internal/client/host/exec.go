package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ExecResult is the outcome of a shell script run by Exec.
type ExecResult struct {
	Output    string // stdout and stderr, interleaved as written
	Code      int    // exit status (-1 when killed)
	TimedOut  bool
	Truncated bool // Output kept only its head and tail
}

// Execer runs a script with combined output, an output cap, and
// process-tree cleanup on cancel. Local and SSH implement it; other Host
// implementations fall back to Run.
type Execer interface {
	Exec(ctx context.Context, script string, maxOut int) (ExecResult, error)
}

// Exec runs script on h via Execer when available.
func Exec(ctx context.Context, h Host, script string, maxOut int) (ExecResult, error) {
	if e, ok := h.(Execer); ok {
		return e.Exec(ctx, script, maxOut)
	}
	out, code, err := h.Run(ctx, script)
	if err != nil && code != 0 {
		out += "\n" + err.Error()
		err = nil
	}
	return ExecResult{Output: out, Code: code, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}, err
}

// nonInteractiveEnv keeps tools from waiting on a terminal that isn't
// there (stdin is /dev/null): pagers, git credential prompts, apt.
var nonInteractiveEnv = []string{
	"PAGER=cat", "GIT_PAGER=cat", "SYSTEMD_PAGER=", "GIT_TERMINAL_PROMPT=0",
	"DEBIAN_FRONTEND=noninteractive", "TERM=dumb", "NO_COLOR=1",
}

// capWriter keeps the first and last half of max bytes.
type capWriter struct {
	mu        sync.Mutex
	max       int
	head      bytes.Buffer
	tail      []byte
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if room := w.max/2 - w.head.Len(); room > 0 {
		k := min(room, len(p))
		w.head.Write(p[:k])
		p = p[k:]
	}
	if len(p) > 0 {
		w.tail = append(w.tail, p...)
		if over := len(w.tail) - w.max/2; over > 0 {
			w.tail = w.tail[over:]
			w.truncated = true
		}
	}
	return n, nil
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.truncated {
		return w.head.String() + string(w.tail)
	}
	return fmt.Sprintf("%s\n…[output truncated]…\n%s", w.head.String(), w.tail)
}

func shellBin() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "/bin/sh"
}

// Exec runs script under bash (or sh) with stdin from /dev/null. On
// cancel or timeout the whole process group gets SIGTERM, then SIGKILL.
func (l *Local) Exec(ctx context.Context, script string, maxOut int) (ExecResult, error) {
	c := exec.Command(shellBin(), "-c", script)
	c.Env = append(os.Environ(), nonInteractiveEnv...)
	w := &capWriter{max: maxOut}
	c.Stdout, c.Stderr = w, w
	setProcessGroup(c)
	if err := c.Start(); err != nil {
		return ExecResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-ctx.Done():
		timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		killGroup(c, false)
		select {
		case err = <-done:
		case <-time.After(2 * time.Second):
			killGroup(c, true)
			err = <-done
		}
	}
	res := ExecResult{Output: w.String(), Truncated: w.truncated, TimedOut: timedOut}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Code = ee.ExitCode()
	default:
		return res, err
	}
	if ctx.Err() != nil && res.Code == 0 {
		res.Code = -1
	}
	return res, nil
}

// Exec runs script on the remote host. Cancel kills the session.
func (s *SSH) Exec(ctx context.Context, script string, maxOut int) (ExecResult, error) {
	sess, err := s.client.NewSession()
	if err != nil {
		return ExecResult{}, err
	}
	defer sess.Close()
	w := &capWriter{max: maxOut}
	sess.Stdout, sess.Stderr = w, w
	env := ""
	for _, e := range nonInteractiveEnv {
		env += e + " "
	}
	q := shellQuote(script)
	cmd := fmt.Sprintf("export %s; if command -v bash >/dev/null 2>&1; then exec bash -c %s; else exec sh -c %s; fi", env, q, q)
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	res := ExecResult{}
	select {
	case err = <-done:
	case <-ctx.Done():
		res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		_ = sess.Signal(ssh.SIGKILL)
		sess.Close()
		err = <-done
		res.Code = -1
	}
	res.Output, res.Truncated = w.String(), w.truncated
	var ee *ssh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Code = ee.ExitStatus()
	case res.Code == -1:
	default:
		var missing *ssh.ExitMissingError
		if !errors.As(err, &missing) {
			return res, err
		}
		res.Code = -1
	}
	return res, nil
}
