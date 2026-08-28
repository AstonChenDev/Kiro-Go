package proxy

// API-key four-endpoint routing is intentionally isolated from the legacy
// Kiro/OAuth request loop in kiro.go. The feature is opt-in so updating the
// upstream project keeps the old path byte-for-byte compatible and rollback is
// an environment change rather than a code revert.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
)

const (
	apiKeyRouteModeEnv      = "KIRO_APIKEY_ROUTE_MODE"
	apiKeyRouteAllowlistEnv = "KIRO_APIKEY_ROUTE_ACCOUNT_IDS"
	apiKeyRouteModeFour     = "four"
	apiKeyFourEndpointCount = 4
)

type apiKeyRouteEndpoint struct {
	Name      string
	URL       string
	AmzTarget string
}

type apiKeyRouteCompletion struct {
	sawOutput     bool
	sawToolUse    bool
	sawStopReason bool
}

// trackAPIKeyRouteCompletion adapts the Q/IDE API-key stream to the stricter
// completeness contract used by the rest of this project. Q can close a valid,
// frame-aligned 2xx stream after content without emitting metadataEvent's
// stopReason. Keep forwarding output immediately, while recording enough state
// to supply the missing terminal signal after a clean parser return.
func trackAPIKeyRouteCompletion(callback *KiroStreamCallback) (*KiroStreamCallback, *apiKeyRouteCompletion) {
	state := &apiKeyRouteCompletion{}
	tracked := &KiroStreamCallback{}
	if callback != nil {
		*tracked = *callback
	}

	originalOnText := tracked.OnText
	tracked.OnText = func(text string, thinking bool) {
		if text != "" {
			state.sawOutput = true
		}
		if originalOnText != nil {
			originalOnText(text, thinking)
		}
	}
	originalOnToolUse := tracked.OnToolUse
	tracked.OnToolUse = func(toolUse KiroToolUse) {
		state.sawOutput = true
		state.sawToolUse = true
		if originalOnToolUse != nil {
			originalOnToolUse(toolUse)
		}
	}
	originalOnStopReason := tracked.OnStopReason
	tracked.OnStopReason = func(reason string) {
		if strings.TrimSpace(reason) != "" {
			state.sawStopReason = true
		}
		if originalOnStopReason != nil {
			originalOnStopReason(reason)
		}
	}
	originalOnCleanEOF := tracked.onCleanEOF
	tracked.onCleanEOF = func() {
		completeAPIKeyRouteStream(tracked, state)
		if originalOnCleanEOF != nil {
			originalOnCleanEOF()
		}
	}
	return tracked, state
}

func completeAPIKeyRouteStream(callback *KiroStreamCallback, state *apiKeyRouteCompletion) {
	if callback == nil || state == nil || !state.sawOutput || state.sawStopReason {
		return
	}
	reason := "end_turn"
	if state.sawToolUse {
		reason = "tool_use"
	}
	callback.OnStopReason(reason)
	logger.Debugf("[KiroAPIKeyRoute] Synthesized missing stop reason %s after clean 2xx stream", reason)
}

// apiKeyFourEndpointEnabled gates the new path without changing persisted
// configuration or the admin API. An optional account-id allowlist supports a
// one-key production canary before the mode is enabled pool-wide.
func apiKeyFourEndpointEnabled(account *config.Account) bool {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(apiKeyRouteModeEnv)), apiKeyRouteModeFour) {
		return false
	}

	allowlist := strings.TrimSpace(os.Getenv(apiKeyRouteAllowlistEnv))
	if allowlist == "" {
		return true
	}
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return false
	}
	for _, id := range strings.Split(allowlist, ",") {
		if strings.TrimSpace(id) == account.ID {
			return true
		}
	}
	return false
}

// apiKeyFourEndpointPlan returns the fixed protocol order verified against the
// live Kiro API. CodeWhisperer has no regional hostname outside us-east-1, so
// its target is sent to the regional Amazon Q host there.
func apiKeyFourEndpointPlan(region string) []apiKeyRouteEndpoint {
	region = strings.TrimSpace(strings.ToLower(region))
	if !kiroRegionPattern.MatchString(region) {
		region = "us-east-1"
	}

	qURL := fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", region)
	runtimeURL := fmt.Sprintf("https://runtime.%s.kiro.dev/generateAssistantResponse", region)
	codeWhispererURL := "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse"
	if region != "us-east-1" {
		codeWhispererURL = qURL
	}

	return []apiKeyRouteEndpoint{
		{Name: "API Key Q IDE", URL: qURL},
		{Name: "API Key Kiro Runtime", URL: runtimeURL},
		{
			Name:      "API Key CodeWhisperer",
			URL:       codeWhispererURL,
			AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		},
		{
			Name:      "API Key AmazonQ",
			URL:       qURL,
			AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		},
	}
}

// callAPIKeyFourEndpoints attempts four upstream throttling paths with one
// credential and one immutable request body. A 2xx response is authoritative:
// its stream is consumed immediately and no later endpoint is contacted, even
// if that accepted stream subsequently proves malformed. This avoids duplicate
// generations after an upstream has already accepted the request.
func callAPIKeyFourEndpoints(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if account == nil {
		return fmt.Errorf("API key account is nil")
	}
	if payload == nil {
		return fmt.Errorf("Kiro payload is nil")
	}

	// The live four-endpoint protocol accepts the IDE body on every host. The
	// Claude translator already supplies these IDE envelope fields, while the
	// OpenAI translator intentionally omits them for the legacy CLI runtime.
	// Normalize them here, inside the isolated route, so both public APIs produce
	// a valid Q/IDE request without coupling either translator to this feature.
	// Do it once before marshaling; every endpoint receives the exact same bytes.
	payload.ConversationState.CurrentMessage.UserInputMessage.Origin = "AI_EDITOR"
	if strings.TrimSpace(payload.ConversationState.AgentTaskType) == "" {
		payload.ConversationState.AgentTaskType = "vibe"
	}
	if strings.TrimSpace(payload.ConversationState.AgentContinuationId) == "" {
		payload.ConversationState.AgentContinuationId = uuid.New().String()
	}
	if strings.TrimSpace(payload.ConversationState.ChatTriggerType) == "" {
		payload.ConversationState.ChatTriggerType = "MANUAL"
	}
	if strings.TrimSpace(payload.ConversationState.ConversationID) == "" {
		payload.ConversationState.ConversationID = uuid.New().String()
	}
	requestBody, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	endpoints := apiKeyFourEndpointPlan(account.EffectiveApiRegion())
	client := GetClientForProxy(ResolveAccountProxyURL(account))
	invocationID := uuid.New().String()
	var lastErr error

	for index, endpoint := range endpoints {
		if err := ctx.Err(); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL, bytes.NewReader(requestBody))
		if err != nil {
			return err
		}
		headerValues := buildStreamingHeaderValues(account, req.URL.Host)
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("Accept", "*/*")
		applyKiroBaseHeaders(req, account, headerValues)
		req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
		req.Header.Set("x-amzn-codewhisperer-optout", "false")
		req.Header.Set("Amz-Sdk-Request", fmt.Sprintf("attempt=%d; max=%d", index+1, len(endpoints)))
		req.Header.Set("Amz-Sdk-Invocation-Id", invocationID)
		if endpoint.AmzTarget != "" {
			req.Header.Set("X-Amz-Target", endpoint.AmzTarget)
		}

		resp, requestErr := client.Do(req)
		if requestErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			lastErr = fmt.Errorf("%s request failed: %w", endpoint.Name, requestErr)
			logger.Warnf("[KiroAPIKeyRoute] Endpoint %s transport failure, trying next: %v", endpoint.Name, requestErr)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			trackedCallback, _ := trackAPIKeyRouteCompletion(callback)
			emitted, streamErr := parseEventStreamTracked(resp.Body, trackedCallback)
			resp.Body.Close()
			if streamErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				// The endpoint accepted the generation. Never replay it on a
				// different throttling path, regardless of whether output was seen.
				logger.Warnf("[KiroAPIKeyRoute] Endpoint %s accepted request but stream failed (emitted=%t): %v", endpoint.Name, emitted, streamErr)
				return streamErr
			}
			logger.Debugf("[KiroAPIKeyRoute] Endpoint %s succeeded", endpoint.Name)
			return nil
		}

		errorBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxKiroErrorBodyBytes))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("HTTP %d from %s (read error body: %w)", resp.StatusCode, endpoint.Name, readErr)
		} else {
			message := strings.TrimSpace(string(errorBody))
			if message == "" {
				message = http.StatusText(resp.StatusCode)
			}
			if resp.StatusCode == http.StatusBadRequest && isImproperlyFormedRejection(message) {
				return newUpstreamPermanentError(resp.StatusCode, message)
			}
			lastErr = fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, endpoint.Name, message)
		}

		if !shouldFallbackAPIKeyEndpoint(resp.StatusCode) {
			return lastErr
		}
		if index+1 < len(endpoints) {
			logger.Warnf("[KiroAPIKeyRoute] Endpoint %s returned HTTP %d, trying next", endpoint.Name, resp.StatusCode)
		}
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all API key endpoints failed")
}

func shouldFallbackAPIKeyEndpoint(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		(status >= http.StatusInternalServerError && status <= 599)
}
