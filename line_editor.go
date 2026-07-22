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
	ReadLine(context.Context, string, terminalInputRenderer) (string, error)
}

// terminalInputRenderer owns all terminal writes while an editor reads keys.
// Editors update editableLine; PlainUI draws its prompt, text, and cursor.
type terminalInputRenderer interface {
	WriteTerminal(string)
	DrawInput(prompt string, line editableLine, ghost string, columns int)
	FinishInput(suffix string)
}

type editableLine struct {
	text   []rune
	cursor int
}

// pathCompletionState keeps the matches from the last completion request so
// repeated Tab presses can cycle through them even after the line contains a
// complete match.
type pathCompletionState struct {
	start   int
	prefix  string
	matches []string
	index   int
}

func (s *pathCompletionState) reset() {
	*s = pathCompletionState{}
}

func (s *pathCompletionState) appliesTo(line *editableLine) bool {
	if len(s.matches) < 2 || s.start < 0 || s.start > line.cursor || line.cursor != len(line.text) {
		return false
	}
	token := string(line.text[s.start:line.cursor])
	if s.index < 0 {
		return token == s.prefix
	}
	return s.index < len(s.matches) && token == s.matches[s.index]
}

func bestPathMatch(matches []string) (index int, match string) {
	index = -1
	for i, candidate := range matches {
		if index < 0 || len([]rune(candidate)) > len([]rune(match)) {
			index = i
			match = candidate
		}
	}
	return index, match
}

// cyclePathCompletion applies the next path completion and returns whether the
// line changed. The ghost-text suggestion is applied first; subsequent Tab
// presses cycle through the complete matches.
func cyclePathCompletion(line *editableLine, state *pathCompletionState) bool {
	if state.appliesTo(line) {
		state.index = (state.index + 1) % len(state.matches)
		line.replace(state.start, line.cursor, state.matches[state.index])
		return true
	}

	start, _, matches := completePath(line)
	if len(matches) == 0 {
		state.reset()
		return false
	}
	// Select the best match on the initial Tab, then cycle from there. This
	// accepts the same suggestion that is shown as ghost text.
	index, match := bestPathMatch(matches)
	line.replace(start, line.cursor, match)
	*state = pathCompletionState{
		start:   start,
		prefix:  match,
		matches: matches,
		index:   index,
	}
	return true
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

// lineExtent returns the bounds of the logical input line holding the cursor.
// lineStart is the index immediately after the previous newline (or 0),
// lineEnd is the next newline (or len(text)), and lineIndex counts newlines
// before the cursor. Logical lines, not wrapped display rows, drive vertical
// cursor movement in the multi-line editor.
func (l *editableLine) lineExtent() (lineStart, lineEnd, lineIndex int) {
	lineStart = 0
	for i := 0; i < l.cursor; i++ {
		if l.text[i] == '\n' {
			lineStart = i + 1
			lineIndex++
		}
	}
	lineEnd = l.cursor
	for lineEnd < len(l.text) && l.text[lineEnd] != '\n' {
		lineEnd++
	}
	return lineStart, lineEnd, lineIndex
}

// moveUp moves the cursor to the same column on the previous logical line. It
// returns false on the first line so callers can fall back to history.
func (l *editableLine) moveUp() bool {
	lineStart, _, lineIndex := l.lineExtent()
	if lineIndex == 0 {
		return false
	}
	column := l.cursor - lineStart
	prevEnd := lineStart - 1 // the newline that ended the previous line
	prevStart := prevEnd
	for prevStart > 0 && l.text[prevStart-1] != '\n' {
		prevStart--
	}
	if column > prevEnd-prevStart {
		column = prevEnd - prevStart
	}
	l.cursor = prevStart + column
	return true
}

// moveDown moves the cursor to the same column on the next logical line. It
// returns false on the last line so callers can fall back to history.
func (l *editableLine) moveDown() bool {
	lineStart, lineEnd, _ := l.lineExtent()
	if lineEnd >= len(l.text) {
		return false
	}
	column := l.cursor - lineStart
	nextStart := lineEnd + 1
	nextEnd := nextStart
	for nextEnd < len(l.text) && l.text[nextEnd] != '\n' {
		nextEnd++
	}
	if column > nextEnd-nextStart {
		column = nextEnd - nextStart
	}
	l.cursor = nextStart + column
	return true
}

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

// pathGhostCompletion returns the uncommitted suffix of the best path match
// for the token at the end of the line. The suffix is displayed, but is not
// added to the editable input until the user accepts it with Tab.
func pathGhostCompletion(line *editableLine) string {
	if line.cursor != len(line.text) {
		return ""
	}
	start, _, matches := completePath(line)
	if line.cursor-start < 2 { // Keep "@" itself from showing a suggestion.
		return ""
	}
	if len(matches) == 0 {
		return ""
	}
	_, match := bestPathMatch(matches)
	token := []rune(string(line.text[start:line.cursor]))
	candidate := []rune(match)
	if len(candidate) <= len(token) {
		return ""
	}
	for i, r := range token {
		if candidate[i] != r {
			return ""
		}
	}
	return string(candidate[len(token):])
}

// inputResult normalizes scanner and terminal-editor results for PlainUI.
type inputResult struct {
	text string
	ok   bool
	err  error
}

func editorInput(ctx context.Context, editor lineEditor, prompt string, renderer terminalInputRenderer) inputResult {
	text, err := editor.ReadLine(ctx, prompt, renderer)
	switch {
	case err == nil:
		return inputResult{text: text, ok: true}
	case errors.Is(err, io.EOF):
		return inputResult{}
	default:
		return inputResult{err: err}
	}
}
