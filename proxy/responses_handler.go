package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

const defaultResponsesModel = "claude-sonnet-4.5"

func (h *Handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultResponsesModel
	}

	storedInputCopy := append(json.RawMessage(nil), req.Input...)

	storeResponse := true
	if req.Store != nil {
		storeResponse = *req.Store
	}

	var historyMessages []OpenAIMessage
	apiKeyID := apiKeyIDFromContext(r.Context())
	if req.PreviousResponseID != "" {
		prev, loadErr := loadResponseForOwner(req.PreviousResponseID, apiKeyID)
		if loadErr != nil {
			// Log the real reason server-side for ops, but return a single fixed
			// generic message to the client so a bad id never leaks the server's
			// filesystem path (the raw os.PathError) nor distinguishes missing vs
			// expired vs not-owner.
			logger.Warnf("[Responses] previous_response_id %q load failed: %v", req.PreviousResponseID, loadErr)
			h.sendOpenAIError(w, 404, "invalid_request_error", "previous_response_id not found")
			return
		}
		historyMessages = expandPreviousResponseHistory(prev)
	}

	inputMessages, err := parseResponsesInput(req.Input)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}

	finalMessages := make([]OpenAIMessage, 0, len(historyMessages)+len(inputMessages)+1)
	finalMessages = append(finalMessages, historyMessages...)
	if strings.TrimSpace(req.Instructions) != "" {
		// New instructions on this turn always take effect, even when
		// continuing from previous_response_id. Place them after the
		// expanded history so they apply to the current and future turns,
		// while ancestor instructions (re-emitted by expandPreviousResponseHistory)
		// stay in scope for the historical exchanges they shaped.
		finalMessages = append(finalMessages, OpenAIMessage{
			Role:    "system",
			Content: req.Instructions,
		})
	}
	finalMessages = append(finalMessages, inputMessages...)

	if len(finalMessages) == 0 {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one message")
		return
	}

	hasUser := false
	for _, m := range finalMessages {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one user message")
		return
	}

	openaiReq := &OpenAIRequest{
		Model:    req.Model,
		Messages: finalMessages,
		Stream:   req.Stream,
		Tools:    req.Tools,
	}
	if req.Temperature != nil {
		openaiReq.Temperature = req.Temperature
	}
	if req.MaxOutputTokens != nil {
		openaiReq.MaxTokens = *req.MaxOutputTokens
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinkingMode, err := resolveResponsesThinkingMode(req.Model, req.Reasoning, thinkingCfg.Suffix)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}
	openaiReq.Model = actualModel

	estimatedInputTokens := estimateOpenAIRequestInputTokensWithThinking(openaiReq, thinkingMode)
	kiroPayload := openAIToKiroWithThinkingMode(openaiReq, thinkingMode)

	respID := generateResponseID()

	if req.Stream {
		h.handleResponsesStream(r.Context(), w, kiroPayload, actualModel, thinkingMode.Enabled, estimatedInputTokens,
			apiKeyID, respID, &req, storedInputCopy, storeResponse)
		return
	}

	h.handleResponsesNonStream(r.Context(), w, kiroPayload, actualModel, thinkingMode.Enabled, estimatedInputTokens,
		apiKeyID, respID, &req, storedInputCopy, storeResponse)
}

func (h *Handler) handleResponsesNonStream(
	ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	excluded := make(map[string]bool)
	var lastErr error
	reqStart := time.Now()

	retryBudget := resolveAccountRetryBudget(h.pool.Count())
	for attempt := 0; attempt < retryBudget; attempt++ {
		if attempt > 0 {
			time.Sleep(accountRetryBackoff(attempt - 1))
		}
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			break
		}
		if !h.pool.Acquire(account.ID, false) {
			// Account selection already reserved a scheduler in-flight slot.
			// Acquire rejected before dispatch, so finish it neutrally.
			h.pool.RecordPermanentRejection(account.ID)
			excluded[account.ID] = true
			attempt--
			continue
		}
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var content, reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamStopReason string

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnStopReason: func(reason string) {
				upstreamStopReason = reason
			},
		}

		measure := func() (int, int, string, bool) {
			return len(content), len(toolUses), upstreamStopReason, reasoningContent != ""
		}

		reset := func() {
			content = ""
			reasoningContent = ""
			toolUses = nil
			inputTokens = 0
			outputTokens = 0
			credits = 0
			realInputTokens = 0
			upstreamStopReason = ""
		}

		// Fully buffered path: nothing reaches the client until the response is
		// encoded, so a retry can never duplicate output.
		err := runKiroWithIntegrityRetry(ctx, account, payload, callback, measure, reset, nil)
		if err != nil {
			if ctx.Err() != nil {
				h.pool.RecordPermanentRejection(account.ID)
				return
			}
			lastErr = err
			excluded[account.ID] = true
			// Integrity failures are upstream hiccups, not account faults.
			if isStreamIntegrityError(err) {
				h.pool.RecordPermanentRejection(account.ID)
			} else {
				h.handleAccountFailure(account, err)
			}
			if isUpstreamPermanentError(err) {
				break
			}
			continue
		}

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordSuccessLog("responses", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		respObj := buildResponsesObject(respID, model, finalContent, reasoningContent, toolUses, inputTokens, outputTokens, req, upstreamStopReason)
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions
		respObj.OwnerKeyID = apiKeyID

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(respObj)
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}
	h.recordFailureWithDetails("responses", model, "", lastErr)
	h.sendOpenAIError(w, 500, "server_error", improperlyFormedClientMessage(lastErr))
}

func mapResponsesCompletion(reason string) (status, incompleteReason string) {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "max_tokens", "max_output_tokens", "length", "model_context_window_exceeded", "context_window_exceeded":
		return "incomplete", "max_output_tokens"
	case "refusal", "content_filter", "content_filtered", "guardrail_intervened":
		return "incomplete", "content_filter"
	default:
		return "completed", ""
	}
}

func buildResponsesObject(
	id, model, content, reasoningContent string, toolUses []KiroToolUse,
	inputTokens, outputTokens int, req *ResponsesRequest, upstreamStopReason string,
) *ResponsesObject {
	output := make([]ResponseOutputItem, 0, 2+len(toolUses))

	if strings.TrimSpace(reasoningContent) != "" {
		item := ResponseOutputItem{
			ID:     generateOutputItemID("rs"),
			Type:   "reasoning",
			Status: "completed",
		}
		partType := "reasoning_text"
		if responsesReasoningSummaryMode(req.Reasoning) != "" {
			partType = "summary_text"
			item.Summary = []ResponseContentPart{{Type: partType, Text: reasoningContent}}
		} else {
			item.Content = []ResponseContentPart{{Type: partType, Text: reasoningContent}}
		}
		output = append(output, item)
	}

	if strings.TrimSpace(content) != "" {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: content,
			}},
		})
	}

	for _, tu := range toolUses {
		args, _ := json.Marshal(tu.Input)
		output = append(output, ResponseOutputItem{
			ID:        generateOutputItemID("fc"),
			Type:      "function_call",
			Status:    "completed",
			CallID:    tu.ToolUseID,
			Name:      tu.Name,
			Arguments: string(args),
		})
	}

	if len(output) == 0 {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "",
			}},
		})
	}

	status, incompleteReason := mapResponsesCompletion(upstreamStopReason)
	var incompleteDetails *ResponsesIncompleteDetails
	if incompleteReason != "" {
		incompleteDetails = &ResponsesIncompleteDetails{Reason: incompleteReason}
	}

	return &ResponsesObject{
		ID:                 id,
		Object:             "response",
		CreatedAt:          time.Now().Unix(),
		Status:             status,
		Model:              model,
		Output:             output,
		Usage:              ResponsesUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
		Reasoning:          req.Reasoning,
		IncompleteDetails:  incompleteDetails,
	}
}

func (h *Handler) handleResponsesStream(
	ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	send := func(eventName string, payload interface{}) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(data))
		flusher.Flush()
	}

	createdAt := time.Now().Unix()
	initial := &ResponsesObject{
		ID:                 respID,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "in_progress",
		Model:              model,
		Output:             []ResponseOutputItem{},
		Usage:              ResponsesUsage{},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
		Reasoning:          req.Reasoning,
	}
	send("response.created", map[string]interface{}{
		"type":     "response.created",
		"response": initial,
	})

	excluded := make(map[string]bool)
	var lastErr error
	responseStarted := false
	reqStart := time.Now()

	retryBudget := resolveAccountRetryBudget(h.pool.Count())
	for attempt := 0; attempt < retryBudget; attempt++ {
		if attempt > 0 {
			time.Sleep(accountRetryBackoff(attempt - 1))
		}
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			break
		}
		if !h.pool.Acquire(account.ID, true) {
			// Acquire did not take an SSE slot, but selection did reserve the
			// scheduler slot. Return that reservation without penalising health.
			h.pool.RecordPermanentRejection(account.ID)
			excluded[account.ID] = true
			attempt--
			continue
		}
		if err := h.ensureValidToken(account); err != nil {
			h.pool.Release(account.ID, true)
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		send("response.in_progress", map[string]interface{}{
			"type":     "response.in_progress",
			"response": initial,
		})

		var (
			rawContent         strings.Builder
			rawEventReasoning  strings.Builder
			finalText          strings.Builder
			finalReasoning     strings.Builder
			currentMessageText strings.Builder
			currentReasoning   strings.Builder
			toolUses           []KiroToolUse
			inputTokens        int
			outputTokens       int
			credits            float64
			realInputTokens    int
			upstreamStopReason string
		)

		messageItemID := ""
		messageItemIndex := -1
		messageStarted := false
		reasoningItemID := ""
		reasoningItemIndex := -1
		reasoningStarted := false
		outputIndex := 0
		contentIndex := 0
		summaryMode := responsesReasoningSummaryMode(req.Reasoning)

		closeReasoning := func() {
			if !reasoningStarted {
				return
			}
			text := currentReasoning.String()
			if summaryMode != "" {
				send("response.reasoning_summary_text.done", map[string]interface{}{
					"type":          "response.reasoning_summary_text.done",
					"item_id":       reasoningItemID,
					"output_index":  reasoningItemIndex,
					"summary_index": 0,
					"text":          text,
				})
				send("response.reasoning_summary_part.done", map[string]interface{}{
					"type":          "response.reasoning_summary_part.done",
					"item_id":       reasoningItemID,
					"output_index":  reasoningItemIndex,
					"summary_index": 0,
					"part": map[string]string{
						"type": "summary_text",
						"text": text,
					},
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": reasoningItemIndex,
					"item": map[string]interface{}{
						"id":      reasoningItemID,
						"type":    "reasoning",
						"status":  "completed",
						"summary": []map[string]string{{"type": "summary_text", "text": text}},
					},
				})
			} else {
				send("response.reasoning_text.done", map[string]interface{}{
					"type":          "response.reasoning_text.done",
					"item_id":       reasoningItemID,
					"output_index":  reasoningItemIndex,
					"content_index": 0,
					"text":          text,
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": reasoningItemIndex,
					"item": map[string]interface{}{
						"id":      reasoningItemID,
						"type":    "reasoning",
						"status":  "completed",
						"content": []map[string]string{{"type": "reasoning_text", "text": text}},
					},
				})
			}
			reasoningStarted = false
			reasoningItemID = ""
			reasoningItemIndex = -1
			currentReasoning.Reset()
			outputIndex++
		}

		closeMessage := func() {
			if !messageStarted {
				return
			}
			text := currentMessageText.String()
			send("response.output_text.done", map[string]interface{}{
				"type":          "response.output_text.done",
				"item_id":       messageItemID,
				"output_index":  messageItemIndex,
				"content_index": contentIndex,
				"text":          text,
			})
			send("response.content_part.done", map[string]interface{}{
				"type":          "response.content_part.done",
				"item_id":       messageItemID,
				"output_index":  messageItemIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": text,
				},
			})
			send("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": messageItemIndex,
				"item": map[string]interface{}{
					"id":     messageItemID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]interface{}{{
						"type": "output_text",
						"text": text,
					}},
				},
			})
			messageStarted = false
			messageItemID = ""
			messageItemIndex = -1
			currentMessageText.Reset()
			outputIndex++
		}

		ensureMessageStarted := func() {
			if messageStarted {
				return
			}
			closeReasoning()
			messageStarted = true
			messageItemID = generateOutputItemID("msg")
			messageItemIndex = outputIndex
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": messageItemIndex,
				"item": map[string]interface{}{
					"id":      messageItemID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []map[string]interface{}{},
				},
			})
			send("response.content_part.added", map[string]interface{}{
				"type":          "response.content_part.added",
				"item_id":       messageItemID,
				"output_index":  messageItemIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": "",
				},
			})
			responseStarted = true
		}

		ensureReasoningStarted := func() {
			if reasoningStarted {
				return
			}
			closeMessage()
			reasoningStarted = true
			reasoningItemID = generateOutputItemID("rs")
			reasoningItemIndex = outputIndex
			item := map[string]interface{}{
				"id":     reasoningItemID,
				"type":   "reasoning",
				"status": "in_progress",
			}
			if summaryMode != "" {
				item["summary"] = []map[string]interface{}{}
			} else {
				item["content"] = []map[string]interface{}{}
			}
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": reasoningItemIndex,
				"item":         item,
			})
			if summaryMode != "" {
				send("response.reasoning_summary_part.added", map[string]interface{}{
					"type":          "response.reasoning_summary_part.added",
					"item_id":       reasoningItemID,
					"output_index":  reasoningItemIndex,
					"summary_index": 0,
					"part": map[string]string{
						"type": "summary_text",
						"text": "",
					},
				})
			}
			responseStarted = true
		}

		emitSegment := func(text string, state int) {
			if state == thinkingSegmentText {
				if text == "" {
					return
				}
				ensureMessageStarted()
				finalText.WriteString(text)
				currentMessageText.WriteString(text)
				send("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       messageItemID,
					"output_index":  messageItemIndex,
					"content_index": contentIndex,
					"delta":         text,
				})
				responseStarted = true
				return
			}
			if !thinking {
				return
			}
			ensureReasoningStarted()
			if text != "" {
				finalReasoning.WriteString(text)
				currentReasoning.WriteString(text)
				eventName := "response.reasoning_text.delta"
				payload := map[string]interface{}{
					"type":          eventName,
					"item_id":       reasoningItemID,
					"output_index":  reasoningItemIndex,
					"content_index": 0,
					"delta":         text,
				}
				if summaryMode != "" {
					eventName = "response.reasoning_summary_text.delta"
					payload["type"] = eventName
					delete(payload, "content_index")
					payload["summary_index"] = 0
				}
				send(eventName, payload)
			}
			if state == thinkingSegmentEnd {
				closeReasoning()
			}
		}

		streamParser := newThinkingStreamParser(thinking, emitSegment)

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawEventReasoning.WriteString(text)
				} else {
					rawContent.WriteString(text)
				}
				streamParser.Push(text, isThinking)
			},
			OnToolUse: func(tu KiroToolUse) {
				streamParser.Flush()
				closeReasoning()
				closeMessage()

				toolUses = append(toolUses, tu)
				args, _ := json.Marshal(tu.Input)
				fcID := generateOutputItemID("fc")
				send("response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "in_progress",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": "",
					},
				})
				send("response.function_call_arguments.delta", map[string]interface{}{
					"type":         "response.function_call_arguments.delta",
					"item_id":      fcID,
					"output_index": outputIndex,
					"delta":        string(args),
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "completed",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": string(args),
					},
				})
				outputIndex++
				responseStarted = true
			},
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnStopReason: func(reason string) {
				upstreamStopReason = reason
			},
		}
		measure := func() (int, int, string, bool) {
			return rawContent.Len(), len(toolUses), upstreamStopReason, rawEventReasoning.Len() > 0
		}

		// Retries only run while responseStarted is false, i.e. before any
		// content or function-call item has been sent, so the output_index /
		// content_index cursors are still untouched. Only the accumulators need
		// clearing.
		reset := func() {
			rawContent.Reset()
			rawEventReasoning.Reset()
			finalText.Reset()
			finalReasoning.Reset()
			currentMessageText.Reset()
			currentReasoning.Reset()
			toolUses = nil
			inputTokens = 0
			outputTokens = 0
			credits = 0
			realInputTokens = 0
			upstreamStopReason = ""
			messageItemID = ""
			messageItemIndex = -1
			messageStarted = false
			reasoningItemID = ""
			reasoningItemIndex = -1
			reasoningStarted = false
			outputIndex = 0
			streamParser.Reset()
		}

		err := runKiroWithIntegrityRetry(ctx, account, payload, callback, measure, reset,
			func() bool { return !responseStarted })
		if err != nil {
			h.pool.Release(account.ID, true)
			if ctx.Err() != nil {
				h.pool.RecordPermanentRejection(account.ID)
				return
			}
			lastErr = err
			excluded[account.ID] = true
			// Integrity failures are upstream hiccups, not account faults.
			if isStreamIntegrityError(err) {
				h.pool.RecordPermanentRejection(account.ID)
			} else {
				h.handleAccountFailure(account, err)
			}
			if isUpstreamPermanentError(err) && !responseStarted {
				break
			}
			if !responseStarted {
				continue
			}
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error": map[string]string{
						"type":    "server_error",
						"message": improperlyFormedClientMessage(err),
					},
				},
			})
			h.recordFailureWithDetails("responses", model, account.ID, err)
			return
		}

		streamParser.Flush()
		closeReasoning()
		closeMessage()
		finalContent := finalText.String()
		reasoning := finalReasoning.String()
		if !thinking {
			reasoning = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoning, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordSuccessLog("responses", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		respObj := buildResponsesObject(respID, model, finalContent, reasoning, toolUses, inputTokens, outputTokens, req, upstreamStopReason)
		respObj.CreatedAt = createdAt
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions
		respObj.OwnerKeyID = apiKeyID

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		send("response.completed", map[string]interface{}{
			"type":     "response.completed",
			"response": respObj,
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		h.pool.Release(account.ID, true)
		return
	}

	if lastErr == nil {
		send("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error": map[string]string{
					"type":    "server_error",
					"message": "No available accounts",
				},
			},
		})
		return
	}
	h.recordFailureWithDetails("responses", model, "", lastErr)
	send("response.failed", map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":     respID,
			"status": "failed",
			"error": map[string]string{
				"type":    "server_error",
				"message": improperlyFormedClientMessage(lastErr),
			},
		},
	})
}
