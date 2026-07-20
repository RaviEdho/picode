package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This test proves the repo functions handle two-backtick-context hunks.
func TestBacktickContextLines(t *testing.T) {
	cases := []struct {
		name    string
		content string
		patch   string
		want    string
	}{
		{
			name:    "two context lines each with backtick no edits",
			content: "const block = `# Start\nplain middle line\nclosing backtick`\n",
			patch:   begin() + "*** Update File: scratch.txt\n" + "@@\n" + " const block = `# Start\n" + " plain middle line\n" + " closing backtick`\n" + end(),
			want:    "const block = `# Start\nplain middle line\nclosing backtick`\n",
		},
		{
			name:    "two context backtick lines with edit between",
			content: "const block = `# Start\nold middle line\nclosing backtick`\n",
			patch:   begin() + "*** Update File: scratch.txt\n" + "@@\n" + " const block = `# Start\n" + "-old middle line\n" + "+new middle line\n" + " closing backtick`\n" + end(),
			want:    "const block = `# Start\nnew middle line\nclosing backtick`\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, err := parsePatch(tc.patch)
			if err != nil {
				t.Fatalf("parsePatch error: %v", err)
			}
			if len(ops) != 1 || len(ops[0].hunks) != 1 {
				t.Fatalf("unexpected ops: %+v", ops)
			}
			got, err := applyPatchHunks("scratch.txt", []byte(tc.content), ops[0].hunks)
			if err != nil {
				t.Fatalf("applyPatchHunks error: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestMarkdownBulletLinesCanBeRemovedTogether(t *testing.T) {
	content := "# Tools\n- Use them only when needed.\n- Inspect before editing.\n- Keep each step minimal.\n\n# Responses\n"
	patch := begin() +
		"*** Update File: scratch.txt\n" +
		"@@ # Tools\n" +
		"-- Use them only when needed.\n" +
		"-- Inspect before editing.\n" +
		" - Keep each step minimal.\n" +
		end()

	ops, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parsePatch error: %v", err)
	}
	got, err := applyPatchHunks("scratch.txt", []byte(content), ops[0].hunks)
	if err != nil {
		t.Fatalf("applyPatchHunks error: %v", err)
	}
	want := "# Tools\n- Keep each step minimal.\n\n# Responses\n"
	if string(got) != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

func TestPatchContextMismatchReportsFirstDifference(t *testing.T) {
	content := "# Section\nactual line\n"
	patch := begin() +
		"*** Update File: scratch.txt\n" +
		"@@ # Section\n" +
		" expected line\n" +
		end()

	ops, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parsePatch error: %v", err)
	}
	_, err = applyPatchHunks("scratch.txt", []byte(content), ops[0].hunks)
	if err == nil {
		t.Fatal("applyPatchHunks unexpectedly succeeded")
	}
	message := err.Error()
	for _, want := range []string{"context did not match", "actual line", "expected line"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not contain %q", message, want)
		}
	}
}

func TestPatchSectionAnchorDisambiguatesRepeatedContext(t *testing.T) {
	content := "# First\n- same bullet\n# Second\n- same bullet\n"
	patch := begin() +
		"*** Update File: scratch.txt\n" +
		"@@ # Second\n" +
		"-- same bullet\n" +
		end()

	ops, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parsePatch error: %v", err)
	}
	got, err := applyPatchHunks("scratch.txt", []byte(content), ops[0].hunks)
	if err != nil {
		t.Fatalf("applyPatchHunks error: %v", err)
	}
	want := "# First\n- same bullet\n# Second\n"
	if string(got) != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

func TestPatchSectionAnchorLimitsSearchToSection(t *testing.T) {
	content := "# First\n- same bullet\n# Second\n- same bullet\n"
	patch := begin() +
		"*** Update File: scratch.txt\n" +
		"@@ # First\n" +
		"-- same bullet\n" +
		end()

	ops, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parsePatch error: %v", err)
	}
	got, err := applyPatchHunks("scratch.txt", []byte(content), ops[0].hunks)
	if err != nil {
		t.Fatalf("applyPatchHunks error: %v", err)
	}
	want := "# First\n# Second\n- same bullet\n"
	if string(got) != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

// This test drives the FULL in-repo entry point: it builds a ToolCall whose
// Function.Arguments is the JSON-serialized patch (exactly as the harness
// delivers it), and runs executeApplyPatch against a real temp file. If this
// passes while invoking apply_patch through the harness fails, the bug is
// conclusively outside this repository.
func TestExecuteApplyPatchDoubleBacktick(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig)

	target := filepath.Join(dir, "target.txt")
	content := "const block = `# Start\nold middle line\nclosing backtick`\n"
	if err := os.WriteFile(target, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	patch := begin() +
		"*** Update File: target.txt\n" +
		"@@\n" +
		" const block = `# Start\n" +
		"-old middle line\n" +
		"+new middle line\n" +
		" closing backtick`\n" +
		end()

	args, err := json.Marshal(struct {
		Patch string `json:"patch"`
	}{Patch: patch})
	if err != nil {
		t.Fatal(err)
	}

	tc := ToolCall{
		ID:       "call-test",
		Type:     "function",
		Function: ToolCallFunc{Name: "apply_patch", Arguments: string(args)},
	}

	exec := NewToolExecutor()
	res := exec.executeApplyPatch(context.Background(), tc)
	if res.Status != ToolCompleted {
		t.Fatalf("executeApplyPatch failed: status=%s output=%s", res.Status, res.Output)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "const block = `# Start\nnew middle line\nclosing backtick`\n"
	if string(got) != want {
		t.Fatalf("file content mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestParsePatchRejectsContaminatedAddFileLines(t *testing.T) {
	for _, line := range []string{"...", "Body paragra...", "</parameter>", "endregion", "#endregion"} {
		t.Run(line, func(t *testing.T) {
			patch := begin() + "*** Add File: new.md\n+" + line + "\n" + end()
			if _, err := parsePatch(patch); err == nil {
				t.Fatalf("parsePatch accepted possible truncation marker %q", line)
			}
		})
	}
}

func TestParsePatchAllowsWhitespaceAfterEndSentinel(t *testing.T) {
	patch := begin() + "*** Add File: new.md\n+content\n*** End Patch\n \n\t\n"
	if _, err := parsePatch(patch); err != nil {
		t.Fatalf("parsePatch rejected trailing whitespace: %v", err)
	}
}

func TestExecuteApplyPatchAddFileWritesExactContent(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig)

	patch := begin() +
		"*** Add File: new.md\n" +
		"+# New Doc\n" +
		"+Body paragraph one.\n" +
		"+Further text here.\n" +
		end()
	args, err := json.Marshal(struct {
		Patch string `json:"patch"`
	}{Patch: patch})
	if err != nil {
		t.Fatal(err)
	}
	res := NewToolExecutor().executeApplyPatch(context.Background(), ToolCall{
		Function: ToolCallFunc{Name: "apply_patch", Arguments: string(args)},
	})
	if res.Status != ToolCompleted {
		t.Fatalf("executeApplyPatch failed: status=%s output=%s", res.Status, res.Output)
	}

	got, err := os.ReadFile(filepath.Join(dir, "new.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# New Doc\nBody paragraph one.\nFurther text here.\n"
	if string(got) != want {
		t.Fatalf("file content mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func begin() string { return "*** Begin Patch\n" }
func end() string   { return "*** End Patch\n" }
