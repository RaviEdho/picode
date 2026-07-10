package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ChatStreamer is the API boundary used by Session.
type ChatStreamer interface {
	StreamChat(context.Context, []Message) (*StreamReader, error)
}

// ErrSessionBusy prevents overlapping writes to conversation history.
var ErrSessionBusy = errors.New("session is already running a turn")

// UsageTotals tracks token use across the session.
type UsageTotals struct {
	Prompt     int
	Cached     int
	Completion int
}

// Total returns prompt and completion tokens combined.
func (u UsageTotals) Total() int { return u.Prompt + u.Completion }

// Session owns conversation history and tool-call round trips.
type Session struct {
	client   ChatStreamer
	executor *ToolExecutor
	logger   *RequestLogger

	systemEnabled bool
	systemMessage Message
	history       []Message

	// busy protects history from concurrent turns.
	busy    atomic.Bool
	usageMu sync.Mutex
	usage   UsageTotals
}

// NewSession creates a session with an optional system message.
func NewSession(client ChatStreamer, executor *ToolExecutor, prompt PromptResolution, logger *RequestLogger) *Session {
	s := &Session{
		client:        client,
		executor:      executor,
		logger:        logger,
		systemEnabled: prompt.Enabled,
	}
	if prompt.Enabled {
		s.systemMessage = Message{Role: "system", Content: prompt.Text}
	}
	return s
}

// RunTurn processes one user message through its final model response.
func (s *Session) RunTurn(ctx context.Context, input string, events EventSink) error {
	if !s.busy.CompareAndSwap(false, true) {
		return ErrSessionBusy
	}
	defer s.busy.Store(false)

	// Roll back to this point if the turn fails or returns nothing.
	turnStart := len(s.history)
	s.history = append(s.history, Message{Role: "user", Content: input})

	for {
		// Rebuild the request so the system message always stays first.
		messages := make([]Message, 0, len(s.history)+1)
		if s.systemEnabled {
			messages = append(messages, s.systemMessage)
		}
		messages = append(messages, s.history...)

		// One turn may require several model requests around tool calls.
		assistant, usage, finishReason, err := streamAssistant(ctx, s.client, messages, events)
		if err != nil {
			s.history = s.history[:turnStart]
			return err
		}
		if assistant == nil {
			s.history = s.history[:turnStart]
			events.Emit(EmptyResponseEvent{})
			return nil
		}

		s.history = append(s.history, *assistant)
		if usage != nil {
			s.addUsage(*usage)
		}
		if finishReason != "tool_calls" {
			return nil
		}

		// Send each tool result back to the model before continuing.
		for _, tc := range assistant.ToolCalls {
			result := s.executor.Execute(ctx, tc)
			s.logger.LogEvent(fmt.Sprintf("tool %s: cmd=%q status=%s output=(%d bytes)",
				tc.Function.Name, result.Command, result.Status, len(result.Output)))

			events.Emit(ToolResultEvent{
				Name:    tc.Function.Name,
				Command: result.Command,
				Output:  result.Output,
				Status:  result.Status,
			})

			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.history = append(s.history, Message{Role: "tool", ToolCallID: tc.ID, Content: result.Output})
		}
	}
}

// CancelActiveTool reports whether a running command was cancelled.
func (s *Session) CancelActiveTool() bool {
	return s.executor.CancelActive()
}

// Usage returns a safe snapshot of the token totals.
func (s *Session) Usage() UsageTotals {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return s.usage
}

// addUsage merges usage from one model response.
func (s *Session) addUsage(u Usage) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.usage.Prompt += u.PromptTokens
	if u.PromptTokensDetails != nil {
		s.usage.Cached += u.PromptTokensDetails.CachedTokens
	}
	s.usage.Completion += u.CompletionTokens
}
