//go:build !unix

package host

import "os/exec"

func setProcessGroup(*exec.Cmd) {}

func killGroup(c *exec.Cmd, _ bool) {
	if c.Process != nil {
		_ = c.Process.Kill()
	}
}
