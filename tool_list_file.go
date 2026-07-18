package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	listFileDefaultDepth   = 2
	listFileMaxDepth       = 8
	listFileDefaultEntries = 200
	listFileMaxEntries     = 500
	listFileMaxOutput      = 32 * 1024
)

// listFileTool returns a bounded directory listing without shell syntax or metadata noise.
func listFileTool() Tool {
	return functionTool(
		"list_file",
		"Bounded directory tree under a relative path in cwd. Start with the smallest relevant path, depth 1, and small max_entries; expand only as needed. For repository structure, not contents. Sorted D/F/S output; skips .git. Use run_command for metadata/filtering.",
		map[string]any{
			"path":        stringParameter("Smallest relevant relative directory; default ."),
			"depth":       integerParameter(1, listFileMaxDepth, "Max descendant depth; start at 1; increase only as needed; default 2."),
			"max_entries": integerParameter(1, listFileMaxEntries, "Max entries; start small; increase only as needed; default 200."),
		},
	)
}

// executeListFile validates arguments and returns a compact bounded listing.
func (e *ToolExecutor) executeListFile(ctx context.Context, tc ToolCall) ToolResult {
	var args struct {
		Path       string `json:"path"`
		Depth      int    `json:"depth"`
		MaxEntries int    `json:"max_entries"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return ToolResult{Output: fmt.Sprintf("error: invalid arguments: %v", err), Status: ToolFailed}
	}
	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		args.Path = "."
	}
	if args.Depth == 0 {
		args.Depth = listFileDefaultDepth
	}
	if args.Depth < 1 || args.Depth > listFileMaxDepth {
		return failedListFileResult(args.Path, fmt.Errorf("depth must be between 1 and %d", listFileMaxDepth))
	}
	if args.MaxEntries == 0 {
		args.MaxEntries = listFileDefaultEntries
	}
	if args.MaxEntries < 1 || args.MaxEntries > listFileMaxEntries {
		return failedListFileResult(args.Path, fmt.Errorf("max_entries must be between 1 and %d", listFileMaxEntries))
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{Input: args.Path, Output: fmt.Sprintf("error: list aborted: %v", err), Status: ToolAborted}
	}

	root, err := filepath.Abs(".")
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return failedListFileResult(args.Path, fmt.Errorf("resolve working directory: %w", err))
	}
	directory, err := safeListDirectoryPath(root, args.Path)
	if err != nil {
		return failedListFileResult(args.Path, err)
	}

	type entry struct {
		path string
		kind byte
	}
	entries := make([]entry, 0, args.MaxEntries)
	walkErr := filepath.WalkDir(directory, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == directory {
			return nil
		}
		rel, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		if isSkippedListDirectory(d, rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		depth := listPathDepth(rel)
		if depth > args.Depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		kind := byte('F')
		if d.IsDir() {
			kind = 'D'
		} else if d.Type()&os.ModeSymlink != 0 {
			kind = 'S'
		}
		entries = append(entries, entry{path: filepath.ToSlash(rel), kind: kind})
		if len(entries) >= args.MaxEntries {
			return errListLimit
		}
		return nil
	})
	if errors.Is(walkErr, errListLimit) {
		walkErr = nil
	}
	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return ToolResult{Input: args.Path, Output: fmt.Sprintf("error: list aborted: %v", walkErr), Status: ToolAborted}
		}
		return failedListFileResult(args.Path, fmt.Errorf("list %q: %w", args.Path, walkErr))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })

	var output strings.Builder
	fmt.Fprintf(&output, "%s [depth=%d]:\n", filepath.ToSlash(args.Path), args.Depth)
	for _, item := range entries {
		line := fmt.Sprintf("%c %s\n", item.kind, item.path)
		if output.Len()+len(line) > listFileMaxOutput {
			output.WriteString("[truncated; request a smaller scope]\n")
			break
		}
		output.WriteString(line)
	}
	if len(entries) == 0 {
		output.WriteString("(empty)\n")
	}
	if len(entries) >= args.MaxEntries {
		output.WriteString(fmt.Sprintf("[max_entries=%d; request a smaller scope]\n", args.MaxEntries))
	}
	return ToolResult{Input: args.Path, Output: output.String(), Status: ToolCompleted}
}

var errListLimit = errors.New("list entry limit reached")

func failedListFileResult(path string, err error) ToolResult {
	return ToolResult{Input: path, Output: fmt.Sprintf("error: %v", err), Status: ToolFailed}
}

func safeListDirectoryPath(root, name string) (string, error) {
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("unsafe path %q: paths must be relative", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path %q: path escapes the working directory", name)
	}
	full := filepath.Join(root, clean)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", name, err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path %q: symlink escapes the working directory", name)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", name, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", name)
	}
	return resolved, nil
}

func isSkippedListDirectory(d fs.DirEntry, relative string) bool {
	base := filepath.Base(relative)
	return d.IsDir() && base == ".git"
}

func listPathDepth(path string) int {
	depth := 1
	for _, separator := range path {
		if separator == '/' || separator == '\\' {
			depth++
		}
	}
	return depth
}
