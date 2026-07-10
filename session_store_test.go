package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func testPersistedSession(id string, updated time.Time, messages []Message) *PersistedSession {
	return &PersistedSession{
		Version:          currentSessionVersion,
		ID:               id,
		CreatedAt:        updated.Add(-time.Minute),
		UpdatedAt:        updated,
		Model:            "test-model",
		WorkingDirectory: filepath.Clean(os.TempDir()),
		System: PersistedSystem{
			Enabled:            true,
			BasePrompt:         "test prompt",
			IncludeEnvironment: true,
		},
		Messages: messages,
		Usage:    UsageTotals{Prompt: 12, Cached: 4, Completion: 3},
	}
}

func TestGenerateSessionID(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9]{12}$`)
	seen := make(map[string]bool)
	for range 100 {
		id, err := GenerateSessionID()
		if err != nil {
			t.Fatal(err)
		}
		if !valid.MatchString(id) {
			t.Fatalf("invalid generated ID %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate generated ID %q", id)
		}
		seen[id] = true
	}
}

func TestFileSessionStoreRoundTripAndSave(t *testing.T) {
	store, err := NewFileSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Nanosecond)
	state := testPersistedSession("abc123def456", now, []Message{{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID: "call-1", Type: "function",
			Function: ToolCallFunc{Name: "run_command", Arguments: `{"command":"pwd"}`},
		}},
	}, {Role: "tool", ToolCallID: "call-1", Content: "/tmp"}})

	if err := store.Create(state); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(state); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("second Create error = %v, want ErrSessionExists", err)
	}
	loaded, err := store.Load(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Model != state.Model || loaded.Usage != state.Usage || len(loaded.Messages) != 2 {
		t.Fatalf("loaded state mismatch: %#v", loaded)
	}
	if got := loaded.Messages[0].ToolCalls[0].Function.Arguments; got != `{"command":"pwd"}` {
		t.Fatalf("tool arguments = %q", got)
	}

	loaded.Messages = append(loaded.Messages, Message{Role: "assistant", Content: "done"})
	loaded.UpdatedAt = now.Add(time.Second)
	if err := store.Save(loaded); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.Load(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Messages) != 3 || reloaded.Messages[2].Content != "done" {
		t.Fatalf("saved messages = %#v", reloaded.Messages)
	}

	if info, err := os.Stat(filepath.Join(store.dir, state.ID+".json")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("session permissions = %o, want no group/other access", info.Mode().Perm())
	}
}

func TestFileSessionStoreLoadLatestPrefersPopulated(t *testing.T) {
	store, err := NewFileSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	populated := testPersistedSession("aaaaaaaaaaaa", now, []Message{{Role: "user", Content: "hello"}})
	empty := testPersistedSession("bbbbbbbbbbbb", now.Add(time.Hour), []Message{})
	for _, state := range []*PersistedSession{populated, empty} {
		if err := store.Create(state); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := store.LoadLatest(populated.WorkingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != populated.ID {
		t.Fatalf("latest ID = %q, want populated %q", latest.ID, populated.ID)
	}
}

func TestFileSessionStoreRejectsInvalidIDsAndVersions(t *testing.T) {
	store, err := NewFileSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"short", "ABC123DEF456", "../../etc/pass"} {
		if _, err := store.Load(id); err == nil {
			t.Fatalf("Load(%q) unexpectedly succeeded", id)
		}
	}
	state := testPersistedSession("abc123def456", time.Now(), nil)
	state.Version++
	if err := store.Create(state); err == nil {
		t.Fatal("Create with unsupported version unexpectedly succeeded")
	}
}

func TestSessionFailedTurnRollsBackHistory(t *testing.T) {
	initial := SessionSnapshot{
		Messages: []Message{{Role: "user", Content: "previous"}, {Role: "assistant", Content: "answer"}},
		Usage:    UsageTotals{Prompt: 2, Completion: 1},
	}
	session := NewSession(failingStreamer{}, NewToolExecutor(), PromptResolution{}, nil, initial)
	committed, err := session.RunTurn(context.Background(), "not saved", discardEvents{})
	if err == nil || committed {
		t.Fatalf("RunTurn = (%v, %v), want false and error", committed, err)
	}
	snapshot := session.Snapshot()
	if len(snapshot.Messages) != len(initial.Messages) {
		t.Fatalf("history length = %d, want %d", len(snapshot.Messages), len(initial.Messages))
	}
	if snapshot.Messages[0].Content != "previous" {
		t.Fatalf("history was mutated: %#v", snapshot.Messages)
	}
}

type failingStreamer struct{}

func (failingStreamer) StreamChat(context.Context, []Message) (*StreamReader, error) {
	return nil, errors.New("stream failed")
}

type discardEvents struct{}

func (discardEvents) Emit(UIEvent) {}

func TestPersistentConversationSavesCompletedTurn(t *testing.T) {
	store, err := NewFileSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := testPersistedSession("abc123def456", time.Now(), []Message{})
	state.Usage = UsageTotals{}
	if err := store.Create(state); err != nil {
		t.Fatal(err)
	}
	streamer := singleChunkStreamer{chunk: ChatCompletionChunk{
		Choices: []ChunkChoice{{
			Delta:        Delta{Role: "assistant", Content: "hello back"},
			FinishReason: strPtr("stop"),
		}},
		Usage: &Usage{PromptTokens: 7, CompletionTokens: 2},
	}}
	session := NewSession(streamer, NewToolExecutor(), PromptResolution{}, nil, SessionSnapshot{})
	conversation := NewPersistentConversation(session, store, state)
	if err := conversation.RunTurn(context.Background(), "hello", discardEvents{}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[0].Content != "hello" || loaded.Messages[1].Content != "hello back" {
		t.Fatalf("saved messages = %#v", loaded.Messages)
	}
	if loaded.Usage.Prompt != 7 || loaded.Usage.Completion != 2 {
		t.Fatalf("saved usage = %#v", loaded.Usage)
	}
	if !loaded.UpdatedAt.After(state.UpdatedAt) {
		t.Fatalf("updated timestamp %v did not advance past %v", loaded.UpdatedAt, state.UpdatedAt)
	}
}

type singleChunkStreamer struct {
	chunk ChatCompletionChunk
}

func (s singleChunkStreamer) StreamChat(context.Context, []Message) (*StreamReader, error) {
	chunk := s.chunk
	return &StreamReader{single: &chunk}, nil
}

func TestFileSessionStoreLockRejectsConcurrentUse(t *testing.T) {
	store, err := NewFileSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Lock("abc123def456")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.Lock("abc123def456")
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("second Lock error = %v, want ErrSessionLocked", err)
	}
}

func TestPlainUIPrintHistoryMatchesTranscriptStyle(t *testing.T) {
	var out strings.Builder
	ui := NewPlainUI(strings.NewReader(""), &out, io.Discard)
	ui.printHistory([]Message{
		{Role: "user", Content: "inspect it"},
		{Role: "assistant", Content: "I will inspect."},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call-1", Type: "function",
			Function: ToolCallFunc{Name: "run_command", Arguments: `{"command":"printf 'one\ntwo'"}`},
		}}},
		{Role: "tool", ToolCallID: "call-1", Content: "one\ntwo"},
		{Role: "assistant", Content: "Done."},
	})

	got := out.String()
	for _, want := range []string{
		colorCyan + "you>" + colorReset + " inspect it\n",
		colorGreen + "model>" + colorReset + " I will inspect.\n",
		colorYellow + "run_command>" + colorReset + " printf 'one\ntwo'\n",
		colorYellow + "   output>" + colorReset + " " + colorFaded + "one\ntwo" + colorReset + "\n",
		colorGreen + "model>" + colorReset + " Done.\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("history output missing %q\nfull output: %q", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "resum") {
		t.Fatalf("history output contains a resume marker: %q", got)
	}
}

func TestNormalizeResumeArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "bare", args: []string{"-resume"}, want: []string{"-resume"}},
		{name: "separate ID", args: []string{"-resume", "abc123def456"}, want: []string{"-resume=abc123def456"}},
		{name: "equals ID", args: []string{"-resume=abc123def456"}, want: []string{"-resume=abc123def456"}},
		{name: "next flag", args: []string{"-resume", "-model", "test"}, want: []string{"-resume", "-model", "test"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeResumeArgs(test.args)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("normalizeResumeArgs(%q) = %q, want %q", test.args, got, test.want)
			}
		})
	}
}

func TestResumeFlag(t *testing.T) {
	var resume resumeFlag
	if err := resume.Set("true"); err != nil || !resume.Enabled || resume.SessionID != "" {
		t.Fatalf("bare resume = %#v, err %v", resume, err)
	}
	if err := resume.Set("abc123def456"); err != nil || !resume.Enabled || resume.SessionID != "abc123def456" {
		t.Fatalf("specific resume = %#v, err %v", resume, err)
	}
	if err := resume.Set("false"); err != nil || resume.Enabled || resume.SessionID != "" {
		t.Fatalf("disabled resume = %#v, err %v", resume, err)
	}
}

func TestFileSessionStoreLoadLatestFiltersWorkingDirectory(t *testing.T) {
	store, err := NewFileSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	wantedDir := filepath.Join(string(filepath.Separator), "work", "wanted")
	otherDir := filepath.Join(string(filepath.Separator), "work", "other")
	wanted := testPersistedSession("aaaaaaaaaaaa", now, []Message{{Role: "user", Content: "wanted"}})
	wanted.WorkingDirectory = wantedDir
	other := testPersistedSession("bbbbbbbbbbbb", now.Add(time.Hour), []Message{{Role: "user", Content: "other"}})
	other.WorkingDirectory = otherDir
	for _, state := range []*PersistedSession{wanted, other} {
		if err := store.Create(state); err != nil {
			t.Fatal(err)
		}
	}

	latest, err := store.LoadLatest(wantedDir)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != wanted.ID {
		t.Fatalf("latest ID = %q, want same-directory session %q", latest.ID, wanted.ID)
	}
	if _, err := store.LoadLatest(filepath.Join(string(filepath.Separator), "work", "missing")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing-directory LoadLatest error = %v, want ErrSessionNotFound", err)
	}
}

func TestRequireSessionWorkingDirectoryRejectsMismatch(t *testing.T) {
	state := testPersistedSession("abc123def456", time.Now(), nil)
	state.WorkingDirectory = filepath.Join(string(filepath.Separator), "work", "original")
	if err := requireSessionWorkingDirectory(state, state.WorkingDirectory); err != nil {
		t.Fatalf("matching directory rejected: %v", err)
	}
	current := filepath.Join(string(filepath.Separator), "work", "different")
	err := requireSessionWorkingDirectory(state, current)
	if !errors.Is(err, ErrSessionWrongDir) {
		t.Fatalf("mismatched directory error = %v, want ErrSessionWrongDir", err)
	}
	for _, want := range []string{state.ID, state.WorkingDirectory, current} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("mismatch error %q does not contain %q", err, want)
		}
	}
}

func TestLegacySessionWithoutWorkingDirectoryIsRejected(t *testing.T) {
	state := testPersistedSession("abc123def456", time.Now(), nil)
	state.Version = 1
	state.WorkingDirectory = ""
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	_, err = decodeSession(data)
	if err == nil || !strings.Contains(err.Error(), "working-directory tracking") {
		t.Fatalf("legacy session error = %v, want safe-resume explanation", err)
	}
}
