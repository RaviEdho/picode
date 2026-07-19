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

const (
	maxCommandOutput       = 1 << 20
	commandOutputHeadBytes = maxCommandOutput / 2
	commandOutputTailBytes = maxCommandOutput - commandOutputHeadBytes
)

var (
	errToolCancelled = errors.New("tool cancelled by user")
	errToolTimedOut  = errors.New("tool timed out")
)

// runCommandTool returns the OpenAI schema for the shell tool.
func runCommandTool() Tool {
	_, _, shellSyntaxNote := shellInfo()
	return functionTool(
		"run_command",
		"Run a focused local shell command; return combined stdout/stderr. "+shellSyntaxNote+" Use for builds/tests, git, metadata, binaries, or when no dedicated tool fits. Keep output focused; investigate read-only first; verify after. Cap: 1 MiB with head/tail retained; timeout: 30s. For long tasks, redirect and poll. Trim trailing whitespace.",
		map[string]any{
			"command": stringParameter(shellCommandDescription()),
		},
		"command",
	)
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
	if len(e.active) == 0 {
		return false
	}
	for _, cancel := range e.active {
		cancel(errToolCancelled)
	}
	return true
}

// runShellCommand captures output and places the process tree in its own group.
func (e *ToolExecutor) runShellCommand(ctx context.Context, command string) (string, ToolStatus, error) {
	// Every command inherits session cancellation and has a hard timeout.
	baseCtx, timeoutCancel := context.WithTimeoutCause(ctx, 30*time.Second, errToolTimedOut)
	defer timeoutCancel()

	cmdCtx, cmdCancel := context.WithCancelCause(baseCtx)

	// Publish this cancel function for the frontend's Ctrl-C handler.
	commandID := e.registerActive(cmdCancel)

	defer func() {
		cmdCancel(nil)
		e.unregisterActive(commandID)
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
	output := newBoundedCommandOutput()
	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(output, stdout)
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
		return output.String(), status, err
	case <-cmdCtx.Done():
		// Kill the group so child processes cannot survive cancellation.
		killProcessGroup(cmd.Process.Pid)
		<-waitCh
		<-copyDone
		status, err := commandCancellation(context.Cause(cmdCtx))
		return output.String(), status, err
	}
}

type boundedCommandOutput struct {
	head      bytes.Buffer
	tail      []byte
	truncated bool
}

func newBoundedCommandOutput() *boundedCommandOutput {
	return &boundedCommandOutput{tail: make([]byte, 0, commandOutputTailBytes)}
}

func (o *boundedCommandOutput) Write(data []byte) (int, error) {
	original := len(data)
	if o.head.Len() < commandOutputHeadBytes {
		n := commandOutputHeadBytes - o.head.Len()
		if n > len(data) {
			n = len(data)
		}
		_, _ = o.head.Write(data[:n])
		data = data[n:]
	}
	if len(data) > 0 {
		o.truncated = true
		combined := append(o.tail, data...)
		if len(combined) > commandOutputTailBytes {
			combined = combined[len(combined)-commandOutputTailBytes:]
		}
		o.tail = append(o.tail[:0], combined...)
	}
	return original, nil
}

func (o *boundedCommandOutput) String() string {
	value := o.head.String()
	if o.truncated {
		value += "\n[output truncated after 1 MiB]\n" + string(o.tail)
	} else {
		value += string(o.tail)
	}
	return strings.TrimRight(value, "\n\t ")
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
