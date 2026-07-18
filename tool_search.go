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
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	searchDefaultMaxResults = 100
	searchMaxResults        = 500
	searchDefaultContext    = 0
	searchMaxContext        = 3
	searchMaxOutput         = 32 * 1024
	searchMaxFileBytes      = 4 * 1024 * 1024
	searchProbeBytes        = 8 * 1024
	searchMaxLineBytes      = 1024 * 1024
)

var (
	errSearchResultLimit = errors.New("search result limit reached")
	errSearchOutputLimit = errors.New("search output limit reached")
)

// searchTool returns a bounded text search tool for focused repository inspection.
func searchTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name: "search",
			Description: "Search bounded UTF-8 text files under a relative path. Prefer this before " +
				"read_file when a symbol, error, or text location is unknown. Restrict path to the " +
				"smallest relevant scope, use literal matching by default, keep max_results small, " +
				"and add context only when needed. Regex and case-insensitive matching are opt-in. " +
				"Skips .git and common dependency/generated directories.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Text or regular expression",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "Smallest relevant relative file or directory; defaults to .",
					},
					"case_sensitive": map[string]any{
						"type":        "boolean",
						"default":     true,
						"description": "Case-sensitive; default true",
					},
					"regex": map[string]any{
						"type":        "boolean",
						"default":     false,
						"description": "Treat as regex; default false",
					},
					"max_results": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     searchMaxResults,
						"default":     searchDefaultMaxResults,
						"description": "Maximum matching lines; keep small and increase only if results are incomplete; default 100",
					},
					"context_lines": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"maximum":     searchMaxContext,
						"default":     searchDefaultContext,
						"description": "Surrounding context lines; start at 0 and add only when needed; default 0",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

// executeSearch validates arguments and performs a bounded workspace search.
func (e *ToolExecutor) executeSearch(ctx context.Context, tc ToolCall) ToolResult {
	var args struct {
		Query         string `json:"query"`
		Path          string `json:"path"`
		CaseSensitive *bool  `json:"case_sensitive"`
		Regex         bool   `json:"regex"`
		MaxResults    int    `json:"max_results"`
		ContextLines  int    `json:"context_lines"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return ToolResult{Output: fmt.Sprintf("error: invalid arguments: %v", err), Status: ToolFailed}
	}
	if strings.TrimSpace(args.Query) == "" {
		return failedSearchResult(args.Query, errors.New("query is required"))
	}
	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		args.Path = "."
	}
	if args.MaxResults == 0 {
		args.MaxResults = searchDefaultMaxResults
	}
	if args.MaxResults < 1 || args.MaxResults > searchMaxResults {
		return failedSearchResult(args.Query, fmt.Errorf("max_results must be between 1 and %d", searchMaxResults))
	}
	if args.ContextLines < 0 || args.ContextLines > searchMaxContext {
		return failedSearchResult(args.Query, fmt.Errorf("context_lines must be between 0 and %d", searchMaxContext))
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{Input: args.Query, Output: fmt.Sprintf("error: search aborted: %v", err), Status: ToolAborted}
	}

	var matcher func(string) bool
	caseSensitive := true
	if args.CaseSensitive != nil {
		caseSensitive = *args.CaseSensitive
	}
	if args.Regex {
		pattern := args.Query
		if !caseSensitive {
			pattern = "(?i:" + pattern + ")"
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return failedSearchResult(args.Query, fmt.Errorf("invalid regex: %w", err))
		}
		matcher = re.MatchString
	} else {
		query := args.Query
		if !caseSensitive {
			query = strings.ToLower(query)
		}
		matcher = func(line string) bool {
			if !caseSensitive {
				line = strings.ToLower(line)
			}
			return strings.Contains(line, query)
		}
	}

	root, err := filepath.Abs(".")
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return failedSearchResult(args.Query, fmt.Errorf("resolve working directory: %w", err))
	}
	searchRoot, err := safeSearchPath(root, args.Path)
	if err != nil {
		return failedSearchResult(args.Path, err)
	}

	var output strings.Builder
	fmt.Fprintf(&output, "search %q in %q:\n", args.Query, filepath.ToSlash(args.Path))
	matched := 0
	walkErr := filepath.WalkDir(searchRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path != searchRoot && entry.IsDir() && isSkippedSearchDirectory(entry.Name()) {
			return filepath.SkipDir
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > searchMaxFileBytes {
			return nil
		}
		remaining := args.MaxResults - matched
		matches, lines, err := searchFile(ctx, path, matcher, remaining)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if err := appendSearchResults(&output, filepath.ToSlash(relative), lines, matches, args.ContextLines); err != nil {
			return err
		}
		matched += len(matches)
		if matched >= args.MaxResults {
			return errSearchResultLimit
		}
		return nil
	})

	if errors.Is(walkErr, errSearchResultLimit) {
		output.WriteString(fmt.Sprintf("[result limit reached: %d; narrow the path or raise max_results]\n", args.MaxResults))
		walkErr = nil
	} else if errors.Is(walkErr, errSearchOutputLimit) {
		output.WriteString("[output truncated at 32 KiB; narrow the path or reduce context_lines]\n")
		walkErr = nil
	}
	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return ToolResult{Input: args.Query, Output: fmt.Sprintf("error: search aborted: %v", walkErr), Status: ToolAborted}
		}
		return failedSearchResult(args.Path, fmt.Errorf("search %q: %w", args.Path, walkErr))
	}
	if matched == 0 {
		output.WriteString("(no matches)\n")
	}
	return ToolResult{Input: args.Query, Output: output.String(), Status: ToolCompleted}
}

func searchFile(ctx context.Context, path string, matcher func(string) bool, maxMatches int) ([]int, []string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	probe := make([]byte, searchProbeBytes)
	n, readErr := file.Read(probe)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, nil, readErr
	}
	if bytesLookBinary(probe[:n]) || !utf8.Valid(probe[:n]) {
		return nil, nil, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), searchMaxLineBytes)
	lines := make([]string, 0)
	matches := make([]int, 0)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		line := scanner.Text()
		if !utf8.ValidString(line) {
			return nil, nil, nil
		}
		lines = append(lines, line)
		if matcher(line) {
			matches = append(matches, len(lines)-1)
			if len(matches) >= maxMatches {
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// Skip unusually long lines rather than failing an otherwise valid repository search.
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return matches, lines, nil
}

func appendSearchResults(output *strings.Builder, path string, lines []string, matches []int, contextLines int) error {
	matchSet := make(map[int]bool, len(matches))
	for _, line := range matches {
		matchSet[line] = true
	}
	ranges := make([][2]int, 0, len(matches))
	for _, line := range matches {
		start, end := line-contextLines, line+contextLines
		if start < 0 {
			start = 0
		}
		if end >= len(lines) {
			end = len(lines) - 1
		}
		if len(ranges) > 0 && start <= ranges[len(ranges)-1][1]+1 {
			if end > ranges[len(ranges)-1][1] {
				ranges[len(ranges)-1][1] = end
			}
			continue
		}
		ranges = append(ranges, [2]int{start, end})
	}
	for _, searchRange := range ranges {
		for line := searchRange[0]; line <= searchRange[1]; line++ {
			separator := "-"
			if matchSet[line] {
				separator = ":"
			}
			entry := fmt.Sprintf("%s:%d%s %s\n", path, line+1, separator, lines[line])
			if output.Len()+len(entry) > searchMaxOutput {
				return errSearchOutputLimit
			}
			output.WriteString(entry)
		}
	}
	return nil
}

func bytesLookBinary(data []byte) bool {
	return strings.IndexByte(string(data), 0) >= 0
}

func safeSearchPath(root, name string) (string, error) {
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
	if _, err := os.Stat(resolved); err != nil {
		return "", fmt.Errorf("stat %q: %w", name, err)
	}
	return resolved, nil
}

func isSkippedSearchDirectory(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".hg", ".svn", "node_modules", "vendor", "dist", "build", "target", "coverage":
		return true
	default:
		return false
	}
}

func failedSearchResult(input string, err error) ToolResult {
	return ToolResult{Input: input, Output: fmt.Sprintf("error: %v", err), Status: ToolFailed}
}
