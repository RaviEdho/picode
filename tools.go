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
	"time"
)

type ToolExecutor struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func NewToolExecutor() *ToolExecutor { return &ToolExecutor{} }

func (e *ToolExecutor) CancelActive() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel == nil {
		return false
	}
	e.cancel()
	return true
}

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
						"type":        "string",
						"description": shellCommandDescription(),
					},
				},
				"required": []string{"command"},
			},
		},
	}
}

func allTools() []Tool {
	return []Tool{runCommandTool()}
}

func (e *ToolExecutor) Execute(ctx context.Context, tc ToolCall) (cmd string, output string) {
	switch tc.Function.Name {
	case "run_command":
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return "", fmt.Sprintf("error: invalid arguments: %v", err)
		}
		out, err := e.runShellCommand(ctx, args.Command)
		if err != nil {
			out = fmt.Sprintf("error: %v", err)
		}
		if out == "" {
			out = "(no output)"
		}
		return args.Command, out
	default:
		return "", fmt.Sprintf("error: unknown tool: %s", tc.Function.Name)
	}
}

func (e *ToolExecutor) runShellCommand(ctx context.Context, command string) (string, error) {
	baseCtx, timeoutCancel := context.WithTimeout(ctx, 30*time.Second)
	defer timeoutCancel()

	cmdCtx, cmdCancel := context.WithCancel(baseCtx)

	e.mu.Lock()
	e.cancel = cmdCancel
	e.mu.Unlock()

	defer func() {
		cmdCancel()
		e.mu.Lock()
		e.cancel = nil
		e.mu.Unlock()
	}()

	shell, shellFlag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, shellFlag = "cmd", "/c"
	}

	cmd := exec.CommandContext(cmdCtx, shell, shellFlag, command)
	cmd.SysProcAttr = sysProcAttrNewProcessGroup()

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
		return strings.TrimRight(buf.String(), "\n\t "), err
	case <-cmdCtx.Done():
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
