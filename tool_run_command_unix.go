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

// sysProcAttrNewProcessGroup makes the spawned shell the leader of a fresh
// process group, so the whole tree (shell + its descendants) can be signalled
// together when the command is cancelled or times out.
func sysProcAttrNewProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGKILL to the entire process group. A negative pid
// targets the group whose leader has that pid (which, thanks to Setpgid,
// equals the direct child's pid).
func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
