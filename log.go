package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RequestLogger writes each outgoing chat-completion request (and any error
// responses) to a session log file. A nil *RequestLogger is a valid no-op so
// callers don't need nil checks.
type RequestLogger struct {
	mu   sync.Mutex
	file *os.File
	seq  int // monotonic request counter within the session
}

// NewRequestLogger creates a log file under ~/.picode/logs/ (creating the
// directory if needed) and returns a logger that writes to that file.
// The filename includes a timestamp so each session gets its own file.
func NewRequestLogger() (*RequestLogger, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	logDir := filepath.Join(home, ".picode", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	ts := time.Now().Format("2006-01-02_150405")
	path := filepath.Join(logDir, ts+".log")
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create log file: %w", err)
	}
	return &RequestLogger{file: f}, nil
}

// LogRequest pretty-prints the full JSON request body to the log file,
// preceded by a timestamp and sequence number.
func (l *RequestLogger) LogRequest(raw []byte) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	ts := time.Now().Format("15:04:05.000")

	// Pretty-print for readability.
	var pretty json.RawMessage
	header := fmt.Sprintf("─── request #%d  %s ───", l.seq, ts)
	if err := json.Unmarshal(raw, &pretty); err != nil {
		// Fallback: dump raw.
		l.writeFile(header, string(raw))
		return
	}
	out, _ := json.MarshalIndent(pretty, "", "  ")
	l.writeFile(header, string(out))
}

// LogResponseError writes a non-200 response status and body to the log file.
func (l *RequestLogger) LogResponseError(status int, body string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := time.Now().Format("15:04:05.000")
	header := fmt.Sprintf("─── response error #%d  %s  HTTP %d ───", l.seq, ts, status)
	l.writeFile(header, body)
}

// LogResponse records the completed model response assembled from the stream.
func (l *RequestLogger) LogResponse(response any, usage *Usage, finishReason string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := time.Now().Format("15:04:05.000")
	header := fmt.Sprintf("response #%d  %s", l.seq, ts)
	payload := struct {
		Response     any    `json:"response"`
		FinishReason string `json:"finish_reason,omitempty"`
		Usage        *Usage `json:"usage,omitempty"`
	}{response, finishReason, usage}
	raw, err := json.Marshal(payload)
	if err != nil {
		l.writeFile(header, fmt.Sprint(response))
		return
	}
	var pretty json.RawMessage
	if json.Unmarshal(raw, &pretty) == nil {
		raw, _ = json.MarshalIndent(pretty, "", "  ")
	}
	l.writeFile(header, string(raw))
}

// LogEvent writes a free-form event line (e.g. session start/end) to the log
// file.
func (l *RequestLogger) LogEvent(msg string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := time.Now().Format("15:04:05.000")
	l.writeFile(fmt.Sprintf("%s  %s", ts, msg), "")
}

func (l *RequestLogger) writeFile(header, body string) {
	fmt.Fprint(l.file, header+"\n"+body+"\n\n")
}

// Close flushes and closes the log file.
func (l *RequestLogger) Close() {
	if l == nil || l.file == nil {
		return
	}
	l.file.Sync()
	l.file.Close()
}
