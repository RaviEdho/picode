package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	readFileDefaultLines = 200
	readFileMaxLines     = 200
	readFileMaxLineBytes = 4096
	readFileMaxOutput    = 32 * 1024
)

// readFileTool returns bounded, numbered text without the shell overhead of run_command.
func readFileTool() Tool {
	return functionTool(
		"read_file",
		"Focused numbered lines from a UTF-8 text file in cwd. Search first if location unknown; smallest relevant range; don't reread unchanged content. Paths relative; output capped at 200 lines/32 KiB. run_command for binaries/commands.",
		map[string]any{
			"path":       stringParameter("Relative file path under cwd."),
			"start_line": integerParameter(1, 0, "First 1-based line; use search's location when possible; default 1."),
			"end_line":   integerParameter(1, 0, "Last 1-based line; request the smallest useful range; default 200 lines after start_line."),
		},
		"path",
	)
}

// executeReadFile validates arguments and reads a bounded section of a file.
func (e *ToolExecutor) executeReadFile(ctx context.Context, tc ToolCall) ToolResult {
	var args struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return ToolResult{Output: fmt.Sprintf("error: invalid arguments: %v", err), Status: ToolFailed}
	}

	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		return failedReadFileResult(args.Path, errors.New("path is required"))
	}
	if args.StartLine == 0 {
		args.StartLine = 1
	}
	if args.StartLine < 1 {
		return failedReadFileResult(args.Path, errors.New("start_line must be at least 1"))
	}
	if args.EndLine < 0 {
		return failedReadFileResult(args.Path, errors.New("end_line must be at least 1"))
	}
	if args.EndLine == 0 {
		args.EndLine = maxInt()
		if args.StartLine <= maxInt()-(readFileDefaultLines-1) {
			args.EndLine = args.StartLine + readFileDefaultLines - 1
		}
	}
	if args.EndLine < args.StartLine {
		return failedReadFileResult(args.Path, errors.New("end_line must not be before start_line"))
	}
	if args.EndLine-args.StartLine+1 > readFileMaxLines {
		args.EndLine = args.StartLine + readFileMaxLines - 1
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{Input: args.Path, Output: fmt.Sprintf("error: read aborted: %v", err), Status: ToolAborted}
	}

	root, err := filepath.Abs(".")
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return failedReadFileResult(args.Path, fmt.Errorf("resolve working directory: %w", err))
	}
	path, err := safeReadFilePath(root, args.Path)
	if err != nil {
		return failedReadFileResult(args.Path, err)
	}

	file, err := os.Open(path)
	if err != nil {
		return failedReadFileResult(args.Path, fmt.Errorf("open %q: %w", args.Path, err))
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var output strings.Builder
	returned, lineNumber := 0, 0
	truncatedOutput := false
	for {
		if err := ctx.Err(); err != nil {
			return ToolResult{Input: args.Path, Output: fmt.Sprintf("error: read aborted: %v", err), Status: ToolAborted}
		}
		line, lineTruncated, readErr := readBoundedLine(reader)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return failedReadFileResult(args.Path, fmt.Errorf("read %q: %w", args.Path, readErr))
		}
		lineNumber++
		if !utf8.ValidString(line) {
			// Trim only an incomplete rune at the bounded suffix; reject malformed bytes elsewhere.
			if lineTruncated {
				line = trimIncompleteUTF8Suffix(line)
			}
			if !utf8.ValidString(line) {
				return failedReadFileResult(args.Path, fmt.Errorf("%q is not valid UTF-8", args.Path))
			}
		}
		if lineNumber < args.StartLine {
			continue
		}
		if lineNumber > args.EndLine {
			break
		}
		if lineTruncated {
			line += "…[line truncated]"
		}
		entry := fmt.Sprintf("%d| %s\n", lineNumber, line)
		if output.Len()+len(entry) > readFileMaxOutput {
			truncatedOutput = true
			break
		}
		output.WriteString(entry)
		returned++
	}

	if returned == 0 {
		output.WriteString("(none)\n")
	}
	header := fmt.Sprintf("%s [%d-%d]:\n", filepath.ToSlash(args.Path), args.StartLine, args.StartLine+returned-1)
	if returned == 0 {
		header = fmt.Sprintf("%s [%d-%d requested]:\n", filepath.ToSlash(args.Path), args.StartLine, args.EndLine)
	}
	if truncatedOutput {
		output.WriteString("[truncated; request a smaller range]\n")
	}
	return ToolResult{Input: args.Path, Output: header + output.String(), Status: ToolCompleted}
}

func maxInt() int { return int(^uint(0) >> 1) }

func trimIncompleteUTF8Suffix(line string) string {
	start := len(line) - 4
	if start < 0 {
		start = 0
	}
	for i := start; i < len(line); i++ {
		if utf8.RuneStart(line[i]) && !utf8.FullRuneInString(line[i:]) {
			return line[:i]
		}
	}
	return line
}

func failedReadFileResult(path string, err error) ToolResult {
	return ToolResult{Input: path, Output: fmt.Sprintf("error: %v", err), Status: ToolFailed}
}

// safeReadFilePath permits only files whose resolved path remains below root.
func safeReadFilePath(root, name string) (string, error) {
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("unsafe path %q: paths must be relative", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
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
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", name)
	}
	return resolved, nil
}

// readBoundedLine consumes a complete line but retains a bounded prefix to cap memory use.
func readBoundedLine(reader *bufio.Reader) (string, bool, error) {
	line := make([]byte, 0, readFileMaxLineBytes)
	truncated := false
	for {
		part, prefix, err := reader.ReadLine()
		if len(part) > 0 && len(line) < readFileMaxLineBytes {
			remaining := readFileMaxLineBytes - len(line)
			if len(part) > remaining {
				line = append(line, part[:remaining]...)
				truncated = true
			} else {
				line = append(line, part...)
			}
		}
		if len(part) > 0 && len(line) >= readFileMaxLineBytes && prefix {
			truncated = true
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return string(line), truncated, nil
			}
			return "", false, err
		}
		if !prefix {
			return string(line), truncated, nil
		}
	}
}
