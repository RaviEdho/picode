package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPlainUIRendererOwnsInputDrawing(t *testing.T) {
	var out bytes.Buffer
	ui := NewPlainUI(strings.NewReader(""), &out, &bytes.Buffer{})
	ui.startRenderer()
	defer ui.stopRenderer()

	ui.DrawInput("you> ", editableLine{text: []rune("hello"), cursor: 2}, "", 80)
	ui.FinishInput("")
	ui.flushRenderer()

	if got := out.String(); !strings.Contains(got, "you> hello") {
		t.Fatalf("rendered input = %q, want prompt and input", got)
	}
	if !strings.Contains(out.String(), "\033[7C") {
		t.Fatalf("rendered input = %q, want cursor placement", out.String())
	}
}

func TestPlainUIFlushRendererOrdersHistoryBeforeInput(t *testing.T) {
	var out bytes.Buffer
	ui := NewPlainUI(strings.NewReader(""), &out, &bytes.Buffer{})
	ui.startRenderer()
	defer ui.stopRenderer()

	ui.printHistory([]Message{{Role: "assistant", Content: "restored"}})
	ui.flushRenderer()
	ui.DrawInput("you> ", editableLine{}, "", 80)

	rendered := out.String()
	history := strings.Index(rendered, "restored")
	prompt := strings.LastIndex(rendered, "you> ")
	if history < 0 || prompt < 0 || history > prompt {
		t.Fatalf("rendered output = %q, want history before input", rendered)
	}
}
