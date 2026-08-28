//go:build live

package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-go/config"

	"github.com/google/uuid"
)

// TestLiveAPIKeyFourEndpointRoute is an opt-in smoke test for release
// verification. It never reads repository configuration and never logs the
// credential; the caller must inject KIRO_LIVE_API_KEY into the test process.
func TestLiveAPIKeyFourEndpointRoute(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("KIRO_LIVE_API_KEY"))
	if key == "" {
		t.Skip("KIRO_LIVE_API_KEY is not set")
	}
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("initialize isolated live-test config: %v", err)
	}

	t.Setenv(apiKeyRouteModeEnv, apiKeyRouteModeFour)
	t.Setenv(apiKeyRouteAllowlistEnv, "")
	account := &config.Account{
		ID:         strings.TrimSpace(os.Getenv("KIRO_LIVE_ACCOUNT_ID")),
		AuthMethod: "api_key",
		KiroApiKey: key,
		ApiRegion:  strings.TrimSpace(os.Getenv("KIRO_LIVE_REGION")),
		MachineId:  strings.TrimSpace(os.Getenv("KIRO_LIVE_MACHINE_ID")),
	}
	if account.ID == "" {
		account.ID = "live-api-key-route-smoke"
	}

	payload := &KiroPayload{}
	// Deliberately mirror the OpenAI translator: it supplies the conversation
	// fields but not the two IDE agent fields. The isolated four-endpoint route
	// must normalize that envelope before it reaches q.amazonaws.com.
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = uuid.New().String()
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "Reply with exactly OK."
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.5"
	payload.InferenceConfig = &InferenceConfig{MaxTokens: 16}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var text string
	var stopReason string
	err := CallKiroAPIContext(ctx, account, payload, &KiroStreamCallback{
		OnText: func(chunk string, thinking bool) {
			if !thinking {
				text += chunk
			}
		},
		OnStopReason: func(reason string) { stopReason = reason },
	})
	if err != nil {
		t.Fatalf("live four-endpoint route failed: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("live four-endpoint route returned no assistant content")
	}
	if strings.TrimSpace(stopReason) == "" {
		t.Fatal("live four-endpoint route returned no completion reason")
	}
	t.Logf("live route succeeded with %d assistant bytes and stop reason %s", len(text), stopReason)
}
