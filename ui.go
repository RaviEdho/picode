package main

import "context"

type Frontend interface {
	Run(context.Context, *Session) error
	Warning(string)
}

type EventSink interface {
	Emit(UIEvent)
}

type UIEvent interface {
	isUIEvent()
}

type StatusPhase string

const (
	StatusWaiting  StatusPhase = "waiting for response"
	StatusThinking StatusPhase = "thinking"
)

type StatusEvent struct{ Phase StatusPhase }

type AssistantDeltaEvent struct{ Text string }

type ToolCallUpdateEvent struct {
	Index   int
	Name    string
	Command string
}

type ToolResultEvent struct {
	Name      string
	Command   string
	Output    string
	Cancelled bool
}

type StreamFinishedEvent struct{}

type EmptyResponseEvent struct{}

func (StatusEvent) isUIEvent()         {}
func (AssistantDeltaEvent) isUIEvent() {}
func (ToolCallUpdateEvent) isUIEvent() {}
func (ToolResultEvent) isUIEvent()     {}
func (StreamFinishedEvent) isUIEvent() {}
func (EmptyResponseEvent) isUIEvent()  {}
