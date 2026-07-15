//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// newShellCommand passes the command directly to the POSIX shell.
func newShellCommand(command string) *exec.Cmd {
	return exec.Command("sh", "-c", command)
}

// sysProcAttrNewProcessGroup lets cancellation signal the shell and all descendants together.
func sysProcAttrNewProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup uses a negative PID to signal the child-led process group.
func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
