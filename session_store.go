package main

import (
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	currentSessionVersion = 2
	sessionIDLength       = 12
	sessionIDAlphabet     = "abcdefghijklmnopqrstuvwxyz0123456789"
)

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionExists   = errors.New("session already exists")
	ErrSessionLocked   = errors.New("session is already open")
	ErrSessionWrongDir = errors.New("session belongs to a different working directory")
)

// PersistedSystem stores only stable prompt configuration. Runtime environment
// details are rebuilt whenever a session is resumed.
type PersistedSystem struct {
	Enabled            bool   `json:"enabled"`
	BasePrompt         string `json:"base_prompt,omitempty"`
	IncludeEnvironment bool   `json:"include_environment"`
}

// PersistedSession is the versioned on-disk representation of a conversation.
type PersistedSession struct {
	Version          int             `json:"version"`
	ID               string          `json:"id"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	Model            string          `json:"model,omitempty"`
	WorkingDirectory string          `json:"working_directory"`
	System           PersistedSystem `json:"system"`
	Messages         []Message       `json:"messages"`
	Usage            UsageTotals     `json:"usage"`
}

// SessionLock is held for the lifetime of a process using a session.
type SessionLock interface {
	Close() error
}

// FileSessionStore persists sessions as private JSON files in one directory.
type FileSessionStore struct {
	dir string
}

func NewFileSessionStore(dir string) (*FileSessionStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure session directory: %w", err)
	}
	return &FileSessionStore{dir: dir}, nil
}

func NewDefaultFileSessionStore() (*FileSessionStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	root := filepath.Join(home, ".picode")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create picode data directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure picode data directory: %w", err)
	}
	return NewFileSessionStore(filepath.Join(root, "sessions"))
}

// currentWorkingDirectory returns a stable absolute path for session scoping.
// Resolving symlinks makes equivalent paths compare consistently.
func currentWorkingDirectory() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	absolute, err := filepath.Abs(wd)
	if err != nil {
		return "", fmt.Errorf("resolve absolute working directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve working directory symlinks: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func sameWorkingDirectory(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func requireSessionWorkingDirectory(state *PersistedSession, workingDirectory string) error {
	if sameWorkingDirectory(state.WorkingDirectory, workingDirectory) {
		return nil
	}
	return fmt.Errorf("%w: session %q belongs to %q; current directory is %q",
		ErrSessionWrongDir, state.ID, state.WorkingDirectory, workingDirectory)
}

// GenerateSessionID returns 12 cryptographically random lowercase letters and digits.
func GenerateSessionID() (string, error) {
	const unbiasedLimit = 256 - (256 % len(sessionIDAlphabet))
	id := make([]byte, sessionIDLength)
	buf := make([]byte, sessionIDLength)
	for i := 0; i < len(id); {
		if _, err := cryptorand.Read(buf); err != nil {
			return "", fmt.Errorf("generate session ID: %w", err)
		}
		for _, value := range buf {
			if int(value) >= unbiasedLimit {
				continue
			}
			id[i] = sessionIDAlphabet[int(value)%len(sessionIDAlphabet)]
			i++
			if i == len(id) {
				break
			}
		}
	}
	return string(id), nil
}

func validateSessionID(id string) error {
	if len(id) != sessionIDLength {
		return fmt.Errorf("invalid session ID %q: expected 12 lowercase letters or digits", id)
	}
	for _, char := range id {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9')) {
			return fmt.Errorf("invalid session ID %q: expected 12 lowercase letters or digits", id)
		}
	}
	return nil
}

func (s *FileSessionStore) sessionPath(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Lock prevents concurrent processes from writing the same session.
func (s *FileSessionStore) Lock(id string) (SessionLock, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	lock, err := acquireSessionLock(filepath.Join(s.dir, "."+id+".lock"))
	if err != nil {
		return nil, err
	}
	return lock, nil
}

// Create writes a new session without replacing an existing one.
func (s *FileSessionStore) Create(state *PersistedSession) error {
	if err := validatePersistedSession(state); err != nil {
		return err
	}
	data, err := marshalSession(state)
	if err != nil {
		return err
	}
	path := s.sessionPath(state.ID)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: %s", ErrSessionExists, state.ID)
	}
	if err != nil {
		return fmt.Errorf("create session %q: %w", state.ID, err)
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write session %q: %w", state.ID, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync session %q: %w", state.ID, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close session %q: %w", state.ID, err)
	}
	ok = true
	return nil
}

// Save atomically replaces an existing session with a complete snapshot.
func (s *FileSessionStore) Save(state *PersistedSession) error {
	if err := validatePersistedSession(state); err != nil {
		return err
	}
	path := s.sessionPath(state.ID)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, state.ID)
	} else if err != nil {
		return fmt.Errorf("stat session %q: %w", state.ID, err)
	}
	data, err := marshalSession(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "."+state.ID+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary session file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure temporary session file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write session %q: %w", state.ID, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync session %q: %w", state.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close session %q: %w", state.ID, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace session %q: %w", state.ID, err)
	}
	return nil
}

// Delete removes a saved session. Callers must hold its session lock.
func (s *FileSessionStore) Delete(id string) error {
	if err := validateSessionID(id); err != nil {
		return err
	}
	if err := os.Remove(s.sessionPath(id)); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	} else if err != nil {
		return fmt.Errorf("delete session %q: %w", id, err)
	}
	return nil
}

func (s *FileSessionStore) Load(id string) (*PersistedSession, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.sessionPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("read session %q: %w", id, err)
	}
	state, err := decodeSession(data)
	if err != nil {
		return nil, fmt.Errorf("load session %q: %w", id, err)
	}
	if state.ID != id {
		return nil, fmt.Errorf("load session %q: file contains session ID %q", id, state.ID)
	}
	return state, nil
}

// List returns every valid session for a working directory, newest first.
// Unreadable or invalid files are ignored so one damaged session does not make
// the rest of the session history inaccessible.
func (s *FileSessionStore) List(workingDirectory string) ([]*PersistedSession, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read session directory: %w", err)
	}
	var sessions []*PersistedSession
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if validateSessionID(id) != nil {
			continue
		}
		state, err := s.Load(id)
		if err != nil || !sameWorkingDirectory(state.WorkingDirectory, workingDirectory) {
			continue
		}
		sessions = append(sessions, state)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].UpdatedAt.Equal(sessions[j].UpdatedAt) {
			return sessions[i].ID > sessions[j].ID
		}
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	return sessions, nil
}

// LoadLatest returns the most recently updated valid session from the given
// working directory. Populated sessions are preferred so an accidental empty
// launch does not hide useful history.
func (s *FileSessionStore) LoadLatest(workingDirectory string) (*PersistedSession, error) {
	sessions, err := s.List(workingDirectory)
	if err != nil {
		return nil, err
	}
	var populated, empty []*PersistedSession
	for _, state := range sessions {
		if len(state.Messages) == 0 {
			empty = append(empty, state)
		} else {
			populated = append(populated, state)
		}
	}
	candidates := populated
	if len(candidates) == 0 {
		candidates = empty
	}
	if len(candidates) == 0 {
		return nil, ErrSessionNotFound
	}
	return candidates[0], nil
}

func marshalSession(state *PersistedSession) ([]byte, error) {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode session %q: %w", state.ID, err)
	}
	return append(data, '\n'), nil
}

func decodeSession(data []byte) (*PersistedSession, error) {
	var state PersistedSession
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if err := validatePersistedSession(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

func validatePersistedSession(state *PersistedSession) error {
	if state == nil {
		return errors.New("session is nil")
	}
	if state.Version == 1 {
		return errors.New("session predates working-directory tracking and cannot be resumed safely")
	}
	if state.Version != currentSessionVersion {
		return fmt.Errorf("unsupported session version %d (expected %d)", state.Version, currentSessionVersion)
	}
	if err := validateSessionID(state.ID); err != nil {
		return err
	}
	if state.CreatedAt.IsZero() || state.UpdatedAt.IsZero() {
		return errors.New("session timestamps are missing")
	}
	if state.WorkingDirectory == "" || !filepath.IsAbs(state.WorkingDirectory) {
		return errors.New("session working directory is missing or not absolute")
	}
	return nil
}
