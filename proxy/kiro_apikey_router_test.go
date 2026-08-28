package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestAPIKeyFourEndpointEnabledIsOptInAndSupportsAllowlist(t *testing.T) {
	account := &config.Account{ID: "account-a"}

	t.Setenv(apiKeyRouteModeEnv, "")
	t.Setenv(apiKeyRouteAllowlistEnv, "")
	if apiKeyFourEndpointEnabled(account) {
		t.Fatal("four-endpoint routing must default to legacy/off")
	}

	t.Setenv(apiKeyRouteModeEnv, " FOUR ")
	if !apiKeyFourEndpointEnabled(account) {
		t.Fatal("four mode without an allowlist should enable every API-key account")
	}

	t.Setenv(apiKeyRouteAllowlistEnv, "account-b, account-c")
	if apiKeyFourEndpointEnabled(account) {
		t.Fatal("account outside the canary allowlist must stay on legacy routing")
	}
	if apiKeyFourEndpointEnabled(nil) {
		t.Fatal("nil account must not match a non-empty canary allowlist")
	}

	t.Setenv(apiKeyRouteAllowlistEnv, "account-b, account-a")
	if !apiKeyFourEndpointEnabled(account) {
		t.Fatal("account in the canary allowlist should use four-endpoint routing")
	}
}

func TestCallAPIKeyFourEndpointsValidatesInputsAndMarshalability(t *testing.T) {
	if err := callAPIKeyFourEndpoints(context.Background(), nil, newKiroRetryTestPayload(), &KiroStreamCallback{}); err == nil || !strings.Contains(err.Error(), "account is nil") {
		t.Fatalf("nil account error = %v", err)
	}
	account := &config.Account{ID: "account", AuthMethod: "api_key", KiroApiKey: "key"}
	if err := callAPIKeyFourEndpoints(context.Background(), account, nil, &KiroStreamCallback{}); err == nil || !strings.Contains(err.Error(), "payload is nil") {
		t.Fatalf("nil payload error = %v", err)
	}

	payload := newKiroRetryTestPayload()
	tool := KiroToolWrapper{}
	tool.ToolSpecification.InputSchema.JSON = make(chan int)
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		Tools: []KiroToolWrapper{tool},
	}
	if err := callAPIKeyFourEndpoints(context.Background(), account, payload, &KiroStreamCallback{}); err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("unmarshalable payload error = %v", err)
	}
}

func TestAPIKeyFourEndpointPlan(t *testing.T) {
	tests := []struct {
		name   string
		region string
		want   []apiKeyRouteEndpoint
	}{
		{
			name:   "us east keeps CodeWhisperer host",
			region: "us-east-1",
			want: []apiKeyRouteEndpoint{
				{Name: "API Key Q IDE", URL: "https://q.us-east-1.amazonaws.com/generateAssistantResponse"},
				{Name: "API Key Kiro Runtime", URL: "https://runtime.us-east-1.kiro.dev/generateAssistantResponse"},
				{Name: "API Key CodeWhisperer", URL: "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse", AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"},
				{Name: "API Key AmazonQ", URL: "https://q.us-east-1.amazonaws.com/generateAssistantResponse", AmzTarget: "AmazonQDeveloperStreamingService.SendMessage"},
			},
		},
		{
			name:   "non us east sends CodeWhisperer target to regional Q",
			region: "eu-central-1",
			want: []apiKeyRouteEndpoint{
				{Name: "API Key Q IDE", URL: "https://q.eu-central-1.amazonaws.com/generateAssistantResponse"},
				{Name: "API Key Kiro Runtime", URL: "https://runtime.eu-central-1.kiro.dev/generateAssistantResponse"},
				{Name: "API Key CodeWhisperer", URL: "https://q.eu-central-1.amazonaws.com/generateAssistantResponse", AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"},
				{Name: "API Key AmazonQ", URL: "https://q.eu-central-1.amazonaws.com/generateAssistantResponse", AmzTarget: "AmazonQDeveloperStreamingService.SendMessage"},
			},
		},
		{
			name:   "invalid region is contained to us east",
			region: "not/a/region",
			want: []apiKeyRouteEndpoint{
				{Name: "API Key Q IDE", URL: "https://q.us-east-1.amazonaws.com/generateAssistantResponse"},
				{Name: "API Key Kiro Runtime", URL: "https://runtime.us-east-1.kiro.dev/generateAssistantResponse"},
				{Name: "API Key CodeWhisperer", URL: "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse", AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"},
				{Name: "API Key AmazonQ", URL: "https://q.us-east-1.amazonaws.com/generateAssistantResponse", AmzTarget: "AmazonQDeveloperStreamingService.SendMessage"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := apiKeyFourEndpointPlan(tc.region)
			if len(got) != apiKeyFourEndpointCount {
				t.Fatalf("endpoint count = %d, want %d", len(got), apiKeyFourEndpointCount)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("endpoint plan:\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

type capturedAPIKeyRequest struct {
	Host          string
	RequestHost   string
	Path          string
	Target        string
	Authorization string
	TokenType     string
	ContentType   string
	AgentMode     string
	OptOut        string
	SDKRequest    string
	InvocationID  string
	Connection    string
	Close         bool
	Body          []byte
}

func TestAPIKeyFourEndpointRouteUsesSameKeyAndBodyInExactOrder(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	t.Setenv(apiKeyRouteAllowlistEnv, "")

	account := apiKeyRouterTestAccount(t, "eu-central-1")
	statuses := []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusRequestTimeout, http.StatusOK}
	var requests []capturedAPIKeyRequest
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		requests = append(requests, capturedAPIKeyRequest{
			Host:          req.URL.Host,
			RequestHost:   req.Host,
			Path:          req.URL.Path,
			Target:        req.Header.Get("X-Amz-Target"),
			Authorization: req.Header.Get("Authorization"),
			TokenType:     req.Header.Get("tokentype"),
			ContentType:   req.Header.Get("Content-Type"),
			AgentMode:     req.Header.Get("x-amzn-kiro-agent-mode"),
			OptOut:        req.Header.Get("x-amzn-codewhisperer-optout"),
			SDKRequest:    req.Header.Get("Amz-Sdk-Request"),
			InvocationID:  req.Header.Get("Amz-Sdk-Invocation-Id"),
			Connection:    req.Header.Get("Connection"),
			Close:         req.Close,
			Body:          body,
		})

		status := statuses[len(requests)-1]
		if status == http.StatusOK {
			return apiKeyRouterResponse(status, bytes.NewReader(awsEventStreamFrame(t,
				"assistantResponseEvent", map[string]interface{}{"content": "ok"}))), nil
		}
		return apiKeyRouterResponse(status, strings.NewReader(`{"message":"retry"}`)), nil
	}))

	payload := newKiroRetryTestPayload()
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.5"
	var text string
	err := CallKiroAPIContext(context.Background(), account, payload, &KiroStreamCallback{
		OnText: func(chunk string, _ bool) { text += chunk },
	})
	if err != nil {
		t.Fatalf("four-endpoint call failed: %v", err)
	}
	if text != "ok" {
		t.Fatalf("assistant text = %q, want ok", text)
	}
	if len(requests) != apiKeyFourEndpointCount {
		t.Fatalf("request count = %d, want %d", len(requests), apiKeyFourEndpointCount)
	}

	wantHosts := []string{
		"q.eu-central-1.amazonaws.com",
		"runtime.eu-central-1.kiro.dev",
		"q.eu-central-1.amazonaws.com",
		"q.eu-central-1.amazonaws.com",
	}
	wantTargets := []string{
		"",
		"",
		"AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		"AmazonQDeveloperStreamingService.SendMessage",
	}
	invocationID := requests[0].InvocationID
	if invocationID == "" {
		t.Fatal("logical request must have an invocation id")
	}
	for i, got := range requests {
		if got.Host != wantHosts[i] || got.RequestHost != wantHosts[i] {
			t.Errorf("request %d host = %q / %q, want %q", i+1, got.Host, got.RequestHost, wantHosts[i])
		}
		if got.Path != "/generateAssistantResponse" {
			t.Errorf("request %d path = %q", i+1, got.Path)
		}
		if got.Target != wantTargets[i] {
			t.Errorf("request %d target = %q, want %q", i+1, got.Target, wantTargets[i])
		}
		if got.Authorization != "Bearer route-key" || got.TokenType != "API_KEY" {
			t.Errorf("request %d did not reuse the API key headers", i+1)
		}
		if got.ContentType != "application/x-amz-json-1.0" || got.AgentMode != "vibe" || got.OptOut != "false" {
			t.Errorf("request %d protocol headers are incomplete: content-type=%q agent=%q optout=%q", i+1, got.ContentType, got.AgentMode, got.OptOut)
		}
		if got.SDKRequest != fmt.Sprintf("attempt=%d; max=4", i+1) {
			t.Errorf("request %d SDK attempt header = %q", i+1, got.SDKRequest)
		}
		if got.InvocationID != invocationID {
			t.Errorf("request %d invocation id changed: %q vs %q", i+1, got.InvocationID, invocationID)
		}
		if got.Connection != "" || got.Close {
			t.Errorf("request %d disabled connection reuse: Connection=%q Close=%t", i+1, got.Connection, got.Close)
		}
		if !bytes.Equal(got.Body, requests[0].Body) {
			t.Errorf("request %d body differs from request 1", i+1)
		}
	}

	var bodyPayload KiroPayload
	if err := json.Unmarshal(requests[0].Body, &bodyPayload); err != nil {
		t.Fatalf("decode captured body: %v", err)
	}
	if got := bodyPayload.ConversationState.CurrentMessage.UserInputMessage.Origin; got != "AI_EDITOR" {
		t.Fatalf("shared body origin = %q, want AI_EDITOR", got)
	}
	if got := bodyPayload.ConversationState.AgentTaskType; got != "vibe" {
		t.Fatalf("shared body agent task type = %q, want vibe", got)
	}
	if got := bodyPayload.ConversationState.AgentContinuationId; strings.TrimSpace(got) == "" {
		t.Fatal("shared body must include an agent continuation id")
	}
	if got := bodyPayload.ConversationState.ChatTriggerType; got != "MANUAL" {
		t.Fatalf("shared body chat trigger type = %q, want MANUAL", got)
	}
	if got := bodyPayload.ConversationState.ConversationID; strings.TrimSpace(got) == "" {
		t.Fatal("shared body must include a conversation id")
	}
}

func TestAPIKeyFourEndpointRoutePreservesExistingIDEEnvelope(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	account := apiKeyRouterTestAccount(t, "us-east-1")
	payload := newKiroRetryTestPayload()
	payload.ConversationState.AgentTaskType = "custom-task"
	payload.ConversationState.AgentContinuationId = "existing-continuation"
	payload.ConversationState.ChatTriggerType = "AUTOMATIC"
	payload.ConversationState.ConversationID = "existing-conversation"

	var captured KiroPayload
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		stream := bytes.Join([][]byte{
			awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}),
			awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}),
		}, nil)
		return apiKeyRouterResponse(http.StatusOK, bytes.NewReader(stream)), nil
	}))

	if err := CallKiroAPI(account, payload, &KiroStreamCallback{}); err != nil {
		t.Fatalf("four-endpoint call failed: %v", err)
	}
	if captured.ConversationState.AgentTaskType != "custom-task" ||
		captured.ConversationState.AgentContinuationId != "existing-continuation" ||
		captured.ConversationState.ChatTriggerType != "AUTOMATIC" ||
		captured.ConversationState.ConversationID != "existing-conversation" {
		t.Fatalf("existing IDE envelope was overwritten: %+v", captured.ConversationState)
	}
}

func TestAPIKeyFourEndpointRouteStopsAtFirst2xxRegardlessOfContentType(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	account := apiKeyRouterTestAccount(t, "us-east-1")
	var calls int
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		resp := apiKeyRouterResponse(http.StatusCreated, bytes.NewReader(awsEventStreamFrame(t,
			"assistantResponseEvent", map[string]interface{}{"content": "accepted"})))
		resp.Header.Set("Content-Type", "application/json")
		return resp, nil
	}))

	var text string
	var stopReason string
	err := CallKiroAPI(account, newKiroRetryTestPayload(), &KiroStreamCallback{
		OnText:       func(chunk string, _ bool) { text += chunk },
		OnStopReason: func(reason string) { stopReason = reason },
	})
	if err != nil {
		t.Fatalf("2xx event stream failed: %v", err)
	}
	if calls != 1 || text != "accepted" || stopReason != "end_turn" {
		t.Fatalf("calls=%d text=%q stop=%q, want one accepted completed response", calls, text, stopReason)
	}
}

func TestAPIKeyFourEndpointRoutePreservesUpstreamStopReason(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	account := apiKeyRouterTestAccount(t, "us-east-1")
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
		stream := bytes.Join([][]byte{
			awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "accepted"}),
			awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "max_tokens"}),
		}, nil)
		return apiKeyRouterResponse(http.StatusOK, bytes.NewReader(stream)), nil
	}))

	var reasons []string
	err := CallKiroAPI(account, newKiroRetryTestPayload(), &KiroStreamCallback{
		OnStopReason: func(reason string) { reasons = append(reasons, reason) },
	})
	if err != nil {
		t.Fatalf("2xx event stream failed: %v", err)
	}
	if !reflect.DeepEqual(reasons, []string{"max_tokens"}) {
		t.Fatalf("stop reasons = %v, want upstream max_tokens exactly once", reasons)
	}
}

func TestAPIKeyFourEndpointRouteDoesNotReplayAcceptedBrokenStream(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	account := apiKeyRouterTestAccount(t, "us-east-1")
	var calls int
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return apiKeyRouterResponse(http.StatusOK, bytes.NewReader(nil)), nil
	}))

	err := CallKiroAPI(account, newKiroRetryTestPayload(), &KiroStreamCallback{})
	if !errors.Is(err, errEmptyKiroStream) {
		t.Fatalf("error = %v, want empty-stream error", err)
	}
	if calls != 1 {
		t.Fatalf("accepted request was replayed %d times", calls)
	}
}

func TestAPIKeyFourEndpointRouteStopsOnPermanentHTTPStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusNotFound,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
			account := apiKeyRouterTestAccount(t, "us-east-1")
			var calls int
			installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return apiKeyRouterResponse(status, strings.NewReader(`{"message":"do not retry"}`)), nil
			}))

			err := CallKiroAPI(account, newKiroRetryTestPayload(), &KiroStreamCallback{})
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
				t.Fatalf("error = %v, want HTTP %d", err, status)
			}
			if calls != 1 {
				t.Fatalf("permanent HTTP %d made %d requests", status, calls)
			}
		})
	}
}

func TestAPIKeyFourEndpointRouteClassifiesImproperlyFormedRequest(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	account := apiKeyRouterTestAccount(t, "us-east-1")
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return apiKeyRouterResponse(http.StatusBadRequest, strings.NewReader("Improperly formed request.")), nil
	}))

	err := CallKiroAPI(account, newKiroRetryTestPayload(), &KiroStreamCallback{})
	if !isUpstreamPermanentError(err) {
		t.Fatalf("error = %v, want permanent upstream rejection", err)
	}
}

func TestAPIKeyFourEndpointRouteUsesStatusTextForEmptyErrorBody(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	account := apiKeyRouterTestAccount(t, "us-east-1")
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return apiKeyRouterResponse(http.StatusUnauthorized, strings.NewReader("")), nil
	}))

	err := CallKiroAPI(account, newKiroRetryTestPayload(), &KiroStreamCallback{})
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("error = %v, want readable status text", err)
	}
}

type apiKeyRouterErrorReader struct{ err error }

func (r apiKeyRouterErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestAPIKeyFourEndpointRoutePreservesErrorBodyReadFailure(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	account := apiKeyRouterTestAccount(t, "us-east-1")
	var calls int
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return apiKeyRouterResponse(http.StatusInternalServerError, apiKeyRouterErrorReader{err: io.ErrUnexpectedEOF}), nil
	}))

	err := CallKiroAPI(account, newKiroRetryTestPayload(), &KiroStreamCallback{})
	if err == nil || !strings.Contains(err.Error(), "read error body") || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want wrapped body read failure", err)
	}
	if calls != apiKeyFourEndpointCount {
		t.Fatalf("calls = %d, want all endpoints", calls)
	}
}

func TestAPIKeyFourEndpointRouteFallsBackOnTransportError(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	account := apiKeyRouterTestAccount(t, "us-east-1")
	var calls int
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("temporary dial failure")
		}
		return apiKeyRouterResponse(http.StatusOK, bytes.NewReader(awsEventStreamFrame(t,
			"assistantResponseEvent", map[string]interface{}{"content": "recovered"}))), nil
	}))

	var text string
	err := CallKiroAPI(account, newKiroRetryTestPayload(), &KiroStreamCallback{
		OnText: func(chunk string, _ bool) { text += chunk },
	})
	if err != nil || calls != 2 || text != "recovered" {
		t.Fatalf("err=%v calls=%d text=%q", err, calls, text)
	}
}

func TestAPIKeyFourEndpointRouteReturnsCancellationFromTransport(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	account := apiKeyRouterTestAccount(t, "us-east-1")
	ctx, cancel := context.WithCancel(context.Background())
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, errors.New("transport stopped")
	}))

	err := CallKiroAPIContext(ctx, account, newKiroRetryTestPayload(), &KiroStreamCallback{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
}

func TestAPIKeyFourEndpointRouteReturnsOnlyAfterAllRetryableStatusesFail(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	account := apiKeyRouterTestAccount(t, "us-east-1")
	statuses := []int{
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	}
	var calls int
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
		status := statuses[calls]
		calls++
		return apiKeyRouterResponse(status, strings.NewReader(fmt.Sprintf(`{"status":%d}`, status))), nil
	}))

	err := CallKiroAPI(account, newKiroRetryTestPayload(), &KiroStreamCallback{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503 from API Key AmazonQ") {
		t.Fatalf("final error = %v, want final endpoint status", err)
	}
	if calls != apiKeyFourEndpointCount {
		t.Fatalf("calls = %d, want all %d endpoints", calls, apiKeyFourEndpointCount)
	}
}

func TestAPIKeyFourEndpointRouteExhaustsEndpointsBeforeChangingAccount(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	t.Setenv(apiKeyRouteAllowlistEnv, "")
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("initialize config: %v", err)
	}

	proxyKey := "test://api-key-router/account-failover"
	for _, account := range []config.Account{
		{ID: "route-first", Enabled: true, AuthMethod: "api_key", KiroApiKey: "key-first", Region: "us-east-1", ProxyURL: proxyKey},
		{ID: "route-second", Enabled: true, AuthMethod: "api_key", KiroApiKey: "key-second", Region: "us-east-1", ProxyURL: proxyKey},
	} {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("add account %s: %v", account.ID, err)
		}
	}

	var tokens []string
	installAPIKeyRouterTestClient(t, proxyKey, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		tokens = append(tokens, token)
		if len(tokens) <= apiKeyFourEndpointCount {
			return apiKeyRouterResponse(http.StatusServiceUnavailable, strings.NewReader("temporary failure")), nil
		}
		stream := bytes.Join([][]byte{
			awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "second account succeeded"}),
			awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}),
		}, nil)
		return apiKeyRouterResponse(http.StatusOK, bytes.NewReader(stream)), nil
	}))

	p := accountpool.GetPool()
	p.ResetTransientState()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	payload := newKiroRetryTestPayload()
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.5"
	recorder := httptest.NewRecorder()
	h.handleClaudeNonStream(context.Background(), recorder, payload, "claude-sonnet-4.5", false, claudeThinkingResponseOptions{}, 1, nil, "")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(tokens) != apiKeyFourEndpointCount+1 {
		t.Fatalf("upstream calls = %d, want four failures then one success: %v", len(tokens), tokens)
	}
	for i := 1; i < apiKeyFourEndpointCount; i++ {
		if tokens[i] != tokens[0] {
			t.Fatalf("account changed before four endpoints were exhausted: %v", tokens)
		}
	}
	if tokens[apiKeyFourEndpointCount] == tokens[0] {
		t.Fatalf("account did not change after all endpoints failed: %v", tokens)
	}
}

func TestAPIKeyFourEndpointModeDoesNotAffectOAuthRouting(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	t.Setenv(apiKeyRouteAllowlistEnv, "")

	account := &config.Account{
		ID:          "oauth-account",
		AccessToken: "oauth-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
		ProxyURL:    "test://api-key-router/oauth",
	}
	oldResolver := resolveKiroEndpoints
	resolveKiroEndpoints = func(*config.Account) []kiroEndpoint {
		return []kiroEndpoint{{
			URL:    "https://oauth.example/generateAssistantResponse",
			Origin: "AI_EDITOR",
			Name:   "OAuth test",
		}}
	}
	t.Cleanup(func() { resolveKiroEndpoints = oldResolver })

	var host, authorization, tokenType, contentType string
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		host = req.URL.Host
		authorization = req.Header.Get("Authorization")
		tokenType = req.Header.Get("tokentype")
		contentType = req.Header.Get("Content-Type")
		return apiKeyRouterResponse(http.StatusOK, bytes.NewReader(awsEventStreamFrame(t,
			"assistantResponseEvent", map[string]interface{}{"content": "oauth"}))), nil
	}))

	if err := CallKiroAPI(account, newKiroRetryTestPayload(), &KiroStreamCallback{}); err != nil {
		t.Fatalf("OAuth call failed: %v", err)
	}
	if host != "oauth.example" || authorization != "Bearer oauth-token" || tokenType != "" || contentType != "application/json" {
		t.Fatalf("OAuth route changed: host=%q authorization=%q tokenType=%q contentType=%q", host, authorization, tokenType, contentType)
	}
}

func TestShouldFallbackAPIKeyEndpointIsNarrow(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 599} {
		if !shouldFallbackAPIKeyEndpoint(status) {
			t.Errorf("HTTP %d should fall back", status)
		}
	}
	for _, status := range []int{200, 201, 400, 401, 402, 403, 404, 409, 499, 600} {
		if shouldFallbackAPIKeyEndpoint(status) {
			t.Errorf("HTTP %d should not fall back", status)
		}
	}
}

func TestAPIKeyFourEndpointRouteHonorsClientCancellation(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	account := apiKeyRouterTestAccount(t, "us-east-1")
	var calls int
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not be called")
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := CallKiroAPIContext(ctx, account, newKiroRetryTestPayload(), &KiroStreamCallback{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if calls != 0 {
		t.Fatalf("canceled request reached upstream %d times", calls)
	}
}

func TestAPIKeyLegacyRouteRemainsDefault(t *testing.T) {
	t.Setenv(apiKeyRouteModeEnv, "")
	t.Setenv(apiKeyRouteAllowlistEnv, "")
	account := apiKeyRouterTestAccount(t, "eu-central-1")
	var host, path, target, origin string
	installAPIKeyRouterTestClient(t, account.ProxyURL, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		host = req.URL.Host
		path = req.URL.Path
		target = req.Header.Get("X-Amz-Target")
		body, _ := io.ReadAll(req.Body)
		var payload KiroPayload
		_ = json.Unmarshal(body, &payload)
		origin = payload.ConversationState.CurrentMessage.UserInputMessage.Origin
		return apiKeyRouterResponse(http.StatusOK, bytes.NewReader(awsEventStreamFrame(t,
			"assistantResponseEvent", map[string]interface{}{"content": "legacy"}))), nil
	}))

	if err := CallKiroAPI(account, newKiroRetryTestPayload(), &KiroStreamCallback{}); err != nil {
		t.Fatalf("legacy call failed: %v", err)
	}
	if host != "runtime.eu-central-1.kiro.dev" || path != "/" {
		t.Fatalf("legacy URL changed: host=%q path=%q", host, path)
	}
	if target != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" || origin != "KIRO_CLI" {
		t.Fatalf("legacy protocol changed: target=%q origin=%q", target, origin)
	}
}

func apiKeyRouterTestAccount(t *testing.T, region string) *config.Account {
	t.Helper()
	proxyKey := "test://api-key-router/" + strings.ReplaceAll(t.Name(), " ", "-")
	return &config.Account{
		ID:         "route-account",
		AuthMethod: "api_key",
		KiroApiKey: "route-key",
		ApiRegion:  region,
		ProxyURL:   proxyKey,
	}
}

func installAPIKeyRouterTestClient(t *testing.T, proxyKey string, transport http.RoundTripper) {
	t.Helper()
	old, existed := proxyClientCache.Load(proxyKey)
	proxyClientCache.Store(proxyKey, &http.Client{Transport: transport})
	t.Cleanup(func() {
		if existed {
			proxyClientCache.Store(proxyKey, old)
		} else {
			proxyClientCache.Delete(proxyKey)
		}
	})
}

func apiKeyRouterResponse(status int, body io.Reader) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(body),
		Header:     make(http.Header),
	}
}
