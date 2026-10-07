//go:build unix

package hooks

import (
	"os/exec"
	"syscall"
)

// isolate runs the hook in its own process group and makes cancel kill
// the whole group, so a hook's children can't outlive its timeout.
func isolate(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
