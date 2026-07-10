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

func NewPersistentConversation(session *Session, store *FileSessionStore, state *PersistedSession) *PersistentConversation {
	return &PersistentConversation{session: session, store: store, state: state}
}

func (p *PersistentConversation) RunTurn(ctx context.Context, input string, events EventSink) error {
	committed, err := p.session.RunTurn(ctx, input, events)
	if err != nil || !committed {
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
func (p *PersistentConversation) History() []Message     { return p.session.Snapshot().Messages }
