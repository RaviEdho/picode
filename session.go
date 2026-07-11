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
	Prompt     int      `json:"prompt"`
	Cached     int      `json:"cached"`
	Completion int      `json:"completion"`
	Cost       *float64 `json:"cost,omitempty"`
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

// SessionSnapshot is a detached copy of resumable conversation state.
type SessionSnapshot struct {
	Messages []Message
	Usage    UsageTotals
}

// NewSession creates a session with an optional system message and initial state.
func NewSession(client ChatStreamer, executor *ToolExecutor, prompt PromptResolution, logger *RequestLogger, initial SessionSnapshot) *Session {
	s := &Session{
		client:        client,
		executor:      executor,
		logger:        logger,
		systemEnabled: prompt.Enabled,
		history:       cloneMessages(initial.Messages),
		usage:         initial.Usage,
	}
	if prompt.Enabled {
		s.systemMessage = Message{Role: "system", Content: prompt.Text}
	}
	return s
}

// RunTurn processes one user message through its final model response. The
// committed result is true only when a complete turn was added to history.
func (s *Session) RunTurn(ctx context.Context, input string, events EventSink) (committed bool, err error) {
	if !s.busy.CompareAndSwap(false, true) {
		return false, ErrSessionBusy
	}
	defer s.busy.Store(false)

	// Treat a turn as a transaction so failed and cancelled tool round trips
	// can never leave malformed resumable history behind.
	turnStart := len(s.history)
	usageStart := s.Usage()
	defer func() {
		if !committed {
			s.history = s.history[:turnStart]
			s.setUsage(usageStart)
		}
	}()
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
			return false, err
		}
		if assistant == nil {
			events.Emit(EmptyResponseEvent{})
			return false, nil
		}

		s.history = append(s.history, *assistant)
		if usage != nil {
			s.addUsage(*usage)
		}
		if finishReason != "tool_calls" {
			return true, nil
		}

		// Send each tool result back to the model before continuing.
		for _, tc := range assistant.ToolCalls {
			result := s.executor.Execute(ctx, tc)
			s.logger.LogEvent(fmt.Sprintf("tool %s: cmd=%q status=%s output=(%d bytes)",
				tc.Function.Name, result.Input, result.Status, len(result.Output)))

			events.Emit(ToolResultEvent{
				Name:   tc.Function.Name,
				Input:  result.Input,
				Output: result.Output,
				Status: result.Status,
			})

			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			s.history = append(s.history, Message{Role: "tool", ToolCallID: tc.ID, Content: result.Output})
		}
	}
}

// Snapshot returns a deep copy safe for persistence.
func (s *Session) Snapshot() SessionSnapshot {
	return SessionSnapshot{Messages: cloneMessages(s.history), Usage: s.Usage()}
}

func cloneMessages(messages []Message) []Message {
	cloned := make([]Message, len(messages))
	copy(cloned, messages)
	for i := range cloned {
		if messages[i].ToolCalls != nil {
			cloned[i].ToolCalls = append([]ToolCall(nil), messages[i].ToolCalls...)
		}
	}
	return cloned
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

func (s *Session) setUsage(usage UsageTotals) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.usage = usage
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
	if u.Cost != nil {
		if s.usage.Cost == nil {
			v := 0.0
			s.usage.Cost = &v
		}
		*s.usage.Cost += *u.Cost
	}
}
