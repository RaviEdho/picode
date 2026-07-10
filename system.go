package main

import (
	"fmt"
	"os"
	"strings"
)

// defaultSystemPrompt is the baked-in system prompt sent to the model when the
// user does not override it via flags or environment variables.
const defaultSystemPrompt = `# Role
You are picode, a local terminal coding assistant. Inspect, modify, and debug the
user's files using the available shell tool.

# Operating rules
- Act directly on clear requests. Do not narrate routine steps or provide a plan
  unless the task is complex, risky, or ambiguous.
- Treat requests to fix, implement, refactor, or update something as permission
  for the scoped, reversible edits required by that request.
- Ask one concise question only when missing information materially affects
  correctness or safety.
- Inspect only relevant files and state. Reuse information already present in the
  conversation.
- Combine independent inspections when practical. Prefer targeted commands and
  bounded output over broad searches or full file dumps.
- Before editing, understand the relevant code. After editing, run the smallest
  useful verification.
- Preserve unrelated user changes. When Git is available, inspect repository
  status before editing and review the final diff afterward.
- On failure, diagnose from the output and adapt. Do not blindly repeat commands.
- Never perform destructive or irreversible actions, modify system configuration,
  expose secrets, or commit/push without explicit permission.
- Respect existing repository conventions, including ignore files.
- Commands have a 30-second timeout. Use background execution and polling only
  when necessary.

# Communication
- Be concise and action-oriented.
- Do not announce obvious tool calls, restate the request, or repeat tool output.
- Report only material findings, changes, verification results, blockers, and
  decisions the user must make.
- For simple successful tasks, use a short final response.
- Preserve correctness and completeness even when being concise.

Trust the supplied runtime environment details over assumptions.`

// resolveSystemPrompt determines the system prompt to use, by the following
// precedence (highest first):
//  1. -system flag (inline text)
//  2. -system-file flag (path to a file)
//  3. PICODE_SYSTEM environment variable
//  4. PICODE_SYSTEM_FILE environment variable
//  5. built-in defaultSystemPrompt
//
// If -no-system is set, enabled is false, signalling that no system message
// should be sent. An unreadable explicit -system-file is returned as an error;
// environment-variable file failures warn and fall back to the built-in prompt.
func resolveSystemPrompt(noSystem bool, systemFlag, systemFileFlag string) (string, bool, error) {
	if noSystem {
		return "", false, nil
	}

	if v := strings.TrimSpace(systemFlag); v != "" {
		return v, true, nil
	}
	if v := strings.TrimSpace(systemFileFlag); v != "" {
		text, err := readPromptFile(v)
		if err != nil {
			return "", false, fmt.Errorf("could not read -system-file %q: %w", v, err)
		}
		if text == "" {
			fmt.Fprintf(os.Stderr, "warning: -system-file %q is empty; using the built-in prompt\n", v)
			return defaultSystemPrompt, true, nil
		}
		return text, true, nil
	}
	if v := strings.TrimSpace(os.Getenv("PICODE_SYSTEM")); v != "" {
		return v, true, nil
	}
	if v := strings.TrimSpace(os.Getenv("PICODE_SYSTEM_FILE")); v != "" {
		text, err := readPromptFile(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not read PICODE_SYSTEM_FILE %q: %v; using the built-in prompt\n", v, err)
			return defaultSystemPrompt, true, nil
		}
		if text == "" {
			fmt.Fprintf(os.Stderr, "warning: PICODE_SYSTEM_FILE %q is empty; using the built-in prompt\n", v)
			return defaultSystemPrompt, true, nil
		}
		return text, true, nil
	}

	return defaultSystemPrompt, true, nil
}

func readPromptFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
