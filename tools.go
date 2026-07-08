package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// runCommandTool returns the Tool definition for the run_command shell tool.
// Its description is OS-aware (built from shellInfo) so the model is told to
// use CMD/PowerShell on Windows and POSIX shell elsewhere.
func runCommandTool() Tool {
	_, _, shellSyntaxNote := shellInfo()
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name: "run_command",
			Description: "Execute a shell command on the user's local machine and return " +
				"its combined stdout/stderr. " + shellSyntaxNote + " " +
				"Use it to inspect the filesystem, run builds/tests, query git, read files, " +
				"or apply changes. There is a hard 30-second timeout per call; for long tasks " +
				"use backgrounding (`cmd &`), output redirection, or poll in a later call. " +
				"Output is trimmed of trailing whitespace. Prefer read-only investigative " +
				"commands before making changes, and verify changes afterwards.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type": "string",
						"description": "The full shell command to execute. " +
							"Chain with && or ; , pipe with |, and use 2>&1 to capture stderr.",
					},
				},
				"required": []string{"command"},
			},
		},
	}
}

// allTools returns the full set of tools the model may call. Register new
// tools here in one place; main.go simply assigns this to client.Tools.
func allTools() []Tool {
	return []Tool{runCommandTool()}
}

// executeToolCall runs a single tool call and returns the command that was
// (attempted to be) executed and its output text. Parse/execution failures are
// returned as the output string (not as a Go error) so they can be fed back to
// the model as a normal tool result, matching the harness's original behavior.
func executeToolCall(ctx context.Context, tc ToolCall) (cmd string, output string) {
	switch tc.Function.Name {
	case "run_command":
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return "", fmt.Sprintf("error: invalid arguments: %v", err)
		}
		out, err := runShellCommand(ctx, args.Command)
		if err != nil {
			out = fmt.Sprintf("error: %v", err)
		}
		return args.Command, out
	default:
		return "", fmt.Sprintf("error: unknown tool: %s", tc.Function.Name)
	}
}

// runShellCommand executes command in the platform shell (sh -c on POSIX,
// cmd /c on Windows) with a hard 30s timeout inherited from ctx. It returns
// the combined stdout/stderr (trailing whitespace trimmed).
func runShellCommand(ctx context.Context, command string) (string, error) {
	// Derive a child context that inherits cancellation from the caller
	// (e.g. Ctrl-C) but also enforces a hard 30s timeout per command.
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	shell, shellFlag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, shellFlag = "cmd", "/c"
	}
	cmd := exec.CommandContext(cmdCtx, shell, shellFlag, command)
	out, err := cmd.CombinedOutput()
	// trim trailing whitespace so output stays compact (no spurious blank lines)
	return strings.TrimRight(string(out), "\n\t "), err
}
