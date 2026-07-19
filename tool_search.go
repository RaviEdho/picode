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
	return functionTool(
		"search",
		"Bounded UTF-8 text search under a relative path. Use before read_file when a symbol, error, or location is unknown. Use the smallest scope, literal matching by default, small max_results, and context only as needed. Regex/case-insensitive matching are opt-in. Skips .git and common dependency/generated dirs. When path is omitted, the query also matches file and directory basenames, so a pathless query can locate a file by name.",
		map[string]any{
			"query":          stringParameter("Text or regex."),
			"path":           stringParameter("Smallest relevant relative file/dir; default ."),
			"case_sensitive": booleanParameter(true, "Case-sensitive; default true."),
			"regex":          booleanParameter(false, "Treat as regex; default false."),
			"max_results":    integerParameter(1, searchMaxResults, "Matching lines; keep small, increase only if incomplete; default 100.", searchDefaultMaxResults),
			"context_lines":  integerParameter(0, searchMaxContext, "Context lines; start at 0, add only as needed; default 0.", searchDefaultContext),
		},
		"query",
	)
}

// searchArgs holds the validated parameters for a search call.
type searchArgs struct {
	Query         string `json:"query"`
	Path          string `json:"path"`
	CaseSensitive *bool  `json:"case_sensitive"`
	Regex         bool   `json:"regex"`
	MaxResults    int    `json:"max_results"`
	ContextLines  int    `json:"context_lines"`
}

// executeSearch validates arguments and performs a bounded workspace search.
func (e *ToolExecutor) executeSearch(ctx context.Context, tc ToolCall) ToolResult {
	var args searchArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return ToolResult{Output: fmt.Sprintf("error: invalid arguments: %v", err), Status: ToolFailed}
	}
	if strings.TrimSpace(args.Query) == "" {
		return failedSearchResult(args.Query, errors.New("query is required"))
	}
	args.Path = strings.TrimSpace(args.Path)
	matchFilenames := args.Path == ""
	if matchFilenames {
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
		isDir := entry.IsDir()
		isSymlink := entry.Type()&os.ModeSymlink != 0

		// When no path was supplied, the query also matches file and directory basenames,
		// so the path to a matched named entry can be located without a content hit.
		if matchFilenames && !isSymlink && path != searchRoot && matcher(filepath.Base(path)) {
			if relative, err := filepath.Rel(root, path); err != nil {
				return err
			} else {
				line := fmt.Sprintf("%c %s\n", searchEntryKind(entry), filepath.ToSlash(relative))
				if output.Len()+len(line) > searchMaxOutput {
					return errSearchOutputLimit
				}
				output.WriteString(line)
				matched++
				if matched >= args.MaxResults {
					return errSearchResultLimit
				}
			}
		}
		if isDir || isSymlink {
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
		output.WriteString(fmt.Sprintf("[max_results=%d; narrow path or raise limit]\n", args.MaxResults))
		walkErr = nil
	} else if errors.Is(walkErr, errSearchOutputLimit) {
		output.WriteString("[truncated; narrow path or reduce context]\n")
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

func searchEntryKind(entry os.DirEntry) byte {
	if entry.IsDir() {
		return 'D'
	}
	if entry.Type()&os.ModeSymlink != 0 {
		return 'S'
	}
	return 'F'
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
	header := fmt.Sprintf("%s:\n", path)
	if output.Len()+len(header) > searchMaxOutput {
		return errSearchOutputLimit
	}
	output.WriteString(header)
	for _, searchRange := range ranges {
		for line := searchRange[0]; line <= searchRange[1]; line++ {
			separator := "-"
			if matchSet[line] {
				separator = ":"
			}
			entry := fmt.Sprintf("%d%s %s\n", line+1, separator, lines[line])
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
