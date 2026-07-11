package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	errToolCancelled = errors.New("tool cancelled by user")
	errToolTimedOut  = errors.New("tool timed out")
)

// runCommandTool returns the OpenAI schema for the shell tool.
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
				"redirect output and poll it in a later call. " +
				"Output is trimmed of trailing whitespace. Prefer read-only investigative " +
				"commands before making changes, and verify changes afterwards.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": shellCommandDescription(),
					},
				},
				"required": []string{"command"},
			},
		},
	}
}

// executeRunCommand decodes and executes a run_command tool call.
func (e *ToolExecutor) executeRunCommand(ctx context.Context, tc ToolCall) ToolResult {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return ToolResult{
			Output: fmt.Sprintf("error: invalid arguments: %v", err),
			Status: ToolFailed,
		}
	}

	output, status, err := e.runShellCommand(ctx, args.Command)
	if err != nil {
		output = fmt.Sprintf("error: %v", err)
	}
	if output == "" {
		output = "(no output)"
	}
	return ToolResult{Input: args.Command, Output: output, Status: status}
}

// CancelActive stops the current command without ending the session.
func (e *ToolExecutor) CancelActive() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel == nil {
		return false
	}
	e.cancel(errToolCancelled)
	return true
}

// runShellCommand captures output and places the process tree in its own group.
func (e *ToolExecutor) runShellCommand(ctx context.Context, command string) (string, ToolStatus, error) {
	// Every command inherits session cancellation and has a hard timeout.
	baseCtx, timeoutCancel := context.WithTimeoutCause(ctx, 30*time.Second, errToolTimedOut)
	defer timeoutCancel()

	cmdCtx, cmdCancel := context.WithCancelCause(baseCtx)

	// Publish this cancel function for the frontend's Ctrl-C handler.
	e.mu.Lock()
	e.cancel = cmdCancel
	e.mu.Unlock()

	defer func() {
		cmdCancel(nil)
		e.mu.Lock()
		e.cancel = nil
		e.mu.Unlock()
	}()

	if cause := context.Cause(cmdCtx); cause != nil {
		status, err := commandCancellation(cause)
		return "", status, err
	}

	// Cancellation is handled below so the full process group is always killed.
	cmd := newShellCommand(command)
	cmd.SysProcAttr = sysProcAttrNewProcessGroup()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", ToolFailed, err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		if cause := context.Cause(cmdCtx); cause != nil {
			status, cancelErr := commandCancellation(cause)
			return "", status, cancelErr
		}
		return "", ToolFailed, err
	}

	// Drain output concurrently so a cancelled process cannot block on I/O.
	var buf bytes.Buffer
	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, stdout)
		close(copyDone)
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case err = <-waitCh:
		<-copyDone
		status := ToolCompleted
		if err != nil {
			status = ToolFailed
		}
		return strings.TrimRight(buf.String(), "\n\t "), status, err
	case <-cmdCtx.Done():
		// Kill the group so child processes cannot survive cancellation.
		killProcessGroup(cmd.Process.Pid)
		<-waitCh
		<-copyDone
		status, err := commandCancellation(context.Cause(cmdCtx))
		return strings.TrimRight(buf.String(), "\n\t "), status, err
	}
}

// commandCancellation maps a context cause to a stable tool result.
func commandCancellation(cause error) (ToolStatus, error) {
	switch {
	case errors.Is(cause, errToolCancelled):
		return ToolCancelled, fmt.Errorf("command cancelled by user (Ctrl-C)")
	case errors.Is(cause, errToolTimedOut):
		return ToolTimedOut, fmt.Errorf("command timed out after 30s")
	default:
		return ToolAborted, fmt.Errorf("command aborted: %w", cause)
	}
}
