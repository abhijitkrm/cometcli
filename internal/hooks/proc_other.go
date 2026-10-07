//go:build !unix

package hooks

import "os/exec"

func isolate(*exec.Cmd) {}
