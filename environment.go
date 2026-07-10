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

// shellInfo returns the interpreter, the flag used to pass a command to it, and
// a human-readable trailing hint describing the shell syntax the model should
// use. The hint does NOT repeat the interpreter (callers compose "via `interp
// flag`") so it reads cleanly in both the system prompt and the tool schema.
func shellInfo() (interpreter, flag, syntaxHint string) {
	if runtime.GOOS == "windows" {
		return "cmd", "/c",
			"Use Windows Command Prompt syntax (e.g. `dir`, `copy`, `&&`, `> file`). " +
				"For PowerShell prefix the command with `powershell -NoProfile -Command \"...\"`. " +
				"Paths use backslashes and quoting uses double-quotes."
	}
	return "sh", "-c",
		"Use POSIX shell syntax (&&, |, 2>&1, etc.)."
}

// shellCommandDescription returns OS-appropriate guidance for the command
// argument in the run_command tool schema.
func shellCommandDescription() string {
	if runtime.GOOS == "windows" {
		return "The complete Command Prompt command to execute. Chain commands with &&, " +
			"pipe with |, and use 2>&1 to capture stderr."
	}
	return "The full shell command to execute. Chain with && or ;, pipe with |, " +
		"and use 2>&1 to capture stderr."
}

// buildEnvironmentBlock produces a short Markdown section describing the
// runtime environment. It is captured once at startup. The date changes only
// once per day, which permits substantially more cross-session prompt caching
// than a session timestamp.
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
