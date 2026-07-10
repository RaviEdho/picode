package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

type ChatStreamer interface {
	StreamChat(context.Context, []Message) (*StreamReader, error)
}

var ErrSessionBusy = errors.New("session is already running a turn")

type UsageTotals struct {
	Prompt     int
	Cached     int
	Completion int
}

func (u UsageTotals) Total() int { return u.Prompt + u.Completion }

type Session struct {
	client   ChatStreamer
	executor *ToolExecutor
	logger   *RequestLogger

	systemEnabled bool
	systemMessage Message
	history       []Message

	busy    atomic.Bool
	usageMu sync.Mutex
	usage   UsageTotals
}

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

func (s *Session) RunTurn(ctx context.Context, input string, events EventSink) error {
	if !s.busy.CompareAndSwap(false, true) {
		return ErrSessionBusy
	}
	defer s.busy.Store(false)

	turnStart := len(s.history)
	s.history = append(s.history, Message{Role: "user", Content: input})

	for {
		messages := make([]Message, 0, len(s.history)+1)
		if s.systemEnabled {
			messages = append(messages, s.systemMessage)
		}
		messages = append(messages, s.history...)

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

		for _, tc := range assistant.ToolCalls {
			command, output := s.executor.Execute(ctx, tc)
			s.logger.LogEvent(fmt.Sprintf("tool %s: cmd=%q output=(%d bytes)", tc.Function.Name, command, len(output)))

			cancelled := strings.Contains(output, "command cancelled by user")
			events.Emit(ToolResultEvent{
				Name:      tc.Function.Name,
				Command:   command,
				Output:    output,
				Cancelled: cancelled,
			})

			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.history = append(s.history, Message{Role: "tool", ToolCallID: tc.ID, Content: output})
		}
	}
}

func (s *Session) CancelActiveTool() bool {
	return s.executor.CancelActive()
}

func (s *Session) Usage() UsageTotals {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return s.usage
}

func (s *Session) addUsage(u Usage) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.usage.Prompt += u.PromptTokens
	if u.PromptTokensDetails != nil {
		s.usage.Cached += u.PromptTokensDetails.CachedTokens
	}
	s.usage.Completion += u.CompletionTokens
}
