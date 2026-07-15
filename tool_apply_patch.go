package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	lines []patchLine
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
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name: "apply_patch",
			Description: "Apply a structured patch to files in the current working directory. " +
				"Use *** Begin Patch and *** End Patch with one or more *** Add File, " +
				"*** Update File, or *** Delete File sections. Update sections contain @@ hunks " +
				"whose lines begin with a space (context), + (add), or - (remove). " +
				"All paths must be relative. The patch is validated before files are changed.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"patch": map[string]any{
						"type":        "string",
						"description": "The complete structured patch to apply.",
					},
				},
				"required": []string{"patch"},
			},
		},
	}
}

// executeApplyPatch decodes, validates, and applies an apply_patch tool call.
func (e *ToolExecutor) executeApplyPatch(ctx context.Context, tc ToolCall) ToolResult {
	var args struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return failedPatchResult("", fmt.Errorf("invalid arguments: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{Input: args.Patch, Output: fmt.Sprintf("error: patch aborted: %v", err), Status: ToolAborted}
	}

	operations, err := parsePatch(args.Patch)
	if err != nil {
		return failedPatchResult(args.Patch, err)
	}
	root, err := filepath.Abs(".")
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return failedPatchResult(args.Patch, fmt.Errorf("resolve working directory: %w", err))
	}
	plans, err := preparePatch(root, operations)
	if err != nil {
		return failedPatchResult(args.Patch, err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{Input: args.Patch, Output: fmt.Sprintf("error: patch aborted: %v", err), Status: ToolAborted}
	}
	if err := commitPatch(plans); err != nil {
		return failedPatchResult(args.Patch, err)
	}

	var output strings.Builder
	additions, deletions := 0, 0
	output.WriteString("Patch applied successfully.")
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
	fmt.Fprintf(&output, "\nDiff summary: %d additions, %d deletions across %d files.", additions, deletions, len(plans))
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
	return ToolResult{Input: patch, Output: fmt.Sprintf("error: %v", err), Status: ToolFailed}
}

func parsePatch(text string) ([]patchOperation, error) {
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("patch is not valid UTF-8")
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
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
				op.lines = append(op.lines, lines[i][1:])
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
				hunk := patchHunk{}
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
				return nil, fmt.Errorf("open %q: %w", op.path, statErr)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%q is not a regular file", op.path)
			}
			plan.mode = info.Mode().Perm()
			plan.original, err = os.ReadFile(fullPath)
			if err != nil {
				return nil, fmt.Errorf("read %q: %w", op.path, err)
			}
			if !utf8.Valid(plan.original) {
				return nil, fmt.Errorf("%q is not valid UTF-8", op.path)
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
		return "", fmt.Errorf("unsafe path %q: paths must be relative", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path %q: path escapes the working directory", name)
	}
	full := filepath.Join(root, clean)
	check := full
	if adding {
		check = filepath.Dir(full)
	}
	resolved, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", name, err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path %q: symlink escapes the working directory", name)
	}
	if !adding {
		if info, err := os.Lstat(full); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("unsafe path %q: symbolic links cannot be patched", name)
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
		match := -1
		for i := searchFrom; i+len(oldLines) <= len(lines); i++ {
			if equalLines(lines[i:i+len(oldLines)], oldLines) {
				if match >= 0 {
					return nil, fmt.Errorf("update %q hunk %d: context is ambiguous", path, hunkIndex+1)
				}
				match = i
			}
		}
		if match < 0 {
			return nil, fmt.Errorf("update %q hunk %d: context did not match", path, hunkIndex+1)
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
		if plan.kind == patchDelete {
			err = os.Remove(plan.fullPath)
		} else {
			err = atomicWriteFile(plan.fullPath, plan.content, plan.mode)
		}
		if err != nil {
			rollbackPatch(plans[:committed])
			return fmt.Errorf("apply %q: %w", plan.path, err)
		}
		committed = i + 1
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
