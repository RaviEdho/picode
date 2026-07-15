//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

const powerShellEpilogue = `
$picodeSucceeded = $?
if (-not $picodeSucceeded) {
    if ($null -ne $LASTEXITCODE -and $LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    exit 1
}
`

// newShellCommand sends the script over stdin to avoid a cmd.exe-to-PowerShell quoting boundary.
func newShellCommand(command string) *exec.Cmd {
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "-")
	cmd.Stdin = strings.NewReader("$ErrorActionPreference = 'Stop'\n" + command + powerShellEpilogue)
	return cmd
}

// sysProcAttrNewProcessGroup lets taskkill target the entire child process tree.
func sysProcAttrNewProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessGroup terminates the process tree rooted at pid using taskkill.
func killProcessGroup(pid int) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
