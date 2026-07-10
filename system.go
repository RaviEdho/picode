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

type PromptResolution struct {
	Text     string
	Enabled  bool
	Warnings []string
}

func resolveSystemPrompt(noSystem bool, systemFlag, systemFileFlag string) (PromptResolution, error) {
	if noSystem {
		return PromptResolution{}, nil
	}

	if value := strings.TrimSpace(systemFlag); value != "" {
		return PromptResolution{Text: value, Enabled: true}, nil
	}
	if path := strings.TrimSpace(systemFileFlag); path != "" {
		text, err := readPromptFile(path)
		if err != nil {
			return PromptResolution{}, fmt.Errorf("could not read -system-file %q: %w", path, err)
		}
		if text == "" {
			return PromptResolution{
				Text: defaultSystemPrompt, Enabled: true,
				Warnings: []string{fmt.Sprintf("-system-file %q is empty; using the built-in prompt", path)},
			}, nil
		}
		return PromptResolution{Text: text, Enabled: true}, nil
	}
	if value := strings.TrimSpace(os.Getenv("PICODE_SYSTEM")); value != "" {
		return PromptResolution{Text: value, Enabled: true}, nil
	}
	if path := strings.TrimSpace(os.Getenv("PICODE_SYSTEM_FILE")); path != "" {
		text, err := readPromptFile(path)
		if err != nil {
			return PromptResolution{
				Text: defaultSystemPrompt, Enabled: true,
				Warnings: []string{fmt.Sprintf("could not read PICODE_SYSTEM_FILE %q: %v; using the built-in prompt", path, err)},
			}, nil
		}
		if text == "" {
			return PromptResolution{
				Text: defaultSystemPrompt, Enabled: true,
				Warnings: []string{fmt.Sprintf("PICODE_SYSTEM_FILE %q is empty; using the built-in prompt", path)},
			}, nil
		}
		return PromptResolution{Text: text, Enabled: true}, nil
	}

	return PromptResolution{Text: defaultSystemPrompt, Enabled: true}, nil
}

func readPromptFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
