package main

import (
	"context"
	"errors"
	"fmt"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
)

func TestToolErrorFormatsRecoveryHint(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "validation",
			got:  toolError("invalid", "depth", "must be 1-8; default 2", nil),
			want: "invalid depth: must be 1-8; default 2",
		},
		{
			name: "bad path",
			got:  toolError("bad path", `"../etc"`, "must be relative to the working directory", nil),
			want: `bad path "../etc": must be relative to the working directory`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("got %q, want %q", test.got, test.want)
			}
		})
	}
}

func TestUnpackPathErrorKeepsLeafCause(t *testing.T) {
	cause := &fs.PathError{Op: "open", Path: "README.md", Err: errors.New("permission denied")}
	wrapped := fmt.Errorf("resolve working directory: %w", cause)
	want := `open "README.md": permission denied`
	if got := unpackPathError(wrapped); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := ioToolError(wrapped).Error(); got != "io error: "+want {
		t.Fatalf("got %q, want %q", got, "io error: "+want)
	}
}

func TestToolAbortedResultNormalizesContextCause(t *testing.T) {
	for _, test := range []struct {
		cause error
		want  string
	}{
		{cause: context.Canceled, want: "aborted: canceled by user"},
		{cause: context.DeadlineExceeded, want: "aborted: timed out"},
	} {
		result := toolAbortedResult("input", test.cause)
		if result.Status != ToolAborted || result.Output != test.want {
			t.Fatalf("got status=%s output=%q, want status=%s output=%q", result.Status, result.Output, ToolAborted, test.want)
		}
	}
}

// TestToolErrorMessagesThroughDispatch exercises the worked-example call sites
// end to end: validity-prefixed validation, bad-path refusals, io-error
// unwrapping, and abort normalization, all routed through the public Execute
// dispatcher so the assertions reflect what the model actually sees.
func TestToolErrorMessagesThroughDispatch(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	timedOutCtx, timeoutCancel := context.WithTimeout(context.Background(), 0)
	defer timeoutCancel()
	<-timedOutCtx.Done() // ensure the deadline has fired before use

	tests := []struct {
		name       string
		tool       string
		args       any
		ctx        context.Context
		wantStatus ToolStatus
		prefix     string
		check      func(t *testing.T, output string)
	}{
		{
			name:       "list_file depth too high teaches the band",
			tool:       "list_file",
			args:       map[string]any{"path": ".", "depth": 9},
			ctx:        context.Background(),
			wantStatus: ToolFailed,
			prefix:     "invalid depth: must be 1-8; default 2",
		},
		{
			name:       "list_file max_entries too high teaches the band",
			tool:       "list_file",
			args:       map[string]any{"path": ".", "max_entries": 501},
			ctx:        context.Background(),
			wantStatus: ToolFailed,
			prefix:     "invalid max_entries: must be 1-500; default 200",
		},
		{
			name:       "search max_results out of range teaches the band",
			tool:       "search",
			args:       map[string]any{"query": "x", "max_results": 999},
			ctx:        context.Background(),
			wantStatus: ToolFailed,
			prefix:     "invalid max_results: must be 1-500; default 100",
		},
		{
			name:       "search bad regex surfaces the cause",
			tool:       "search",
			args:       map[string]any{"query": "(", "regex": true},
			ctx:        context.Background(),
			wantStatus: ToolFailed,
			prefix:     "invalid regex: must be a valid regular expression",
		},
		{
			name:       "read_file missing path is required",
			tool:       "read_file",
			args:       map[string]any{"path": "   "},
			ctx:        context.Background(),
			wantStatus: ToolFailed,
			prefix:     "invalid path: is required; provide a relative file path",
		},
		{
			name:       "read_file bad JSON arguments",
			tool:       "read_file",
			args:       json.RawMessage("{not json"),
			ctx:        context.Background(),
			wantStatus: ToolFailed,
			prefix:     "invalid arguments: must be valid JSON",
		},
		{
			name:       "list_file path escape is refused as bad path",
			tool:       "list_file",
			args:       map[string]any{"path": ".."},
			ctx:        context.Background(),
			wantStatus: ToolFailed,
			prefix:     `bad path "..": must be relative to the working directory`,
		},
		{
			name:       "read_file nonexistent io error unwraps leaf cause",
			tool:       "read_file",
			args:       map[string]any{"path": "does_not_exist_zzz.txt"},
			ctx:        context.Background(),
			wantStatus: ToolFailed,
			prefix:     `io error:`,
			check: func(t *testing.T, output string) {
				if strings.Contains(output, "resolve") {
					t.Fatalf("io error still wrapped behind %q; want the leaf cause surfaced", output)
				}
				if !strings.Contains(output, "does_not_exist_zzz.txt") {
					t.Fatalf("io error dropped the path %s", output)
				}
			},
		},
		{
			name:       "list_file canceled context aborts",
			tool:       "list_file",
			args:       map[string]any{"path": "."},
			ctx:        canceledCtx,
			wantStatus: ToolAborted,
			prefix:     "aborted: canceled by user",
		},
		{
			name:       "list_file timed-out context aborts",
			tool:       "list_file",
			args:       map[string]any{"path": "."},
			ctx:        timedOutCtx,
			wantStatus: ToolAborted,
			prefix:     "aborted: timed out",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tc := ToolCall{
				Type:     "function",
				Function: ToolCallFunc{Name: test.tool, Arguments: mustJSON(t, test.args)},
			}
			result := NewToolExecutor().Execute(test.ctx, tc)
			if result.Status != test.wantStatus {
				t.Fatalf("status=%s, want %s (output=%s)", result.Status, test.wantStatus, result.Output)
			}
			if !strings.HasPrefix(result.Output, test.prefix) {
				t.Fatalf("output %q does not start with %q", result.Output, test.prefix)
			}
			if test.check != nil {
				test.check(t, result.Output)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	if raw, ok := v.(json.RawMessage); ok {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	return string(out)
}
