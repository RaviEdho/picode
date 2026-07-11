package main

import (
	"context"
	"io"
	"strings"
)

// streamAssistant assembles a streamed response and emits UI updates.
func streamAssistant(ctx context.Context, client ChatStreamer, history []Message, events EventSink) (*Message, *Usage, string, error) {
	// Always finish the UI stream, including on errors and cancellation.
	events.Emit(StatusEvent{Phase: StatusWaiting})
	defer events.Emit(StreamFinishedEvent{})
	stream, err := client.StreamChat(ctx, history)
	if err != nil {
		return nil, nil, "", err
	}
	defer stream.Close()

	var (
		content   strings.Builder
		toolCalls []ToolCall
		role      string
		finish    string
		usage     *Usage
	)
	gotFirstChunk := false

	for {
		select {
		case <-ctx.Done():
			return nil, nil, "", ctx.Err()
		default:
		}

		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, "", err
		}
		// The first server chunk changes the visible status to thinking.
		if !gotFirstChunk {
			gotFirstChunk = true
			events.Emit(StatusEvent{Phase: StatusThinking})
		}
		// Usage often arrives in a final chunk without any choices.
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		if choice.FinishReason != nil {
			finish = *choice.FinishReason
		}
		delta := choice.Delta
		if delta.Role != "" {
			role = delta.Role
		}
		if delta.Content != "" {
			content.WriteString(delta.Content)
			events.Emit(AssistantDeltaEvent{Text: delta.Content})
		}
		// Tool calls can arrive as fragments across several chunks.
		for _, tc := range delta.ToolCalls {
			for len(toolCalls) <= tc.Index {
				toolCalls = append(toolCalls, ToolCall{})
			}
			current := &toolCalls[tc.Index]
			if tc.ID != "" {
				current.ID = tc.ID
			}
			if tc.Type != "" {
				current.Type = tc.Type
			}
			if tc.Function.Name != "" {
				current.Function.Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				current.Function.Arguments += tc.Function.Arguments
			}
			events.Emit(ToolCallUpdateEvent{
				Index: tc.Index,
				Name:  current.Function.Name,
				Input: displayToolInput(current.Function.Name, current.Function.Arguments),
			})
		}
	}

	// Treat a stream with no text or tools as an empty response.
	if content.Len() == 0 && len(toolCalls) == 0 {
		return nil, usage, finish, nil
	}
	// Some compatible servers omit these fields on tool-only replies.
	if role == "" {
		role = "assistant"
	}
	for i := range toolCalls {
		if toolCalls[i].Type == "" {
			toolCalls[i].Type = "function"
		}
	}

	message := &Message{Role: role, Content: content.String()}
	if len(toolCalls) > 0 {
		message.ToolCalls = toolCalls
	}
	if c, ok := client.(*Client); ok {
		c.Logger.LogResponse(message, usage, finish)
	}
	return message, usage, finish, nil
}

// displayToolInput decodes the primary argument while its JSON is streaming.
func displayToolInput(name, arguments string) string {
	field := "command"
	if name == "apply_patch" {
		field = "patch"
	}
	raw := extractStringValue(arguments, field)
	command := unescapeJSONString(raw)
	trailing := 0
	for i := len(raw) - 1; i >= 0 && raw[i] == '\\'; i-- {
		trailing++
	}
	if trailing%2 == 1 && len(command) > 0 {
		return command[:len(command)-1]
	}
	return command
}

// extractStringValue reads a named field from partial JSON arguments.
func extractStringValue(args, field string) string {
	key := `"` + field + `"`
	idx := strings.Index(args, key)
	if idx < 0 {
		return ""
	}
	rest := args[idx+len(key):]
	rest = strings.TrimLeft(rest, " \t\r\n")
	if !strings.HasPrefix(rest, ":") {
		return ""
	}
	rest = strings.TrimLeft(rest[1:], " \t\r\n")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	// Stop only at a quote that is not escaped.
	escaped := false
	for i, char := range rest {
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			return rest[:i]
		}
	}
	return rest
}

// unescapeJSONString decodes common escapes for live display.
func unescapeJSONString(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case '/':
			b.WriteByte('/')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		default:
			b.WriteByte(s[i])
			b.WriteByte(s[i+1])
		}
		i++
	}
	return b.String()
}
