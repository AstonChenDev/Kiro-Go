package proxy

import (
	"context"
	"kiro-go/config"
	"kiro-go/logger"
	"strings"
)

// oauthStreamCompletion records the signals needed to recognize the current
// OAuth/social upstream's valid completion shape. Those streams may omit
// metadataEvent.stopReason, but a complete response ends with validated
// context-usage and/or metering metadata after the final generated frame.
//
// terminalAfterLastOutput is reset whenever later output arrives. This matters:
// a stale metadata frame followed by more content must not bless a subsequent
// clean EOF as complete.
type oauthStreamCompletion struct {
	sawAnswer               bool
	sawReasoning            bool
	sawToolUse              bool
	sawStopReason           bool
	terminalAfterLastOutput bool
}

// trackOAuthStreamCompletion installs completion tracking without mutating the
// caller's callback. On a fully parsed EOF it synthesizes a terminal reason only
// when the stream has evidence that the upstream deliberately finished:
//   - delivered tool input is complete on its own and maps to tool_use;
//   - normal answer text requires a trailing metadata/metering/context frame.
//
// Reasoning-only output intentionally remains incomplete, preserving the
// stricter guard that prevents a turn containing thinking but no answer from
// being reported as successful.
func trackOAuthStreamCompletion(callback *KiroStreamCallback) (*KiroStreamCallback, *oauthStreamCompletion) {
	state := &oauthStreamCompletion{}
	tracked := &KiroStreamCallback{}
	if callback != nil {
		*tracked = *callback
	}

	originalOnText := tracked.OnText
	tracked.OnText = func(text string, thinking bool) {
		if text != "" {
			if thinking {
				state.sawReasoning = true
			} else {
				state.sawAnswer = true
			}
			state.terminalAfterLastOutput = false
		}
		if originalOnText != nil {
			originalOnText(text, thinking)
		}
	}

	originalOnToolUse := tracked.OnToolUse
	tracked.OnToolUse = func(toolUse KiroToolUse) {
		state.sawToolUse = true
		state.terminalAfterLastOutput = false
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

	originalOnTerminalEvent := tracked.onTerminalEvent
	tracked.onTerminalEvent = func() {
		if state.sawAnswer || state.sawReasoning || state.sawToolUse {
			state.terminalAfterLastOutput = true
		}
		if originalOnTerminalEvent != nil {
			originalOnTerminalEvent()
		}
	}

	originalOnCleanEOF := tracked.onCleanEOF
	tracked.onCleanEOF = func() {
		completeOAuthStream(tracked, state)
		if originalOnCleanEOF != nil {
			originalOnCleanEOF()
		}
	}

	return tracked, state
}

func completeOAuthStream(callback *KiroStreamCallback, state *oauthStreamCompletion) {
	if callback == nil || state == nil || state.sawStopReason || callback.OnStopReason == nil {
		return
	}

	reason := ""
	switch {
	case state.sawToolUse:
		reason = "tool_use"
	case state.sawAnswer && state.terminalAfterLastOutput:
		reason = "end_turn"
	default:
		return
	}

	callback.OnStopReason(reason)
	logger.Debugf("[StreamIntegrity] Synthesized missing OAuth stop reason %s after terminal metadata", reason)
}

// runKiroWithIntegrityRetry calls Kiro and recovers a truncated upstream stream
// the way Kiro IDE does: retry the same request on the same account within a
// bounded budget before surfacing the failure.
//
// callback is reused across attempts. Both CallKiroAPIContext and
// parseEventStreamTracked copy the struct before wrapping any field, so a retry
// cannot double-wrap it; per-attempt state is cleared by reset instead.
// measure reports the integrity inputs after a transport-successful call.
// reset clears per-attempt state before a same-account retry; may be nil.
// canRetry reports whether a retry is still safe (for streaming: nothing has
// been flushed to the client yet). nil means always retryable.
//
// Return contract:
//   - nil: complete success only
//   - transport error from CallKiroAPIContext: caller should rotate/ban as usual
//   - integrity error while still retryable: retries exhausted; caller should
//     rotate account without treating it as an auth/quota failure
//   - integrity error after client flush: caller must surface failure to the
//     client (do not fake end_turn / normal completion). Retry is unsafe.
func runKiroWithIntegrityRetry(
	ctx context.Context,
	account *config.Account,
	payload *KiroPayload,
	callback *KiroStreamCallback,
	measure func() (contentChars, toolCount int, stopReason string, sawReasoning bool),
	reset func(),
	canRetry func() bool,
) error {
	label := accountEmailForLog(account)
	retryable := func() bool {
		if canRetry == nil {
			return true
		}
		return canRetry()
	}

	for attempt := 0; attempt <= maxSameAccountStreamRetries; attempt++ {
		if attempt > 0 && reset != nil {
			reset()
		}

		err := CallKiroAPIContext(ctx, account, payload, callback)
		if err != nil {
			return err
		}

		contentChars, toolCount, stopReason, sawReasoning := measure()
		integrityErr := classifyStreamIntegrity(contentChars, toolCount, stopReason, sawReasoning)
		if integrityErr == nil {
			return nil
		}

		// A canceled client is not an integrity failure: the turn is over and
		// reissuing it would only burn upstream quota.
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}

		if retryable() && attempt < maxSameAccountStreamRetries {
			logger.Warnf("[StreamIntegrity] %v on %s; retrying same account (%d/%d)",
				integrityErr, label, attempt+1, maxSameAccountStreamRetries)
			continue
		}

		if !retryable() {
			// Bytes already reached the client; reissuing would duplicate output.
			// Return the integrity error so callers emit an error event instead of
			// finishing with a forged end_turn/tool_use success.
			logger.Warnf("[StreamIntegrity] %v after client flush; signaling error (no retry)", integrityErr)
			return integrityErr
		}

		logger.Warnf("[StreamIntegrity] giving up after retries: %v", integrityErr)
		return integrityErr
	}

	// Unreachable: every branch inside the loop returns or continues, and the
	// final iteration cannot continue.
	return errUpstreamTruncatedResponse
}
