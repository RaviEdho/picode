package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBytesLookBinary(t *testing.T) {
	cases := map[string]bool{
		"plain text":      false,
		"":                false,
		"text\x00binary":  true,
		"\x00":            true,
	}
	for data, want := range cases {
		if got := bytesLookBinary([]byte(data)); got != want {
			t.Errorf("bytesLookBinary(%q)=%v want %v", data, got, want)
		}
	}
}

func TestIsSkippedSearchDirectory(t *testing.T) {
	skipped := []string{".git", "node_modules", "vendor", "dist", "build", "target", "coverage"}
	for _, name := range skipped {
		if !isSkippedSearchDirectory(name) {
			t.Errorf("want %q skipped", name)
		}
	}
	if isSkippedSearchDirectory("src") {
		t.Errorf("src should not be skipped")
	}
	// Case-insensitive match.
	if !isSkippedSearchDirectory("BUILD") {
		t.Errorf("BUILD should be skipped (case-insensitive)")
	}
}

func TestSearchEntryKind(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]byte{}
	for _, e := range entries {
		kinds[e.Name()] = searchEntryKind(e)
	}
	if kinds["subdir"] != 'D' {
		t.Errorf("subdir kind=%q want 'D'", string(kinds["subdir"]))
	}
	if kinds["file.txt"] != 'F' {
		t.Errorf("file.txt kind=%q want 'F'", string(kinds["file.txt"]))
	}
}

func TestSafeSearchPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "inside.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Absolute paths are rejected.
	if _, err := safeSearchPath(root, target); err == nil {
		t.Fatal("absolute path should be rejected")
	}
	// Parent-directory traversal is rejected.
	if _, err := safeSearchPath(root, filepath.FromSlash("../escape")); err == nil {
		t.Fatal("traversal path should be rejected")
	}
	// Valid relative path resolves under root.
	got, err := safeSearchPath(root, "inside.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, _ := filepath.EvalSymlinks(target)
	if got != want {
		t.Errorf("resolved=%q want %q", got, want)
	}
}

func TestAppendSearchResults(t *testing.T) {
	lines := []string{"alpha", "beta", "gamma", "delta"}
	var out strings.Builder
	if err := appendSearchResults(&out, "f.txt", lines, []int{2}, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "f.txt:") {
		t.Errorf("missing header in %q", got)
	}
	// Line index 2 -> "3: gamma" (1-based, matched separator ':').
	if !strings.Contains(got, "3: gamma") {
		t.Errorf("missing matched line, got=%q", got)
	}
}

func TestAppendSearchResultsWithContext(t *testing.T) {
	lines := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	var out strings.Builder
	// Match index 2 with 1 line of context each side.
	if err := appendSearchResults(&out, "f.txt", lines, []int{2}, 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	// Context lines use '-' separator, the match uses ':'.
	if !strings.Contains(got, "2- beta") || !strings.Contains(got, "3: gamma") || !strings.Contains(got, "4- delta") {
		t.Errorf("context output wrong, got=%q", got)
	}
}
