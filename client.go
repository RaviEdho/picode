package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client sends OpenAI-compatible chat completion requests.
type Client struct {
	BaseURL    string
	APIKey     string
	Model      string
	Parameters LLMParameters
	Tools      []Tool
	HTTPClient *http.Client
	Logger     *RequestLogger // nil when logging is disabled
}

// NewClient creates a client with default model parameters and a five-minute HTTP timeout.
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		Model:      model,
		Parameters: defaultLLMParameters(),
		HTTPClient: &http.Client{
			Timeout: 300 * time.Second,
		},
	}
}

// StreamReader presents SSE streams and single JSON responses through the same interface.
type StreamReader struct {
	scanner    *bufio.Scanner
	resp       *http.Response
	single     *ChatCompletionChunk
	doneSingle bool
}

// Recv returns the next chunk, or io.EOF when the stream ends.
func (s *StreamReader) Recv() (*ChatCompletionChunk, error) {
	if s.single != nil {
		if s.doneSingle {
			return nil, io.EOF
		}
		s.doneSingle = true
		return s.single, nil
	}
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return nil, io.EOF
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("decode chunk: %w", err)
		}
		return &chunk, nil
	}
	if err := s.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// Close releases the underlying HTTP response body, if any.
func (s *StreamReader) Close() error {
	if s.resp != nil {
		return s.resp.Body.Close()
	}
	return nil
}

// StreamChat requests a streamed completion, wrapping non-SSE JSON responses as a single chunk.
func (c *Client) StreamChat(ctx context.Context, messages []Message) (*StreamReader, error) {

	req := ChatCompletionRequest{
		Model:               c.Model,
		Messages:            messages,
		Tools:               c.Tools,
		Stream:              true,
		Temperature:         c.Parameters.Temperature,
		TopP:                c.Parameters.TopP,
		MaxCompletionTokens: c.Parameters.MaxCompletionTokens,
		PresencePenalty:     c.Parameters.PresencePenalty,
		FrequencyPenalty:    c.Parameters.FrequencyPenalty,
		Seed:                c.Parameters.Seed,
		ServiceTier:         c.Parameters.ServiceTier,
		ReasoningEffort:     c.Parameters.ReasoningEffort,
		Verbosity:           c.Parameters.Verbosity,
		StreamOptions: &StreamOptions{
			IncludeUsage: true,
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Log the complete payload before transport errors can occur.
	c.Logger.LogRequest(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		resp.Body.Close()
		errBody := buf.String()
		c.Logger.LogResponseError(resp.StatusCode, errBody)
		return nil, fmt.Errorf("api error %d: %s", resp.StatusCode, errBody)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		// Fallback: server returned a normal JSON completion.
		var result ChatCompletionResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode response: %w", err)
		}
		resp.Body.Close()
		single := &ChatCompletionChunk{
			Usage: &result.Usage,
		}
		if len(result.Choices) > 0 {
			ch := result.Choices[0]
			single.Choices = []ChunkChoice{{
				Index:        ch.Index,
				Delta:        Delta{Role: ch.Message.Role, Content: ch.Message.Content, ToolCalls: toolCallsToDelta(ch.Message.ToolCalls)},
				FinishReason: strPtr(ch.FinishReason),
			}}
		}
		return &StreamReader{single: single}, nil
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &StreamReader{scanner: scanner, resp: resp}, nil
}
func toolCallsToDelta(tcs []ToolCall) []ToolCallDelta {
	out := make([]ToolCallDelta, 0, len(tcs))
	for i, tc := range tcs {
		out = append(out, ToolCallDelta{
			Index:    i,
			ID:       tc.ID,
			Type:     tc.Type,
			Function: ToolCallFuncDelta{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
		})
	}
	return out
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
