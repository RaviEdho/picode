package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type failingChatStreamer struct{ err error }

func (s failingChatStreamer) StreamChat(context.Context, []Message) (*StreamReader, error) {
	return nil, s.err
}

func TestPersistentConversationCheckpointsIncompleteTurn(t *testing.T) {
	store, err := NewFileSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	state := &PersistedSession{
		Version:          currentSessionVersion,
		ID:               "checkpoint01",
		CreatedAt:        now,
		UpdatedAt:        now,
		WorkingDirectory: workingDirectory,
	}
	if err := store.Create(state); err != nil {
		t.Fatal(err)
	}

	streamErr := errors.New("server unavailable")
	session := NewSession(failingChatStreamer{err: streamErr}, NewToolExecutor(), PromptResolution{}, nil, SessionSnapshot{})
	conversation := NewPersistentConversation(session, store, state)
	if err := conversation.RunTurn(context.Background(), "keep this prompt", discardEventSink{}); !errors.Is(err, streamErr) {
		t.Fatalf("RunTurn error = %v, want %v", err, streamErr)
	}

	loaded, err := store.Load(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Role != "user" || loaded.Messages[0].Content != "keep this prompt" {
		t.Fatalf("persisted messages = %#v, want input checkpoint", loaded.Messages)
	}
	if history := conversation.History(); len(history) != 1 || history[0].Content != "keep this prompt" {
		t.Fatalf("History = %#v, want input checkpoint", history)
	}
	if snapshot := session.Snapshot(); len(snapshot.Messages) != 1 || snapshot.Messages[0].Content != "keep this prompt" {
		t.Fatalf("session snapshot = %#v, want input checkpoint", snapshot.Messages)
	}
}
