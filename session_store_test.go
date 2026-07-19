package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateSessionID(t *testing.T) {
	cases := []struct {
		id     string
		wantOK bool
	}{
		{"abcdef012345", true},
		{"aaaaaaaaaaaa", true},
		{"123456789012", true},
		{"", false},
		{"short", false},
		{"abcdefghijkl", true},  // 12 lowercase letters
		{"ABCDEF012345", false}, // uppercase rejected
		{"abcdef01234-", false}, // symbol rejected
		{"abcdef0123456", false}, // too long
	}
	for _, tc := range cases {
		err := validateSessionID(tc.id)
		gotOK := err == nil
		if gotOK != tc.wantOK {
			t.Errorf("validateSessionID(%q) ok=%v want %v: %v", tc.id, gotOK, tc.wantOK, err)
		}
	}
}

func TestGenerateSessionID(t *testing.T) {
	id1, err := GenerateSessionID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := validateSessionID(id1); err != nil {
		t.Fatalf("generated ID invalid: %v", err)
	}
	id2, err := GenerateSessionID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("generated identical IDs %q", id1)
	}
}

func TestSameWorkingDirectory(t *testing.T) {
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	cleaned := filepath.Clean(dir)
	if !sameWorkingDirectory(cleaned, dir) {
		t.Errorf("sameWorkingDirectory(%q, %q) want true", cleaned, dir)
	}
	if sameWorkingDirectory(cleaned, filepath.Join(cleaned, "sub")) {
		t.Errorf("different directories reported as same")
	}
}

func TestValidatePersistedSession(t *testing.T) {
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	good := &PersistedSession{
		Version:          currentSessionVersion,
		ID:               "abcdef012345",
		CreatedAt:        now,
		UpdatedAt:        now,
		WorkingDirectory: dir,
	}
	if err := validatePersistedSession(good); err != nil {
		t.Fatalf("valid session rejected: %v", err)
	}

	invalid := []*PersistedSession{
		nil,
		newSessionVersion(good, currentSessionVersion+1),
		newSessionVersion(good, 1),
		newSessionVersion(&PersistedSession{ID: "ABC012345678", CreatedAt: now, UpdatedAt: now, WorkingDirectory: dir}, currentSessionVersion), // uppercase ID rejected
	}
	for _, s := range invalid {
		if err := validatePersistedSession(s); err == nil {
			t.Errorf("expected error for %v, got nil", s)
		}
	}
}

// newSessionVersion returns a copy of base with the given version for test mutation.
func newSessionVersion(base *PersistedSession, version int) *PersistedSession {
	if base == nil {
		return nil
	}
	clone := *base
	clone.Version = version
	return &clone
}

func TestSessionStoreRoundTrip(t *testing.T) {
	store, err := NewFileSessionStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	state := &PersistedSession{
		Version:          currentSessionVersion,
		ID:               "roundtrip001",
		CreatedAt:        now,
		UpdatedAt:        now,
		WorkingDirectory: dir,
		Model:            "test-model",
	}
	if err := store.Create(state); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Creating again must fail with ErrSessionExists (exclusive create).
	if err := store.Create(state); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("duplicate create err=%v want ErrSessionExists", err)
	}

	loaded, err := store.Load(state.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.ID != state.ID || loaded.Model != state.Model {
		t.Fatalf("loaded mismatch: %+v", loaded)
	}

	state.Model = "changed-model"
	if err := store.Save(state); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err = store.Load(state.ID)
	if err != nil {
		t.Fatalf("load after save: %v", err)
	}
	if loaded.Model != "changed-model" {
		t.Fatalf("save did not persist: %q", loaded.Model)
	}

	if err := store.Delete(state.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Load(state.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("load after delete err=%v want ErrSessionNotFound", err)
	}
}

func TestSessionStoreListAndLatest(t *testing.T) {
	store, err := NewFileSessionStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()

	empty := &PersistedSession{Version: currentSessionVersion, ID: "empty0000000", CreatedAt: base, UpdatedAt: base.Add(time.Second), WorkingDirectory: dir}
	populated := &PersistedSession{Version: currentSessionVersion, ID: "populated000", CreatedAt: base, UpdatedAt: base.Add(2 * time.Second), WorkingDirectory: dir, Messages: []Message{{Role: "user", Content: "hi"}}}

	for _, s := range []*PersistedSession{empty, populated} {
		if err := store.Create(s); err != nil {
			t.Fatalf("create %s: %v", s.ID, err)
		}
	}

	list, err := store.List(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len=%d want 2", len(list))
	}
	// Newest first: populated has the later UpdatedAt.
	if list[0].ID != populated.ID {
		t.Fatalf("list[0]=%q want %q", list[0].ID, populated.ID)
	}

	latest, err := store.LoadLatest(dir)
	if err != nil {
		t.Fatalf("load latest: %v", err)
	}
	// Populated must be preferred over empty even though both exist.
	if latest.ID != populated.ID {
		t.Fatalf("latest=%q want %q", latest.ID, populated.ID)
	}
}
