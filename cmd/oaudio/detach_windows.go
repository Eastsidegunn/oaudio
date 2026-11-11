package main

import (
	"os/exec"
	"syscall"
)

// detach makes the background server survive the console that started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
