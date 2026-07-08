package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// commandRunning indicates whether a shell command is currently executing.
// main.go's signal handler consults it (under commandMu) to decide whether a
// Ctrl-C should cancel the running command or quit the whole session.
var commandRunning atomic.Bool

// currentCommandCancel holds the cancel func for the command in flight, or nil
// when no command is running. Guarded by commandMu. main.go's signal handler
// invokes it to cancel a single command on Ctrl-C without ending the session.
var (
	commandMu            sync.Mutex
	currentCommandCancel context.CancelFunc
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
				"use backgrounding (`cmd > out.log 2>&1 &`), output redirection, or poll in a later call. " +
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
// returned as the output string (not as a Go error) so they can be fed back
// to the model as a normal tool result, matching the harness's original behavior.
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
// cmd /c on Windows). The shell and everything it spawns are placed in a
// dedicated process group so a cancellation or the hard 30s timeout can kill
// the entire tree — not just the direct sh -c child — which is what previously
// let runaway looping grandchildren survive. Combined stdout/stderr is streamed
// into a buffer so output is captured even when we interrupt the process.
func runShellCommand(ctx context.Context, command string) (string, error) {
	// Base context carries the hard 30s timeout and inherits caller
	// cancellation (e.g. session teardown). A wrapping cancellable context lets
	// a manual Ctrl-C cancel this specific command without ending the session.
	baseCtx, timeoutCancel := context.WithTimeout(ctx, 30*time.Second)
	defer timeoutCancel()

	cmdCtx, cmdCancel := context.WithCancel(baseCtx)

	commandMu.Lock()
	currentCommandCancel = cmdCancel
	commandRunning.Store(true)
	commandMu.Unlock()

	defer func() {
		cmdCancel()
		commandMu.Lock()
		currentCommandCancel = nil
		commandRunning.Store(false)
		commandMu.Unlock()
	}()

	shell, shellFlag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, shellFlag = "cmd", "/c"
	}

	cmd := exec.CommandContext(cmdCtx, shell, shellFlag, command)
	// Spawn in a fresh process group so a cancellation/timeout can kill the
	// entire tree (the real runaway loop is usually a grandchild of sh -c).
	cmd.SysProcAttr = sysProcAttrNewProcessGroup()

	// Capture combined stdout/stderr ourselves; CombinedOutput would block
	// until exit, preventing a timely reaction to cancellation.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return "", err
	}

	var buf bytes.Buffer
	copyDone := make(chan struct{})
	go func() {
		io.Copy(&buf, stdout)
		close(copyDone)
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case err = <-waitCh:
		<-copyDone
		// trim trailing whitespace so output stays compact (no spurious blank lines)
		return strings.TrimRight(buf.String(), "\n\t "), err
	case <-cmdCtx.Done():
		// Cancel or timeout: destroy the whole process group so any
		// grandchildren (the actual runaway loop) are killed too. SIGKILL
		// cannot be trapped, so this reliably stops the loop.
		killProcessGroup(cmd.Process.Pid)
		<-waitCh
		<-copyDone
		if cmdCtx.Err() == context.DeadlineExceeded {
			return strings.TrimRight(buf.String(), "\n\t "),
				fmt.Errorf("command timed out after 30s")
		}
		return strings.TrimRight(buf.String(), "\n\t "),
			fmt.Errorf("command cancelled by user (Ctrl-C)")
	}
}
