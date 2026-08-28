package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func stringPointer(value string) *string { return &value }

func TestResolveOpenAIThinkingModeMapsStandardEfforts(t *testing.T) {
	tests := []struct {
		effort string
		budget int
	}{
		{effort: "minimal", budget: 256},
		{effort: "low", budget: 1024},
		{effort: "medium", budget: 4096},
		{effort: "high", budget: 16384},
		{effort: "xhigh", budget: 65536},
		{effort: "max", budget: 200000},
	}
	for _, tc := range tests {
		t.Run(tc.effort, func(t *testing.T) {
			model, mode, err := resolveOpenAIThinkingMode("claude-sonnet-4.5", stringPointer(tc.effort), "reasoning_effort", "-thinking")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if model != "claude-sonnet-4.5" || !mode.Enabled || !mode.Explicit || mode.Effort != tc.effort || mode.MaxThinkingLength != tc.budget {
				t.Fatalf("unexpected resolution: model=%q mode=%+v", model, mode)
			}
			prompt := thinkingModePrompt(mode.MaxThinkingLength)
			if !strings.Contains(prompt, "<max_thinking_length>"+strconv.Itoa(tc.budget)+"</max_thinking_length>") {
				t.Fatalf("budget missing from prompt: %q", prompt)
			}
		})
	}
}

func TestResolveOpenAIThinkingModePrecedenceAndValidation(t *testing.T) {
	model, mode, err := resolveOpenAIThinkingMode("claude-sonnet-4.5-thinking", stringPointer("none"), "reasoning_effort", "-thinking")
	if err != nil {
		t.Fatalf("resolve explicit none: %v", err)
	}
	if model != "claude-sonnet-4.5" || mode.Enabled || !mode.Explicit {
		t.Fatalf("explicit none must override suffix, got model=%q mode=%+v", model, mode)
	}

	model, mode, err = resolveOpenAIThinkingMode("claude-sonnet-4.5-thinking", nil, "reasoning_effort", "-thinking")
	if err != nil || model != "claude-sonnet-4.5" || !mode.Enabled || mode.MaxThinkingLength != defaultMaxThinkingLength {
		t.Fatalf("legacy suffix resolution failed: model=%q mode=%+v err=%v", model, mode, err)
	}

	_, mode, err = resolveOpenAIThinkingMode("claude-sonnet-4.5", nil, "reasoning_effort", "-thinking")
	if err != nil || mode.Enabled {
		t.Fatalf("absent effort without suffix must stay disabled: mode=%+v err=%v", mode, err)
	}

	for _, invalid := range []string{"", "HIGH", "ultra", " high "} {
		_, _, err = resolveOpenAIThinkingMode("claude-sonnet-4.5", stringPointer(invalid), "reasoning_effort", "-thinking")
		if err == nil {
			t.Fatalf("expected %q to be rejected", invalid)
		}
	}
}

func TestResolveResponsesThinkingModeDefaultsAndValidatesSummary(t *testing.T) {
	model, mode, err := resolveResponsesThinkingMode("claude-sonnet-4.5", &ResponsesReasoningConfig{}, "-thinking")
	if err != nil || model != "claude-sonnet-4.5" || !mode.Enabled || mode.Effort != "medium" || mode.MaxThinkingLength != 4096 {
		t.Fatalf("reasoning object default failed: model=%q mode=%+v err=%v", model, mode, err)
	}

	for _, summary := range []string{"auto", "concise", "detailed"} {
		_, _, err = resolveResponsesThinkingMode("claude-sonnet-4.5", &ResponsesReasoningConfig{Summary: stringPointer(summary)}, "-thinking")
		if err != nil {
			t.Fatalf("valid summary %q rejected: %v", summary, err)
		}
	}
	_, _, err = resolveResponsesThinkingMode("claude-sonnet-4.5", &ResponsesReasoningConfig{Summary: stringPointer("verbose")}, "-thinking")
	if err == nil {
		t.Fatal("invalid reasoning.summary must be rejected")
	}
}

func TestOpenAIChatReasoningEffortInjectsBudgetAndReturnsReasoning(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var upstreamBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "<thinking>reasoned carefully</thinking>42",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"reasoning_effort":"medium",
		"messages":[{"role":"user","content":"answer"}]
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertUpstreamThinkingBudget(t, upstreamBody, 4096)
	var response struct {
		Choices []struct {
			Message map[string]interface{} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Choices) != 1 || response.Choices[0].Message["reasoning_content"] != "reasoned carefully" || response.Choices[0].Message["content"] != "42" {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
}

func TestOpenAIChatReasoningEffortNoneOverridesSuffix(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var upstreamBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "42"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"claude-sonnet-4.5-thinking",
		"reasoning_effort":"none",
		"messages":[{"role":"user","content":"answer"}]
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var captured KiroPayload
	if err := json.Unmarshal([]byte(upstreamBody), &captured); err != nil {
		t.Fatalf("decode upstream payload: %v", err)
	}
	for _, history := range captured.ConversationState.History {
		if history.UserInputMessage != nil && strings.Contains(history.UserInputMessage.Content, "<thinking_mode>") {
			t.Fatalf("explicit none must suppress legacy suffix prompt: %s", upstreamBody)
		}
	}
}

func TestOpenAIChatRejectsInvalidReasoningEffort(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"reasoning_effort":"ultra",
		"messages":[{"role":"user","content":"answer"}]
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "reasoning_effort must be one of") {
		t.Fatalf("expected validation error, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOpenAIChatReasoningEffortStreamSeparatesReasoning(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var upstreamBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.WriteHeader(http.StatusOK)
		for _, chunk := range []string{"<thin", "king>stream reason</thin", "king>stream answer"} {
			_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": chunk}))
		}
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"reasoning_effort":"xhigh",
		"stream":true,
		"messages":[{"role":"user","content":"answer"}]
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
	assertUpstreamThinkingBudget(t, upstreamBody, 65536)
	for _, expected := range []string{`"reasoning_content":"stream reason"`, `"content":"stream answer"`, "data: [DONE]"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing %q in stream:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "<thinking>") || strings.Contains(body, "</thinking>") {
		t.Fatalf("thinking markup leaked to client stream:\n%s", body)
	}
}

func TestResponsesReasoningEffortNonStreamUsesStandardItem(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var upstreamBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "<thinking>upstream reasoning</thinking>final answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"answer",
		"reasoning":{"effort":"high","summary":"auto"},
		"store":false
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertUpstreamThinkingBudget(t, upstreamBody, 16384)
	var response ResponsesObject
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Output) != 2 || response.Output[0].Type != "reasoning" || len(response.Output[0].Summary) != 1 || response.Output[0].Summary[0].Type != "summary_text" || response.Output[0].Summary[0].Text != "upstream reasoning" {
		t.Fatalf("unexpected reasoning output: %s", rec.Body.String())
	}
	if response.Output[1].Type != "message" || response.Output[1].Content[0].Text != "final answer" {
		t.Fatalf("unexpected answer output: %s", rec.Body.String())
	}
}

func TestResponsesReasoningWithoutSummaryUsesReasoningText(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "native reasoning"}))
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "answer"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"answer",
		"reasoning":{"effort":"minimal"},
		"store":false
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response ResponsesObject
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Output) != 2 || response.Output[0].Type != "reasoning" || len(response.Output[0].Content) != 1 || response.Output[0].Content[0].Type != "reasoning_text" || response.Output[0].Content[0].Text != "native reasoning" || len(response.Output[0].Summary) != 0 {
		t.Fatalf("unexpected reasoning output: %s", rec.Body.String())
	}
}

func assertUpstreamThinkingBudget(t *testing.T, upstreamBody string, budget int) {
	t.Helper()
	var captured KiroPayload
	if err := json.Unmarshal([]byte(upstreamBody), &captured); err != nil {
		t.Fatalf("decode upstream payload: %v", err)
	}
	want := "<max_thinking_length>" + strconv.Itoa(budget) + "</max_thinking_length>"
	for _, history := range captured.ConversationState.History {
		if history.UserInputMessage != nil && strings.Contains(history.UserInputMessage.Content, want) {
			return
		}
	}
	t.Fatalf("mapped budget %d did not reach mock upstream: %s", budget, upstreamBody)
}

func TestResponsesRejectsInvalidReasoningConfig(t *testing.T) {
	for _, body := range []string{
		`{"model":"claude-sonnet-4.5","input":"answer","reasoning":{"effort":"ultra"}}`,
		`{"model":"claude-sonnet-4.5","input":"answer","reasoning":{"summary":"verbose"}}`,
	} {
		h, cleanup := setupResponsesTestHandler(t)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.handleOpenAIResponses(rec, req)
		cleanup()
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func TestResponsesReasoningStreamSeparatesSplitThinkingTags(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for _, chunk := range []string{"<thin", "king>step ", "one</thin", "king>final answer"} {
			_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": chunk}))
		}
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"answer",
		"reasoning":{"effort":"low"},
		"stream":true,
		"store":false
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
	for _, expected := range []string{
		"event: response.reasoning_text.delta",
		"event: response.reasoning_text.done",
		`"type":"reasoning"`,
		`"type":"reasoning_text","text":"step one"`,
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"final answer",
		"event: response.completed",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing %q in stream:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "<thinking>") || strings.Contains(body, "</thinking>") {
		t.Fatalf("thinking markup leaked to client stream:\n%s", body)
	}
}

func TestResponsesReasoningStreamEmitsSummaryEvents(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "native reason"}))
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "answer"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"answer",
		"reasoning":{"effort":"high","summary":"auto"},
		"stream":true,
		"store":false
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)
	body := rec.Body.String()
	for _, expected := range []string{
		"event: response.reasoning_summary_part.added",
		"event: response.reasoning_summary_text.delta",
		"event: response.reasoning_summary_text.done",
		"event: response.reasoning_summary_part.done",
		`"type":"summary_text","text":"native reason"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing %q in stream:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "event: response.reasoning_text.delta") {
		t.Fatalf("summary request emitted raw reasoning event shape:\n%s", body)
	}
}

func TestResponsesReasoningNoneStripsUnexpectedThinkingTags(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "<thinking>hidden</thinking>answer"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5-thinking",
		"input":"answer",
		"reasoning":{"effort":"none"},
		"stream":true,
		"store":false
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "answer") {
		t.Fatalf("answer missing:\n%s", body)
	}
	if strings.Contains(body, "hidden") || strings.Contains(body, "reasoning_text") || strings.Contains(body, "<thinking>") {
		t.Fatalf("disabled reasoning leaked upstream thinking:\n%s", body)
	}
}
