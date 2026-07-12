package main

// OpenAI-compatible request/response types

type ChatCompletionRequest struct {
	Model               string         `json:"model"`
	Messages            []Message      `json:"messages"`
	Tools               []Tool         `json:"tools,omitempty"`
	Stream              bool           `json:"stream"`
	StreamOptions       *StreamOptions `json:"stream_options,omitempty"`
	Temperature         float64        `json:"temperature"`
	TopP                float64        `json:"top_p"`
	MaxCompletionTokens int            `json:"max_completion_tokens"`
	PresencePenalty     float64        `json:"presence_penalty"`
	FrequencyPenalty    float64        `json:"frequency_penalty"`
	Seed                *int64         `json:"seed,omitempty"`
	ServiceTier         string         `json:"service_tier,omitempty"`
	ReasoningEffort     string         `json:"reasoning_effort,omitempty"`
	Verbosity           string         `json:"verbosity,omitempty"`
	ParallelToolCalls   bool           `json:"parallel_tool_calls"`
}

// LLMParameters contains request-level controls shared by every model call.
// The defaults favor concise, inexpensive coding responses without removing
// access to tools or limiting a normal multi-step response.
type LLMParameters struct {
	Temperature         float64
	TopP                float64
	MaxCompletionTokens int
	PresencePenalty     float64
	FrequencyPenalty    float64
	Seed                *int64
	ServiceTier         string
	ReasoningEffort     string
	Verbosity           string
	ParallelToolCalls   bool
}

func defaultLLMParameters() LLMParameters {
	return LLMParameters{
		Temperature:         0.2,
		TopP:                1,
		MaxCompletionTokens: 4096,
		ServiceTier:         "auto",
		ReasoningEffort:     "low",
		Verbosity:           "low",
		ParallelToolCalls:   false,
	}
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolCallFunc `json:"function"`
}

type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatCompletionResponse struct {
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type Usage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	ReasoningTokens     int                  `json:"reasoning_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	TotalTokens         int                  `json:"total_tokens"`
	Cost                *float64             `json:"cost,omitempty"`
}

// ---- Streaming (SSE) types ----

type ChatCompletionChunk struct {
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []ToolCallDelta `json:"tool_calls,omitempty"`
}

type ToolCallDelta struct {
	Index    int               `json:"index"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function ToolCallFuncDelta `json:"function"`
}

type ToolCallFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
