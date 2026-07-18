package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

// friendlyOS maps runtime.GOOS to a human-friendly label.
func friendlyOS() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "darwin":
		return "macOS"
	default:
		return runtime.GOOS
	}
}

// shellInfo returns the interpreter, command flag, and a syntax hint that does not repeat the interpreter.
func shellInfo() (interpreter, flag, syntaxHint string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", "-NoLogo -NoProfile -NonInteractive -Command -",
			"Use Windows PowerShell syntax (cmdlets, pipelines, `$variables`, and `;`). " +
				"The script is supplied through stdin, so do not wrap it in `powershell -Command`. " +
				"To explicitly use Command Prompt, invoke `cmd.exe /d /c \"...\"`."
	}
	return "sh", "-c",
		"Use POSIX shell syntax (&&, |, 2>&1, etc.)."
}

// shellCommandDescription returns OS-appropriate guidance for the run_command argument.
func shellCommandDescription() string {
	if runtime.GOOS == "windows" {
		return "Windows PowerShell script; use pipelines, semicolons, and PowerShell quoting; " +
			"do not add a powershell.exe wrapper."
	}
	return "Shell command; chain with && or ;, pipe with |, and use 2>&1 for stderr."
}

// buildEnvironmentBlock captures a short Markdown runtime summary whose day-level date supports prompt caching.
func buildEnvironmentBlock() string {
	interp, flag, syntaxHint := shellInfo()
	shellPhrase := interp + " " + flag
	wd, err := os.Getwd()
	if err != nil {
		wd = "(unknown)"
	}
	currentDate := time.Now().Format("2006-01-02")
	return fmt.Sprintf("# Runtime environment\n"+
		"- Platform: %s/%s\n"+
		"- Shell: commands run via `%s`. %s\n"+
		"- Working directory: %q\n"+
		"- Current date: %s",
		friendlyOS(), runtime.GOARCH, shellPhrase, syntaxHint, wd, currentDate)
}
