//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"
)

// sysProcAttrNewProcessGroup puts the child in a new process group on Windows
// so taskkill can target the whole tree later.
func sysProcAttrNewProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessGroup terminates the process tree rooted at pid using taskkill.
func killProcessGroup(pid int) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
