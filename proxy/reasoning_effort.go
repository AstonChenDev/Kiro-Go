package proxy

import (
	"fmt"
	"strings"
)

const (
	defaultMaxThinkingLength = 200000

	// ThinkingModePrompt remains the legacy/default Kiro thinking prompt used by
	// the model-name suffix and Claude compatibility path.
	ThinkingModePrompt = `<thinking_mode>enabled</thinking_mode>
<max_thinking_length>200000</max_thinking_length>`
)

var reasoningEffortBudgets = map[string]int{
	"minimal": 256,
	"low":     1024,
	"medium":  4096,
	"high":    16384,
	"xhigh":   65536,
	"max":     200000,
}

const reasoningEffortValues = "none, minimal, low, medium, high, xhigh, max"

// openAIThinkingMode is the normalized internal representation shared by Chat
// Completions and Responses. MaxThinkingLength is a compatibility budget sent
// through Kiro's prompt protocol; Kiro treats it as a soft budget, not a strict
// token or character limit.
type openAIThinkingMode struct {
	Enabled           bool
	MaxThinkingLength int
	Effort            string
	Explicit          bool
}

// resolveOpenAIThinkingMode strips the legacy model suffix and then applies an
// explicitly supplied OpenAI reasoning effort. Explicit input is authoritative:
// reasoning_effort=none disables thinking even when the model has -thinking.
func resolveOpenAIThinkingMode(model string, effort *string, fieldName, thinkingSuffix string) (string, openAIThinkingMode, error) {
	actualModel, suffixThinking := ParseModelAndThinking(model, thinkingSuffix)
	if effort == nil {
		if !suffixThinking {
			return actualModel, openAIThinkingMode{}, nil
		}
		return actualModel, openAIThinkingMode{
			Enabled:           true,
			MaxThinkingLength: defaultMaxThinkingLength,
			Effort:            "max",
		}, nil
	}

	value := *effort
	if value == "none" {
		return actualModel, openAIThinkingMode{Effort: value, Explicit: true}, nil
	}
	budget, ok := reasoningEffortBudgets[value]
	if !ok {
		if fieldName == "" {
			fieldName = "reasoning_effort"
		}
		return actualModel, openAIThinkingMode{}, fmt.Errorf("%s must be one of: %s", fieldName, reasoningEffortValues)
	}
	return actualModel, openAIThinkingMode{
		Enabled:           true,
		MaxThinkingLength: budget,
		Effort:            value,
		Explicit:          true,
	}, nil
}

func thinkingModePrompt(maxThinkingLength int) string {
	if maxThinkingLength <= 0 || maxThinkingLength == defaultMaxThinkingLength {
		return ThinkingModePrompt
	}
	return fmt.Sprintf("<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>%d</max_thinking_length>", maxThinkingLength)
}

func defaultOpenAIThinkingMode(enabled bool) openAIThinkingMode {
	if !enabled {
		return openAIThinkingMode{}
	}
	return openAIThinkingMode{
		Enabled:           true,
		MaxThinkingLength: defaultMaxThinkingLength,
		Effort:            "max",
	}
}

func estimateOpenAIRequestInputTokensWithThinking(req *OpenAIRequest, mode openAIThinkingMode) int {
	tokens := estimateOpenAIRequestInputTokens(req)
	if mode.Enabled {
		tokens += estimateApproxTokens(thinkingModePrompt(mode.MaxThinkingLength))
	}
	return tokens
}

func resolveResponsesThinkingMode(model string, reasoning *ResponsesReasoningConfig, thinkingSuffix string) (string, openAIThinkingMode, error) {
	if err := validateResponsesReasoningConfig(reasoning); err != nil {
		return "", openAIThinkingMode{}, err
	}
	if reasoning == nil {
		return resolveOpenAIThinkingMode(model, nil, "reasoning.effort", thinkingSuffix)
	}
	if reasoning.Effort != nil {
		return resolveOpenAIThinkingMode(model, reasoning.Effort, "reasoning.effort", thinkingSuffix)
	}
	// A reasoning object without an explicit effort uses the OpenAI-style
	// model default. Medium is the compatibility default for this proxy.
	defaultEffort := "medium"
	return resolveOpenAIThinkingMode(model, &defaultEffort, "reasoning.effort", thinkingSuffix)
}

func validateResponsesReasoningConfig(reasoning *ResponsesReasoningConfig) error {
	if reasoning == nil {
		return nil
	}
	fields := []struct {
		name  string
		value *string
	}{
		{name: "reasoning.summary", value: reasoning.Summary},
		{name: "reasoning.generate_summary", value: reasoning.GenerateSummary},
	}
	for _, field := range fields {
		fieldName, value := field.name, field.value
		if value == nil {
			continue
		}
		switch *value {
		case "auto", "concise", "detailed":
		default:
			return fmt.Errorf("%s must be one of: auto, concise, detailed", fieldName)
		}
	}
	return nil
}

func responsesReasoningSummaryMode(reasoning *ResponsesReasoningConfig) string {
	if reasoning == nil {
		return ""
	}
	if reasoning.Summary != nil {
		return strings.TrimSpace(*reasoning.Summary)
	}
	if reasoning.GenerateSummary != nil {
		return strings.TrimSpace(*reasoning.GenerateSummary)
	}
	return ""
}
