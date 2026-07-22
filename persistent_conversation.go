package main

import (
	"context"
	"fmt"
	"time"
)

// PersistentConversation applies automatic save-after-commit policy around a Session.
type PersistentConversation struct {
	session *Session
	store   *FileSessionStore
	state   *PersistedSession
}

// NewPersistentConversation wraps a session with save-after-commit persistence.
func NewPersistentConversation(session *Session, store *FileSessionStore, state *PersistedSession) *PersistentConversation {
	return &PersistentConversation{session: session, store: store, state: state}
}

// RunTurn checkpoints the user's input before contacting the server, then saves
// the completed turn. The checkpoint keeps an interrupted or failed turn
// resumable instead of losing its prompt when the process terminates.
func (p *PersistentConversation) RunTurn(ctx context.Context, input string, events EventSink) error {
	checkpoint := *p.state
	checkpoint.UpdatedAt = time.Now()
	checkpoint.Messages = append(cloneMessages(p.session.Snapshot().Messages), Message{Role: "user", Content: input})
	if err := p.store.Save(&checkpoint); err != nil {
		return fmt.Errorf("checkpoint session %q: %w", checkpoint.ID, err)
	}
	p.state = &checkpoint

	committed, err := p.session.RunTurn(ctx, input, events)
	if err != nil || !committed {
		p.session.RetainInput(input)
		return err
	}
	snapshot := p.session.Snapshot()
	candidate := *p.state
	candidate.UpdatedAt = time.Now()
	candidate.Messages = snapshot.Messages
	candidate.Usage = snapshot.Usage
	if err := p.store.Save(&candidate); err != nil {
		return fmt.Errorf("save session %q: %w", candidate.ID, err)
	}
	p.state = &candidate
	return nil
}

func (p *PersistentConversation) CancelActiveTool() bool { return p.session.CancelActiveTool() }
func (p *PersistentConversation) Usage() UsageTotals     { return p.session.Usage() }
func (p *PersistentConversation) SessionID() string      { return p.state.ID }

// History returns the durable state. It includes an input checkpoint when a
// turn was interrupted before the session could commit its full response.
func (p *PersistentConversation) History() []Message { return cloneMessages(p.state.Messages) }
