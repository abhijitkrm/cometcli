//go:build unix

package host

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup signals the command's whole process group, so children
// (docker compose, build steps) die with the shell.
func killGroup(c *exec.Cmd, hard bool) {
	if c.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-c.Process.Pid, sig)
}
