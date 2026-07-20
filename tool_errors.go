package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// toolError builds the model-facing error string used by every tool. Keeping
// the formatting here prevents validation, path, I/O, and abort messages from
// drifting between tools.
func toolError(class, field, hint string, cause error) string {
	var b strings.Builder
	b.WriteString(class)
	if field != "" {
		b.WriteByte(' ')
		b.WriteString(field)
	}
	if hint != "" {
		b.WriteString(": ")
		b.WriteString(hint)
	}
	if cause != nil {
		if hint != "" || field != "" {
			b.WriteString(" - ")
		} else {
			b.WriteString(": ")
		}
		b.WriteString(unpackPathError(cause))
	}
	return b.String()
}

// unpackPathError keeps the useful filesystem operation, path, and leaf
// cause while removing wrapper messages such as "resolve" or "open" twice.
func unpackPathError(err error) string {
	if err == nil {
		return ""
	}
	var pathErr *fs.PathError
	for current := err; current != nil; current = errors.Unwrap(current) {
		var candidate *fs.PathError
		if errors.As(current, &candidate) {
			pathErr = candidate
		}
	}
	if pathErr != nil {
		return fmt.Sprintf("%s %q: %v", pathErr.Op, pathErr.Path, pathErr.Err)
	}
	return err.Error()
}

func invalidToolError(field, hint string, cause error) error {
	return errors.New(toolError("invalid", field, hint, cause))
}

func badPathToolError(path, hint string) error {
	return errors.New(toolError("bad path", fmt.Sprintf("%q", path), hint, nil))
}

func ioToolError(cause error) error {
	return errors.New(toolError("io error", "", "", cause))
}

func toolAbortedOutput(cause error) string {
	switch {
	case errors.Is(cause, context.Canceled):
		return "aborted: canceled by user"
	case errors.Is(cause, context.DeadlineExceeded):
		return "aborted: timed out"
	default:
		return toolError("aborted", "", "", cause)
	}
}

func toolAbortedResult(input string, cause error) ToolResult {
	return ToolResult{Input: input, Output: toolAbortedOutput(cause), Status: ToolAborted}
}
