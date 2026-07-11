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

// newShellCommand sends the script over stdin so it is parsed only by
// PowerShell, avoiding the fragile cmd.exe -> PowerShell quoting boundary.
func newShellCommand(command string) *exec.Cmd {
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "-")
	cmd.Stdin = strings.NewReader("$ErrorActionPreference = 'Stop'\n" + command + powerShellEpilogue)
	return cmd
}

// sysProcAttrNewProcessGroup puts the child in a new process group on Windows
// so taskkill can target the whole tree later.
func sysProcAttrNewProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessGroup terminates the process tree rooted at pid using taskkill.
func killProcessGroup(pid int) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
