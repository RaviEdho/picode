package main

import (
	"context"
	"fmt"
	"sync"
)

// ToolStatus records how a tool call ended.
type ToolStatus string

const (
	ToolCompleted ToolStatus = "completed"
	ToolFailed    ToolStatus = "failed"
	ToolCancelled ToolStatus = "cancelled"
	ToolTimedOut  ToolStatus = "timed_out"
	ToolAborted   ToolStatus = "aborted"
)

// ToolResult carries a tool call's output and final status.
type ToolResult struct {
	Input  string
	Output string
	Status ToolStatus
}

// ToolExecutor dispatches tools and tracks an active cancellable operation.
type ToolExecutor struct {
	mu     sync.Mutex
	cancel context.CancelCauseFunc
}

// NewToolExecutor creates an executor with no active tool.
func NewToolExecutor() *ToolExecutor { return &ToolExecutor{} }

// allTools returns every tool exposed to the model.
func allTools() []Tool {
	return []Tool{runCommandTool(), applyPatchTool()}
}

// Execute validates and runs one model tool call.
func (e *ToolExecutor) Execute(ctx context.Context, tc ToolCall) ToolResult {
	switch tc.Function.Name {
	case "run_command":
		return e.executeRunCommand(ctx, tc)
	case "apply_patch":
		return e.executeApplyPatch(ctx, tc)
	default:
		return ToolResult{
			Output: fmt.Sprintf("error: unknown tool: %s", tc.Function.Name),
			Status: ToolFailed,
		}
	}
}
