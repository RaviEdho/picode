package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const maxWorkspacePaths = 20

// buildWorkspaceBlock returns compact, read-only workspace metadata for the model.
func buildWorkspaceBlock() string {
	wd, err := filepath.Abs(".")
	if err != nil {
		return "# Workspace\n- Directory: (unknown)\n- Repository: unavailable"
	}
	rootBytes, err := exec.Command("git", "-C", wd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Sprintf("# Workspace\n- Directory: %q\n- Repository: none", wd)
	}
	root := strings.TrimSpace(string(rootBytes))
	statusBytes, err := exec.Command("git", "-C", wd, "status", "--short", "--untracked-files=all").Output()
	if err != nil {
		return fmt.Sprintf("# Workspace\n- Directory: %q\n- Repository: Git\n- Repository root: %q\n- Working tree: unavailable", wd, root)
	}
	status := strings.TrimSpace(string(statusBytes))
	lines := []string{}
	if status != "" {
		lines = strings.Split(status, "\n")
	}
	changed := make([]string, 0, len(lines))
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
	state := "clean"
	if status != "" {
		state = "modified"
	}
	block := fmt.Sprintf("# Workspace\n- Directory: %q\n- Repository: Git\n- Repository root: %q\n- Working tree: %s", wd, root, state)
	if len(changed) > 0 {
		block += "\n- Changed paths: " + strings.Join(changed, ", ")
		if len(lines) > maxWorkspacePaths {
			block += ", ..."
		}
	}
	return block
}
