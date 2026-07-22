package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testChatStreamer struct {
	stream *StreamReader
}

func (s testChatStreamer) StreamChat(context.Context, []Message) (*StreamReader, error) {
	return s.stream, nil
}

type discardEventSink struct{}

func (discardEventSink) Emit(UIEvent) {}

// TestStreamAssistantPreservesToolArgumentsWithMultipleBackticks exercises the
// SSE decode and fragmented tool-call assembly path.  In particular, the raw
// JSON arguments must not be routed through the Markdown display pipeline.
func TestStreamAssistantPreservesToolArgumentsWithMultipleBackticks(t *testing.T) {
	patch := "*** Begin Patch\n" +
		"*** Update File: scratch.txt\n" +
		"@@\n" +
		" const block = `# Start\n" +
		" plain middle line\n" +
		" closing backtick`\n" +
		"*** End Patch\n"
	arguments, err := json.Marshal(struct {
		Patch string `json:"patch"`
	}{Patch: patch})
	if err != nil {
		t.Fatal(err)
	}

	// Split inside the arguments rather than on a JSON-field boundary, as a
	// provider may do when producing streamed function-call deltas.
	split := len(arguments) / 2
	chunks := []ChatCompletionChunk{
		toolArgumentChunk("assistant", "call-test", "apply_patch", string(arguments[:split]), nil),
		toolArgumentChunk("", "", "", string(arguments[split:]), strPtr("tool_calls")),
	}

	var sse strings.Builder
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		sse.WriteString("data: ")
		sse.Write(encoded)
		sse.WriteString("\n\n")
	}
	sse.WriteString("data: [DONE]\n\n")

	stream := &StreamReader{scanner: newTestScanner(strings.NewReader(sse.String()))}
	assistant, _, finish, err := streamAssistant(context.Background(), testChatStreamer{stream: stream}, nil, discardEventSink{})
	if err != nil {
		t.Fatal(err)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", finish)
	}
	if assistant == nil || len(assistant.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one call", assistant)
	}
	if got := assistant.ToolCalls[0].Function.Arguments; got != string(arguments) {
		t.Fatalf("arguments changed during transport:\ngot:  %q\nwant: %q", got, arguments)
	}

	var decoded struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal([]byte(assistant.ToolCalls[0].Function.Arguments), &decoded); err != nil {
		t.Fatalf("assembled arguments are invalid JSON: %v", err)
	}
	if decoded.Patch != patch {
		t.Fatalf("patch changed during transport:\ngot:  %q\nwant: %q", decoded.Patch, patch)
	}

	dir := t.TempDir()
	originalDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(originalDirectory)
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("const block = `# Start\nplain middle line\nclosing backtick`\n"), 0644); err != nil {
		t.Fatal(err)
	}
	result := NewToolExecutor().executeApplyPatch(context.Background(), assistant.ToolCalls[0])
	if result.Status != ToolCompleted {
		t.Fatalf("apply assembled patch: status=%s output=%s", result.Status, result.Output)
	}
}

func TestStreamAssistantRejectsNegativeToolCallIndex(t *testing.T) {
	stream := &StreamReader{single: &ChatCompletionChunk{
		Choices: []ChunkChoice{{
			Delta: Delta{ToolCalls: []ToolCallDelta{{
				Index: -1,
			}}},
		}},
	}}

	_, _, _, err := streamAssistant(context.Background(), testChatStreamer{stream: stream}, nil, discardEventSink{})
	if err == nil || !strings.Contains(err.Error(), "invalid tool-call index -1") {
		t.Fatalf("error = %v, want invalid negative tool-call index", err)
	}
}

func toolArgumentChunk(role, id, name, arguments string, finish *string) ChatCompletionChunk {
	return ChatCompletionChunk{
		Choices: []ChunkChoice{{
			Delta: Delta{
				Role: role,
				ToolCalls: []ToolCallDelta{{
					Index:    0,
					ID:       id,
					Type:     "function",
					Function: ToolCallFuncDelta{Name: name, Arguments: arguments},
				}},
			},
			FinishReason: finish,
		}},
	}
}

func newTestScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return scanner
}
