//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach makes the background server survive the terminal that started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
