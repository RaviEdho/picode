package main

import (
	"fmt"
	"strings"
)

// resolveLLMParameters validates CLI values and preserves an absent seed so it
// is not sent as an accidental zero to servers that support deterministic runs.
func resolveLLMParameters(parameters LLMParameters, seed int64, seedSet bool) (LLMParameters, error) {
	if parameters.Temperature < 0 || parameters.Temperature > 2 {
		return LLMParameters{}, fmt.Errorf("-temperature must be between 0 and 2")
	}
	if parameters.TopP <= 0 || parameters.TopP > 1 {
		return LLMParameters{}, fmt.Errorf("-top-p must be greater than 0 and at most 1")
	}
	if parameters.MaxCompletionTokens < 1 {
		return LLMParameters{}, fmt.Errorf("-max-completion-tokens must be at least 1")
	}
	if parameters.PresencePenalty < -2 || parameters.PresencePenalty > 2 {
		return LLMParameters{}, fmt.Errorf("-presence-penalty must be between -2 and 2")
	}
	if parameters.FrequencyPenalty < -2 || parameters.FrequencyPenalty > 2 {
		return LLMParameters{}, fmt.Errorf("-frequency-penalty must be between -2 and 2")
	}
	parameters.ServiceTier = strings.TrimSpace(parameters.ServiceTier)
	parameters.ServiceTier = strings.ToLower(parameters.ServiceTier)
	if !oneOf(parameters.ServiceTier, "", "auto", "default", "flex", "priority") {
		return LLMParameters{}, fmt.Errorf("-service-tier must be auto, default, flex, or priority")
	}
	parameters.ReasoningEffort = strings.ToLower(strings.TrimSpace(parameters.ReasoningEffort))
	if !oneOf(parameters.ReasoningEffort, "", "low", "medium", "high") {
		return LLMParameters{}, fmt.Errorf("-reasoning-effort must be low, medium, or high")
	}
	parameters.Verbosity = strings.ToLower(strings.TrimSpace(parameters.Verbosity))
	if !oneOf(parameters.Verbosity, "", "low", "medium", "high") {
		return LLMParameters{}, fmt.Errorf("-verbosity must be low, medium, or high")
	}
	if seedSet {
		parameters.Seed = &seed
	}
	return parameters, nil
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
