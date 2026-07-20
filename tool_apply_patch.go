package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

type patchOperationKind byte

const (
	patchAdd patchOperationKind = iota
	patchUpdate
	patchDelete
)

type patchOperation struct {
	kind  patchOperationKind
	path  string
	lines []string
	hunks []patchHunk
}

type patchHunk struct {
	anchor string
	lines  []patchLine
}

type patchLine struct {
	kind byte
	text string
}

type patchPlan struct {
	kind     patchOperationKind
	path     string
	fullPath string
	content  []byte
	original []byte
	mode     os.FileMode
}

// applyPatchTool returns the OpenAI schema for safe, structured file edits.
func applyPatchTool() Tool {
	return functionTool(
		"apply_patch",
		"Apply the smallest complete structured patch in cwd. Inspect first; preserve unrelated changes; combine related edits; don't rewrite unnecessarily. Use *** Begin Patch/End Patch with Add File, Update File, or Delete File sections. Update @@ hunks prefix lines with space (context), + (add), or - (remove); use @@ # Section as an exact section anchor when useful. A removed Markdown bullet starts with -- so the literal bullet dash is preserved. Paths are relative; validate before changing files.",
		map[string]any{
			"patch": stringParameter("Complete structured patch to apply."),
		},
		"patch",
	)
}

// executeApplyPatch decodes, validates, and applies an apply_patch tool call.
func (e *ToolExecutor) executeApplyPatch(ctx context.Context, tc ToolCall) ToolResult {
	var args struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return failedPatchResult("", invalidToolError("arguments", "must be valid JSON", err))
	}
	if err := ctx.Err(); err != nil {
		return toolAbortedResult(args.Patch, err)
	}

	operations, err := parsePatch(args.Patch)
	if err != nil {
		return failedPatchResult(args.Patch, invalidToolError("patch", "must be a complete structured patch", err))
	}
	root, err := filepath.Abs(".")
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return failedPatchResult(args.Patch, ioToolError(fmt.Errorf("resolve working directory: %w", err)))
	}
	plans, err := preparePatch(root, operations)
	if err != nil {
		return failedPatchResult(args.Patch, err)
	}
	if err := ctx.Err(); err != nil {
		return toolAbortedResult(args.Patch, err)
	}
	if err := commitPatch(plans); err != nil {
		return failedPatchResult(args.Patch, ioToolError(err))
	}

	var output strings.Builder
	additions, deletions := 0, 0
	output.WriteString("applied:")
	for _, plan := range plans {
		marker := byte('M')
		if plan.kind == patchAdd {
			marker = 'A'
		} else if plan.kind == patchDelete {
			marker = 'D'
		}
		if plan.kind == patchAdd {
			additions += countLines(plan.content)
		} else if plan.kind == patchDelete {
			deletions += countLines(plan.original)
		} else {
			additions += countLineDelta(plan.original, plan.content, true)
			deletions += countLineDelta(plan.original, plan.content, false)
		}
		fmt.Fprintf(&output, "\n%c %s", marker, filepath.ToSlash(plan.path))
	}
	fmt.Fprintf(&output, "\ndiff +%d -%d (%d files)", additions, deletions, len(plans))
	return ToolResult{Input: args.Patch, Output: output.String(), Status: ToolCompleted}
}

func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	return strings.Count(string(content), "\n") + 1
}

func countLineDelta(before, after []byte, additions bool) int {
	oldLines := strings.Split(strings.ReplaceAll(string(before), "\r\n", "\n"), "\n")
	newLines := strings.Split(strings.ReplaceAll(string(after), "\r\n", "\n"), "\n")
	if len(oldLines) > 0 && oldLines[len(oldLines)-1] == "" {
		oldLines = oldLines[:len(oldLines)-1]
	}
	if len(newLines) > 0 && newLines[len(newLines)-1] == "" {
		newLines = newLines[:len(newLines)-1]
	}
	common := len(oldLines)
	if len(newLines) < common {
		common = len(newLines)
	}
	if additions {
		return positiveDelta(len(newLines) - common)
	}
	return positiveDelta(len(oldLines) - common)
}

func positiveDelta(value int) int {
	if value > 0 {
		return value
	}
	return 0
}

func failedPatchResult(patch string, err error) ToolResult {
	output := err.Error()
	if !strings.HasPrefix(output, "invalid ") && !strings.HasPrefix(output, "bad path ") && !strings.HasPrefix(output, "io error:") {
		output = invalidToolError("patch", "must be a valid structured patch", err).Error()
	}
	return ToolResult{Input: patch, Output: output, Status: ToolFailed}
}

func parsePatch(text string) ([]patchOperation, error) {
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("patch is not valid UTF-8")
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	// Ignore whitespace-only lines after the sentinel, but do not accept any
	// other trailing data. This makes the sentinel check explicit and prevents
	// truncated or contaminated tool output from being treated as a patch.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 || lines[0] != "*** Begin Patch" || lines[len(lines)-1] != "*** End Patch" {
		return nil, fmt.Errorf("patch must start with *** Begin Patch and end with *** End Patch")
	}

	var operations []patchOperation
	seen := make(map[string]bool)
	for i := 1; i < len(lines)-1; {
		kind, path, ok := parsePatchHeader(lines[i])
		if !ok {
			return nil, fmt.Errorf("line %d: expected a file operation", i+1)
		}
		if path == "" {
			return nil, fmt.Errorf("line %d: file path is empty", i+1)
		}
		if seen[path] {
			return nil, fmt.Errorf("line %d: duplicate operation for %q", i+1, path)
		}
		seen[path] = true
		op := patchOperation{kind: kind, path: path}
		i++

		switch kind {
		case patchAdd:
			for i < len(lines)-1 && !isPatchHeader(lines[i]) {
				if !strings.HasPrefix(lines[i], "+") {
					return nil, fmt.Errorf("line %d: added file lines must begin with +", i+1)
				}
				line := lines[i][1:]
				if err := validateAddedFileLine(path, i+1, line); err != nil {
					return nil, err
				}
				op.lines = append(op.lines, line)
				i++
			}
		case patchDelete:
			if i < len(lines)-1 && !isPatchHeader(lines[i]) {
				return nil, fmt.Errorf("line %d: delete operation cannot contain content", i+1)
			}
		case patchUpdate:
			for i < len(lines)-1 && !isPatchHeader(lines[i]) {
				if !strings.HasPrefix(lines[i], "@@") {
					return nil, fmt.Errorf("line %d: expected @@ hunk header", i+1)
				}
				i++
				hunk := patchHunk{anchor: parsePatchHunkAnchor(lines[i-1])}
				for i < len(lines)-1 && !isPatchHeader(lines[i]) && !strings.HasPrefix(lines[i], "@@") {
					if lines[i] == "\\ No newline at end of file" {
						return nil, fmt.Errorf("line %d: no-newline markers are not supported", i+1)
					}
					if lines[i] == "" || !strings.ContainsRune(" +-", rune(lines[i][0])) {
						return nil, fmt.Errorf("line %d: hunk lines must begin with space, +, or -", i+1)
					}
					hunk.lines = append(hunk.lines, patchLine{kind: lines[i][0], text: lines[i][1:]})
					i++
				}
				if len(hunk.lines) == 0 {
					return nil, fmt.Errorf("empty hunk for %q", path)
				}
				op.hunks = append(op.hunks, hunk)
			}
			if len(op.hunks) == 0 {
				return nil, fmt.Errorf("update operation for %q has no hunks", path)
			}
		}
		operations = append(operations, op)
	}
	if len(operations) == 0 {
		return nil, fmt.Errorf("patch contains no file operations")
	}
	return operations, nil
}

func parsePatchHeader(line string) (patchOperationKind, string, bool) {
	for _, candidate := range []struct {
		prefix string
		kind   patchOperationKind
	}{{"*** Add File: ", patchAdd}, {"*** Update File: ", patchUpdate}, {"*** Delete File: ", patchDelete}} {
		if strings.HasPrefix(line, candidate.prefix) {
			return candidate.kind, strings.TrimSpace(strings.TrimPrefix(line, candidate.prefix)), true
		}
	}
	return 0, "", false
}

func isPatchHeader(line string) bool {
	_, _, ok := parsePatchHeader(line)
	return ok
}

// parsePatchHunkAnchor returns the optional text following @@. A structured
// patch commonly uses a heading (for example, "@@ # Tools") as an anchor.
// Keep conventional line-number hunk headers as metadata so accepting one
// does not unexpectedly narrow the search range.
func parsePatchHunkAnchor(header string) string {
	anchor := strings.TrimSpace(strings.TrimPrefix(header, "@@"))
	if anchor == "" || isUnifiedHunkRange(anchor) || !isMarkdownHeading(anchor) {
		return ""
	}
	return anchor
}

func isUnifiedHunkRange(header string) bool {
	fields := strings.Fields(header)
	if len(fields) < 2 || !isHunkRange(fields[0], '-') || !isHunkRange(fields[1], '+') {
		return false
	}
	return len(fields) == 2 || (len(fields) >= 3 && fields[2] == "@@")
}

func isHunkRange(value string, prefix byte) bool {
	if len(value) < 2 || value[0] != prefix {
		return false
	}
	value = value[1:]
	commaSeen := false
	for i := 0; i < len(value); i++ {
		if value[i] == ',' && !commaSeen {
			commaSeen = true
			if i == 0 || i == len(value)-1 {
				return false
			}
			continue
		}
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// validateAddedFileLine rejects common signs that a patch body was truncated
// or contaminated by the tool-call wrapper. It intentionally only rejects
// marker-shaped fragments: ellipses are valid source and prose in many other
// positions.
func validateAddedFileLine(path string, lineNumber int, line string) error {
	trimmed := strings.TrimSpace(line)
	if trimmed == "</parameter>" || trimmed == "endregion" || trimmed == "#endregion" {
		return fmt.Errorf("line %d: add %q contains a possible truncated tool-output marker %q", lineNumber, path, trimmed)
	}
	if trimmed == "..." {
		return fmt.Errorf("line %d: add %q contains a possible truncation marker", lineNumber, path)
	}
	for offset := 0; ; {
		relative := strings.Index(line[offset:], "...")
		if relative < 0 {
			break
		}
		index := offset + relative
		before, after := line[:index], line[index+3:]
		if strings.TrimSpace(before) == "" || strings.TrimSpace(after) == "" {
			return fmt.Errorf("line %d: add %q contains a possible truncation marker", lineNumber, path)
		}
		beforeRunes := []rune(before)
		if !unicode.IsSpace(beforeRunes[len(beforeRunes)-1]) {
			afterRunes := []rune(strings.TrimLeftFunc(after, unicode.IsSpace))
			if len(afterRunes) > 0 && (unicode.IsLetter(afterRunes[0]) || unicode.IsDigit(afterRunes[0])) {
				return fmt.Errorf("line %d: add %q contains a possible truncation marker", lineNumber, path)
			}
		}
		offset = index + 3
	}
	return nil
}

func preparePatch(root string, operations []patchOperation) ([]patchPlan, error) {
	plans := make([]patchPlan, 0, len(operations))
	seen := make(map[string]bool)
	for _, op := range operations {
		fullPath, err := safePatchPath(root, op.path, op.kind == patchAdd)
		if err != nil {
			return nil, err
		}
		if seen[fullPath] {
			return nil, fmt.Errorf("duplicate operation for %q", op.path)
		}
		seen[fullPath] = true
		plan := patchPlan{kind: op.kind, path: op.path, fullPath: fullPath}
		info, statErr := os.Lstat(fullPath)
		switch op.kind {
		case patchAdd:
			if statErr == nil {
				return nil, fmt.Errorf("add %q: file already exists", op.path)
			}
			if !os.IsNotExist(statErr) {
				return nil, fmt.Errorf("add %q: %w", op.path, statErr)
			}
			plan.mode = 0o644
			plan.content = []byte(strings.Join(op.lines, "\n") + "\n")
		case patchUpdate, patchDelete:
			if statErr != nil {
				return nil, ioToolError(fmt.Errorf("open %q: %w", op.path, statErr))
			}
			if !info.Mode().IsRegular() {
				return nil, ioToolError(fmt.Errorf("%q is not a regular file", op.path))
			}
			plan.mode = info.Mode().Perm()
			plan.original, err = os.ReadFile(fullPath)
			if err != nil {
				return nil, ioToolError(fmt.Errorf("read %q: %w", op.path, err))
			}
			if !utf8.Valid(plan.original) {
				return nil, ioToolError(fmt.Errorf("%q is not valid UTF-8", op.path))
			}
			if op.kind == patchUpdate {
				plan.content, err = applyPatchHunks(op.path, plan.original, op.hunks)
				if err != nil {
					return nil, err
				}
			}
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func safePatchPath(root, name string, adding bool) (string, error) {
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", badPathToolError(name, "must be relative to the working directory")
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", badPathToolError(name, "must be relative to the working directory")
	}
	full := filepath.Join(root, clean)
	check := full
	if adding {
		check = filepath.Dir(full)
	}
	resolved, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", ioToolError(fmt.Errorf("resolve %q: %w", name, err))
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", badPathToolError(name, "must be relative to the working directory")
	}
	if !adding {
		if info, err := os.Lstat(full); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", badPathToolError(name, "symbolic links cannot be patched; use the target's relative path")
		}
	}
	return full, nil
}

func applyPatchHunks(path string, content []byte, hunks []patchHunk) ([]byte, error) {
	newline := "\n"
	text := string(content)
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	hadFinalNewline := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(content) == 0 {
		lines = nil
	}
	searchFrom := 0
	for hunkIndex, hunk := range hunks {
		var oldLines, newLines []string
		for _, line := range hunk.lines {
			if line.kind != '+' {
				oldLines = append(oldLines, line.text)
			}
			if line.kind != '-' {
				newLines = append(newLines, line.text)
			}
		}
		if len(oldLines) == 0 {
			return nil, fmt.Errorf("update %q hunk %d: insertion requires context", path, hunkIndex+1)
		}
		contextStart := searchFrom
		contextEnd := len(lines)
		if hunk.anchor != "" {
			anchorLine, anchorEnd, err := findPatchAnchor(lines, hunk.anchor, searchFrom)
			if err != nil {
				return nil, fmt.Errorf("update %q hunk %d: %w", path, hunkIndex+1, err)
			}
			// The anchor names the section containing the hunk; hunk lines
			// describe content after the heading, not the heading itself.
			anchorContentStart := anchorLine + 1
			if len(oldLines) > 0 && oldLines[0] == hunk.anchor {
				anchorContentStart = anchorLine
			}
			if anchorContentStart > contextStart {
				contextStart = anchorContentStart
			}
			if anchorEnd < contextEnd {
				contextEnd = anchorEnd
			}
			// A trailing heading is useful context for a change at the end of
			// a section. Permit that one boundary line without allowing the
			// hunk to search through the following section.
			if contextEnd < len(lines) && len(oldLines) > 0 && lines[contextEnd] == oldLines[len(oldLines)-1] {
				contextEnd++
			}
		}
		match := -1
		for i := contextStart; i+len(oldLines) <= contextEnd; i++ {
			if equalLines(lines[i:i+len(oldLines)], oldLines) {
				if match >= 0 {
					return nil, fmt.Errorf("update %q hunk %d: context is ambiguous", path, hunkIndex+1)
				}
				match = i
			}
		}
		if match < 0 {
			return nil, contextMismatchError(path, hunkIndex, lines, oldLines, contextStart, contextEnd)
		}
		updated := make([]string, 0, len(lines)-len(oldLines)+len(newLines))
		updated = append(updated, lines[:match]...)
		updated = append(updated, newLines...)
		updated = append(updated, lines[match+len(oldLines):]...)
		lines = updated
		searchFrom = match + len(newLines)
	}
	result := strings.Join(lines, "\n")
	if hadFinalNewline {
		result += "\n"
	}
	if newline == "\r\n" {
		result = strings.ReplaceAll(result, "\n", newline)
	}
	return []byte(result), nil
}

func findPatchAnchor(lines []string, anchor string, searchFrom int) (int, int, error) {
	for i := searchFrom; i < len(lines); i++ {
		if lines[i] == anchor {
			return i, patchAnchorEnd(lines, anchor, i), nil
		}
	}
	// A later hunk may use the same section anchor after an earlier hunk has
	// already moved the search position past the heading.
	for i := searchFrom - 1; i >= 0; i-- {
		if lines[i] == anchor {
			return i, patchAnchorEnd(lines, anchor, i), nil
		}
	}
	return -1, -1, fmt.Errorf("anchor %q did not match", anchor)
}

func patchAnchorEnd(lines []string, anchor string, anchorLine int) int {
	if !isMarkdownHeading(anchor) {
		return len(lines)
	}
	for i := anchorLine + 1; i < len(lines); i++ {
		if isMarkdownHeading(lines[i]) {
			return i
		}
	}
	return len(lines)
}

func isMarkdownHeading(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '#' {
		return false
	}
	return len(trimmed) == 1 || trimmed[1] == '#' || trimmed[1] == ' ' || trimmed[1] == '\t'
}

func contextMismatchError(path string, hunkIndex int, lines, expected []string, searchFrom, searchEnd int) error {
	start := nearestContextStart(lines, expected, searchFrom, searchEnd)
	for offset, want := range expected {
		lineNumber := start + offset + 1
		if start+offset >= len(lines) {
			return fmt.Errorf("update %q hunk %d: context did not match; file ended at line %d, expected %q", path, hunkIndex+1, len(lines), want)
		}
		if lines[start+offset] != want {
			return fmt.Errorf("update %q hunk %d: context did not match; line %d is %q, expected %q", path, hunkIndex+1, lineNumber, lines[start+offset], want)
		}
	}
	return fmt.Errorf("update %q hunk %d: context did not match", path, hunkIndex+1)
}

func nearestContextStart(lines, expected []string, searchFrom, searchEnd int) int {
	maxStart := searchEnd - len(expected)
	if maxStart < searchFrom {
		maxStart = searchFrom
	}
	bestStart, bestMatches := searchFrom, -1
	for start := searchFrom; start <= maxStart; start++ {
		matches := 0
		for offset, want := range expected {
			if start+offset < len(lines) && lines[start+offset] == want {
				matches++
			}
		}
		if matches > bestMatches {
			bestStart, bestMatches = start, matches
		}
	}
	return bestStart
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func commitPatch(plans []patchPlan) error {
	committed := 0
	for i, plan := range plans {
		var err error
		written := false
		if plan.kind == patchDelete {
			err = os.Remove(plan.fullPath)
		} else {
			err = atomicWriteFile(plan.fullPath, plan.content, plan.mode)
			written = err == nil
			if err == nil {
				err = verifyPatchWrite(plan)
			}
		}
		if err != nil {
			rollbackEnd := committed
			if written {
				// The current file was written successfully but failed its
				// integrity check, so it must be part of the rollback too.
				rollbackEnd = i + 1
			}
			rollbackPatch(plans[:rollbackEnd])
			return fmt.Errorf("apply %q: %w", plan.path, err)
		}
		committed = i + 1
	}
	return nil
}

func verifyPatchWrite(plan patchPlan) error {
	got, err := os.ReadFile(plan.fullPath)
	if err != nil {
		return fmt.Errorf("verify %q: %w", plan.path, err)
	}
	if !bytes.Equal(got, plan.content) {
		return fmt.Errorf("verify %q: written content does not match the patch", plan.path)
	}
	return nil
}

func rollbackPatch(plans []patchPlan) {
	for i := len(plans) - 1; i >= 0; i-- {
		plan := plans[i]
		if plan.kind == patchAdd {
			_ = os.Remove(plan.fullPath)
		} else {
			_ = atomicWriteFile(plan.fullPath, plan.original, plan.mode)
		}
	}
}

func atomicWriteFile(path string, content []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".picode-patch-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}

	// Move the old file aside because rename-overwrite is inconsistent on Windows.
	var backupName string
	if _, err := os.Lstat(path); err == nil {
		backup, err := os.CreateTemp(filepath.Dir(path), ".picode-backup-*")
		if err != nil {
			return err
		}
		backupName = backup.Name()
		if err := backup.Close(); err != nil {
			return err
		}
		if err := os.Remove(backupName); err != nil {
			return err
		}
		if err := os.Rename(path, backupName); err != nil {
			return err
		}
	}
	if err := os.Rename(temporaryName, path); err != nil {
		if backupName != "" {
			_ = os.Rename(backupName, path)
		}
		return err
	}
	if backupName != "" {
		_ = os.Remove(backupName)
	}
	return nil
}
