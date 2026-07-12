package main

import "context"

// Conversation is the frontend boundary for a resumable chat session.
type Conversation interface {
	RunTurn(context.Context, string, EventSink) error
	CancelActiveTool() bool
	Usage() UsageTotals
	SessionID() string
	History() []Message
}

// Frontend owns user input and presentation.
type Frontend interface {
	Run(context.Context, Conversation) error
	Warning(string)
}

// EventSink receives UI updates from the session and stream.
type EventSink interface {
	Emit(UIEvent)
}

// UIEvent describes a state change that a frontend can render.
type UIEvent interface {
	isUIEvent()
}

// StatusPhase identifies the current response stage.
type StatusPhase string

const (
	// StatusWaiting covers the time before the first server chunk.
	StatusWaiting StatusPhase = "waiting for response"
	// StatusThinking begins when the server starts responding.
	StatusThinking StatusPhase = "thinking"
)

// StatusEvent changes the spinner label.
type StatusEvent struct{ Phase StatusPhase }

// AssistantDeltaEvent carries one streamed text fragment.
type AssistantDeltaEvent struct{ Text string }

// ToolCallUpdateEvent carries the tool input assembled so far.
type ToolCallUpdateEvent struct {
	Index int
	Name  string
	Input string
	Path  string
}

// ToolResultEvent carries completed tool output.
type ToolResultEvent struct {
	Name   string
	Input  string
	Output string
	Status ToolStatus
}

// StreamFinishedEvent closes the active streamed response.
type StreamFinishedEvent struct{}

// EmptyResponseEvent reports a stream with no text or tool calls.
type EmptyResponseEvent struct{}

func (StatusEvent) isUIEvent()         {}
func (AssistantDeltaEvent) isUIEvent() {}
func (ToolCallUpdateEvent) isUIEvent() {}
func (ToolResultEvent) isUIEvent()     {}
func (StreamFinishedEvent) isUIEvent() {}
func (EmptyResponseEvent) isUIEvent()  {}
