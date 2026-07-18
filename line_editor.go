package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

var errInputInterrupt = errors.New("input interrupted")

// lineEditor is the interactive input boundary; platform constructors return nil for non-terminal streams.
type lineEditor interface {
	ReadLine(context.Context, string) (string, error)
}

type editableLine struct {
	text   []rune
	cursor int
}

func (l *editableLine) insert(r rune) {
	l.text = append(l.text, 0)
	copy(l.text[l.cursor+1:], l.text[l.cursor:])
	l.text[l.cursor] = r
	l.cursor++
}

func (l *editableLine) backspace() {
	if l.cursor == 0 {
		return
	}
	copy(l.text[l.cursor-1:], l.text[l.cursor:])
	l.text = l.text[:len(l.text)-1]
	l.cursor--
}

func (l *editableLine) delete() {
	if l.cursor == len(l.text) {
		return
	}
	copy(l.text[l.cursor:], l.text[l.cursor+1:])
	l.text = l.text[:len(l.text)-1]
}

func (l *editableLine) replace(start, end int, value string) {
	runes := []rune(value)
	l.text = append(append(append([]rune(nil), l.text[:start]...), runes...), l.text[end:]...)
	l.cursor = start + len(runes)
}

func (l *editableLine) set(value string) {
	l.text = []rune(value)
	l.cursor = len(l.text)
}

func (l *editableLine) String() string { return string(l.text) }

// completePath completes the path-like word immediately before the cursor.
// It intentionally does not invoke a shell, so completion remains predictable
// for commands and prose containing paths alike.
func completePath(line *editableLine) (start int, replacement string, matches []string) {
	if line.cursor != len(line.text) {
		return 0, "", nil
	}
	start = line.cursor
	for start > 0 && !unicode.IsSpace(line.text[start-1]) {
		start--
	}
	token := string(line.text[start:line.cursor])
	if !strings.HasPrefix(token, "@") {
		return start, "", nil
	}
	pathToken := strings.TrimPrefix(token, "@")
	separator := "/"
	if strings.Contains(pathToken, `\`) && !strings.Contains(pathToken, "/") {
		separator = `\`
	}
	separatorIndex := strings.LastIndexAny(pathToken, `/\`)
	directoryPart, base := "", pathToken
	if separatorIndex >= 0 {
		directoryPart, base = pathToken[:separatorIndex+1], pathToken[separatorIndex+1:]
	}
	readDirectory := strings.TrimRight(directoryPart, `/\`)
	if readDirectory == "" {
		readDirectory = "."
	}
	entries, err := os.ReadDir(filepath.FromSlash(readDirectory))
	if err != nil {
		return start, "", nil
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), base) {
			value := "@" + directoryPart + entry.Name()
			if entry.IsDir() {
				value += separator
			}
			matches = append(matches, value)
		}
	}
	sort.Strings(matches)
	if len(matches) == 1 {
		return start, matches[0], matches
	}
	if len(matches) == 0 {
		return start, "", nil
	}
	common := []rune(matches[0])
	for _, match := range matches[1:] {
		other := []rune(match)
		limit := len(common)
		if len(other) < limit {
			limit = len(other)
		}
		for i := 0; i < limit; i++ {
			if common[i] != other[i] {
				limit = i
				break
			}
		}
		common = common[:limit]
	}
	if len(common) > len([]rune(token)) {
		return start, string(common), matches
	}
	return start, "", matches
}

// inputResult normalizes scanner and terminal-editor results for PlainUI.
type inputResult struct {
	text string
	ok   bool
	err  error
}

func editorInput(ctx context.Context, editor lineEditor, prompt string) inputResult {
	text, err := editor.ReadLine(ctx, prompt)
	switch {
	case err == nil:
		return inputResult{text: text, ok: true}
	case errors.Is(err, io.EOF):
		return inputResult{}
	default:
		return inputResult{err: err}
	}
}
