package main

import (
	"context"
	"errors"
	"io"
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

func (l *editableLine) set(value string) {
	l.text = []rune(value)
	l.cursor = len(l.text)
}

func (l *editableLine) String() string { return string(l.text) }

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
