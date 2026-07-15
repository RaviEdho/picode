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
	nextID uint64
	active map[uint64]context.CancelCauseFunc
}

// NewToolExecutor creates an executor with no active tool.
func NewToolExecutor() *ToolExecutor {
	return &ToolExecutor{active: make(map[uint64]context.CancelCauseFunc)}
}

func (e *ToolExecutor) registerActive(cancel context.CancelCauseFunc) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextID++
	id := e.nextID
	e.active[id] = cancel
	return id
}

func (e *ToolExecutor) unregisterActive(id uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.active, id)
}

// ExecuteBatch runs read-only tools concurrently and mutations sequentially while retaining result order.
func (e *ToolExecutor) ExecuteBatch(ctx context.Context, calls []ToolCall) []ToolResult {
	results := make([]ToolResult, len(calls))
	var group sync.WaitGroup
	for i, call := range calls {
		if toolMutatesWorkspace(call.Function.Name) {
			group.Wait()
			results[i] = e.Execute(ctx, call)
			continue
		}
		group.Add(1)
		go func(index int, tc ToolCall) {
			defer group.Done()
			results[index] = e.Execute(ctx, tc)
		}(i, call)
	}
	group.Wait()
	return results
}

func toolMutatesWorkspace(name string) bool { return name == "apply_patch" || name == "run_command" }

// allTools returns every tool exposed to the model.
func allTools() []Tool {
	return []Tool{listFileTool(), readFileTool(), searchTool(), runCommandTool(), applyPatchTool()}
}

// Execute validates and runs one model tool call.
func (e *ToolExecutor) Execute(ctx context.Context, tc ToolCall) ToolResult {
	switch tc.Function.Name {
	case "list_file":
		return e.executeListFile(ctx, tc)
	case "read_file":
		return e.executeReadFile(ctx, tc)
	case "search":
		return e.executeSearch(ctx, tc)
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
