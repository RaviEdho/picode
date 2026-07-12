package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const maxWorkspacePaths = 20

// workspaceTool returns compact, read-only workspace metadata for the model.
func workspaceTool() Tool {
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name: "workspace",
			Description: "Use only when local-file or environment work requires workspace facts that are not already available. " +
				"Describes location, platform, Git state, project type, manifests, instruction files, and a bounded top-level listing. Read-only; no arguments.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func (e *ToolExecutor) executeWorkspace(ctx context.Context, tc ToolCall) ToolResult {
	if err := ctx.Err(); err != nil {
		return ToolResult{Output: fmt.Sprintf("error: workspace aborted: %v", err), Status: ToolAborted}
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return ToolResult{Output: fmt.Sprintf("error: invalid arguments: %v", err), Status: ToolFailed}
	}
	return ToolResult{Output: buildWorkspaceBlock(), Status: ToolCompleted}
}

// buildWorkspaceBlock returns compact, read-only workspace metadata for the model.
func buildWorkspaceBlock() string {
	wd, err := filepath.Abs(".")
	if err != nil {
		return "# Workspace\n- Directory: (unknown)\n- Repository: unavailable"
	}
	var block strings.Builder
	fmt.Fprintf(&block, "# Workspace\n- Directory: %q\n- Platform: %s/%s\n", wd, friendlyOS(), runtime.GOARCH)
	rootBytes, err := exec.Command("git", "-C", wd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		block.WriteString("- Repository: none\n")
	} else {
		root := strings.TrimSpace(string(rootBytes))
		fmt.Fprintf(&block, "- Repository: Git\n- Repository root: %q\n", root)
		if rel, relErr := filepath.Rel(root, wd); relErr == nil && rel != "." {
			fmt.Fprintf(&block, "- Repository subdirectory: %q\n", filepath.ToSlash(rel))
		}
		appendGitMetadata(&block, wd)
	}
	appendProjectMetadata(&block, wd)
	return block.String()
}

func appendGitMetadata(block *strings.Builder, wd string) {
	branch := gitOutput(wd, "branch", "--show-current")
	if branch == "" {
		branch = "(detached HEAD)"
	}
	fmt.Fprintf(block, "- Branch: %s\n", branch)
	status := gitOutput(wd, "status", "--short", "--untracked-files=all")
	state := "clean"
	if status != "" {
		state = "modified"
	}
	fmt.Fprintf(block, "- Working tree: %s\n", state)
	lines := strings.Split(status, "\n")
	changed := make([]string, 0, maxWorkspacePaths)
	for _, line := range lines {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		changed = append(changed, filepath.ToSlash(path))
		if len(changed) == maxWorkspacePaths {
			break
		}
	}
	if len(changed) > 0 {
		fmt.Fprintf(block, "- Changed paths: %s", strings.Join(changed, ", "))
		if len(lines) > maxWorkspacePaths {
			block.WriteString(", ...")
		}
		block.WriteByte('\n')
	}
}

func gitOutput(wd string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", wd}, args...)...)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func appendProjectMetadata(block *strings.Builder, wd string) {
	entries, err := os.ReadDir(wd)
	if err != nil {
		return
	}
	manifests := []string{}
	instructions := []string{}
	listing := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" {
			continue
		}
		if entry.IsDir() {
			listing = append(listing, "D "+name)
		} else {
			listing = append(listing, "F "+name)
		}
		switch strings.ToLower(name) {
		case "go.mod", "go.work", "package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml",
			"pyproject.toml", "requirements.txt", "pipfile", "pipfile.lock", "poetry.lock", "setup.py",
			"cargo.toml", "cargo.lock", "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle",
			"settings.gradle.kts", "composer.json", "composer.lock", "gemfile", "gemfile.lock",
			"mix.exs", "mix.lock", "pubspec.yaml", "pubspec.lock", "package.swift", "package.resolved",
			"cmakelists.txt", "meson.build", "makefile", "justfile":
			manifests = append(manifests, name)
		}
		lowerName := strings.ToLower(name)
		if strings.HasSuffix(lowerName, ".csproj") || strings.HasSuffix(lowerName, ".fsproj") || strings.HasSuffix(lowerName, ".sln") {
			manifests = append(manifests, name)
		}
		switch strings.ToLower(name) {
		case "readme", "readme.md", "readme.rst", "readme.txt", "agents.md", "claude.md",
			"cursor.md", ".cursorrules", "copilot-instructions.md", "contributing.md", "code_of_conduct.md",
			"developer.md", "development.md", "instructions.md", "contributing.rst", "contributing.txt",
			"security.md", "support.md", "maintainers.md", "styleguide.md", "style-guide.md":
			instructions = append(instructions, name)
		}
	}
	if len(manifests) > 0 {
		fmt.Fprintf(block, "- Manifests: %s\n", strings.Join(manifests, ", "))
	}
	if len(instructions) > 0 {
		fmt.Fprintf(block, "- Instruction files: %s\n", strings.Join(instructions, ", "))
	}
	if len(listing) > 0 {
		fmt.Fprintf(block, "- Top-level entries: %s\n", strings.Join(listing, ", "))
	}
}
