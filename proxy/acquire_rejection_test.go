package proxy

import (
	"fmt"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestHandlersReturnSchedulerSlotWhenAcquireRejects(t *testing.T) {
	tests := []struct {
		name string
		call func(*Handler, *httptest.ResponseRecorder)
	}{
		{
			name: "claude stream",
			call: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, &KiroPayload{}, "claude-sonnet-4.5", false, claudeThinkingResponseOptions{}, 1, nil, "")
			},
		},
		{
			name: "claude non-stream",
			call: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeNonStream(rec, &KiroPayload{}, "claude-sonnet-4.5", false, claudeThinkingResponseOptions{}, 1, nil, "")
			},
		},
		{
			name: "openai stream",
			call: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleOpenAIStream(rec, &KiroPayload{}, "claude-sonnet-4.5", false, 1, "")
			},
		},
		{
			name: "openai non-stream",
			call: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleOpenAINonStream(rec, &KiroPayload{}, "claude-sonnet-4.5", false, 1, "")
			},
		},
		{
			name: "responses stream",
			call: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleResponsesStream(
					rec,
					&KiroPayload{},
					"claude-sonnet-4.5",
					false,
					1,
					"",
					"resp-acquire-rejected",
					&ResponsesRequest{},
					nil,
					false,
				)
			},
		},
		{
			name: "responses non-stream",
			call: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleResponsesNonStream(
					rec,
					&KiroPayload{},
					"claude-sonnet-4.5",
					false,
					1,
					"",
					"resp-acquire-rejected",
					&ResponsesRequest{},
					nil,
					false,
				)
			},
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accountID := fmt.Sprintf("acquire-reject-%d", i)
			cfgFile := filepath.Join(t.TempDir(), "config.json")
			if err := config.Init(cfgFile); err != nil {
				t.Fatalf("config.Init: %v", err)
			}
			if err := config.AddAccount(config.Account{
				ID:          accountID,
				Enabled:     true,
				AccessToken: "token-" + accountID,
				ProfileArn:  "arn:aws:codewhisperer:profile/" + accountID,
				MaxRPM:      1,
			}); err != nil {
				t.Fatalf("config.AddAccount: %v", err)
			}

			p := accountpool.GetPool()
			p.ResetTransientState()
			p.Reload()

			// Fill the sole account's RPM slot without selecting it. The handler's
			// subsequent selection reserves smart-scheduler in-flight, then its
			// Acquire call must reject without consuming another RPM/SSE slot.
			if !p.Acquire(accountID, false) {
				t.Fatal("failed to fill account RPM slot")
			}

			h := &Handler{
				pool:        p,
				promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
			}
			test.call(h, httptest.NewRecorder())

			stats, ok := p.GetRuntimeStatsSnapshot()[accountID]
			if !ok {
				t.Fatalf("expected runtime stats for selected account %q", accountID)
			}
			if stats.InFlight != 0 {
				t.Fatalf("Acquire rejection leaked scheduler in-flight slot: %+v", stats)
			}
			if stats.SuccessCount != 0 || stats.FailureCount != 0 {
				t.Fatalf("local Acquire rejection must be health-neutral: %+v", stats)
			}
			if stats.SelectedCount != 1 {
				t.Fatalf("expected exactly one account selection, got %+v", stats)
			}
		})
	}
}
