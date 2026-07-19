package main

import "testing"

func TestOneOf(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"", true},
		{"low", true},
		{"LOW", false}, // case-sensitive
		{"unknown", false},
	}
	for _, tc := range cases {
		if got := oneOf(tc.value, "", "low", "high"); got != tc.want {
			t.Errorf("oneOf(%q)=%v want %v", tc.value, got, tc.want)
		}
	}
}

func TestResolveLLMParametersValid(t *testing.T) {
	base := LLMParameters{
		Temperature:         0.5,
		TopP:                0.9,
		MaxCompletionTokens: 1024,
		PresencePenalty:     0,
		FrequencyPenalty:    0,
		ServiceTier:         " AUTO ",
		ReasoningEffort:     "High",
		Verbosity:           "Medium",
	}
	got, err := resolveLLMParameters(base, 0, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// String fields are normalized (trimmed + lowercased).
	if got.ServiceTier != "auto" {
		t.Errorf("ServiceTier=%q want auto", got.ServiceTier)
	}
	if got.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort=%q want high", got.ReasoningEffort)
	}
	if got.Verbosity != "medium" {
		t.Errorf("Verbosity=%q want medium", got.Verbosity)
	}
	if got.Seed != nil {
		t.Errorf("Seed=%v want nil when seedSet is false", got.Seed)
	}
}

func TestResolveLLMParametersSeedSet(t *testing.T) {
	got, err := resolveLLMParameters(defaultLLMParameters(), 42, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Seed == nil || *got.Seed != 42 {
		t.Fatalf("Seed=%v want 42", got.Seed)
	}
}

func TestResolveLLMParametersErrors(t *testing.T) {
	cases := []struct {
		name string
		p    LLMParameters
	}{
		{"temperature too high", LLMParameters{Temperature: 3, TopP: 1, MaxCompletionTokens: 1}},
		{"temperature negative", LLMParameters{Temperature: -0.1, TopP: 1, MaxCompletionTokens: 1}},
		{"top-p zero", LLMParameters{Temperature: 1, TopP: 0, MaxCompletionTokens: 1}},
		{"top-p over one", LLMParameters{Temperature: 1, TopP: 1.5, MaxCompletionTokens: 1}},
		{"zero completion tokens", LLMParameters{Temperature: 1, TopP: 1, MaxCompletionTokens: 0}},
		{"presence penalty out of range", LLMParameters{Temperature: 1, TopP: 1, MaxCompletionTokens: 1, PresencePenalty: 3}},
		{"frequency penalty out of range", LLMParameters{Temperature: 1, TopP: 1, MaxCompletionTokens: 1, FrequencyPenalty: -3}},
		{"invalid service tier", LLMParameters{Temperature: 1, TopP: 1, MaxCompletionTokens: 1, ServiceTier: "bogus"}},
		{"invalid reasoning effort", LLMParameters{Temperature: 1, TopP: 1, MaxCompletionTokens: 1, ReasoningEffort: "bogus"}},
		{"invalid verbosity", LLMParameters{Temperature: 1, TopP: 1, MaxCompletionTokens: 1, Verbosity: "bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveLLMParameters(tc.p, 0, false); err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}
