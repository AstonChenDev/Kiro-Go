package proxy

import (
	"errors"
	"kiro-go/config"
	"kiro-go/logger"
	"math/rand"
	"strings"
	"time"
)

// absoluteMaxAccountRetryAttempts is a defensive ceiling on per-request account
// retries. The budget otherwise means "iterate every selectable account" (see
// resolveAccountRetryBudget): each retry excludes already-tried accounts and
// must pick a different one, so the real retry count converges to the number of
// accounts and never runs away. This only guards pathological cases.
const absoluteMaxAccountRetryAttempts = 64

// resolveAccountRetryBudget returns how many account attempts one request may
// make. Previously this was a fixed 3, so with >3 accounts a request only tried
// the first 3 and gave up while the rest sat idle — under a 429 storm that meant
// an outright failure. Now it scales with the account count so every account
// can be tried. totalAccounts<=0 falls back to 1 (try at least once).
func resolveAccountRetryBudget(totalAccounts int) int {
	if totalAccounts <= 0 {
		return 1
	}
	if totalAccounts > absoluteMaxAccountRetryAttempts {
		return absoluteMaxAccountRetryAttempts
	}
	return totalAccounts
}

// accountRetryBackoff returns the wait before the next retry attempt:
// exponential 200ms→2s cap + jitter, so a transient upstream blip isn't
// amplified by back-to-back retries against the next account. attempt is 0-based.
func accountRetryBackoff(attempt int) time.Duration {
	const baseMS = 200
	const maxMS = 2000
	if attempt < 0 {
		attempt = 0
	}
	shift := attempt
	if shift > 4 {
		shift = 4
	}
	backoff := baseMS << shift
	if backoff > maxMS {
		backoff = maxMS
	}
	jitter := 0
	if j := backoff / 4; j > 0 {
		jitter = rand.Intn(j)
	}
	return time.Duration(backoff+jitter) * time.Millisecond
}

// maxSameAccountStreamRetries bounds same-account recovery of a truncated
// stream. Kiro IDE caps its truncation retry at one (Dt3 = 1 in extension.js),
// but that budget was set for a single-credential client. This proxy also
// rotates accounts, and the two recover different failures: a same-account
// retry helps when the upstream hiccupped, rotation helps when that account's
// backend is unhealthy. Two is therefore not a copy of the IDE.
//
// Cost of the extra attempt is bounded and small. Truncation returns nil from
// CallKiroAPIContext, so it never reaches the endpoint fallback or
// maxStreamAttemptsPerEndpoint - those fire only on transport errors. Worst
// case across maxAccountRetryAttempts accounts is 3*(1+budget) requests: 9 at
// two retries versus 6 at one.
//
// The payoff lands mostly on the three fully buffered paths, whose canRetry is
// nil: a non-stream client gets a 500 with nothing usable, so one more chance
// is worth more there than on a stream that has already flushed partial text.
const maxSameAccountStreamRetries = 2

// errUpstreamTruncatedResponse is a soft failure raised when a transport-clean
// stream carried content but never a terminal signal. It is retryable on the
// same account and must not mark the account unhealthy.
//
// There is deliberately no empty-response error here. A stream that produced no
// output at all is already caught one layer down: parseEventStreamTracked
// returns errEmptyKiroStream when !sawOutput (proxy/kiro.go), and
// CallKiroAPIContext retries it internally. Since sawOutput is set by exactly
// the three signals classifyStreamIntegrity measures (content, reasoning,
// toolUse), an all-zero measurement can never reach this layer with a nil
// error.
var errUpstreamTruncatedResponse = errors.New("upstream truncated response without stop reason")

// classifyStreamIntegrity decides whether an upstream stream that returned no
// transport error is actually complete. parseEventStream reports success on a
// clean EOF, so a stream that died mid-answer is otherwise indistinguishable
// from a finished one.
//
// Complete when a stopReason arrived, or when a tool call was delivered. Both
// match Kiro IDE, whose empty and truncation predicates each require
// toolCallCount === 0.
//
// Truncated when content arrived without any terminal signal.
//
// Reasoning-only with no answer is STRICTER THAN THE IDE, deliberately. The
// IDE's truncation predicate ends in (contentChars > 0 || !reasoningSeen), so
// reasoning with no answer and no stopReason is treated as complete there and
// is never retried. That is the exact shape of the production symptom this
// proxy exists to fix: thinking streams in full, then the turn dies before the
// answer or the tool call. Handing a client reasoning with no answer as a
// successful turn is what made the failure invisible, so it is classified as
// truncated here.
func classifyStreamIntegrity(contentChars, toolCallCount int, stopReason string, sawReasoning bool) error {
	if strings.TrimSpace(stopReason) != "" {
		return nil
	}
	if toolCallCount > 0 {
		return nil
	}
	if contentChars > 0 || sawReasoning {
		return errUpstreamTruncatedResponse
	}
	// No content, no reasoning, no tools: unreachable through the wired paths
	// (errEmptyKiroStream fires first, see above). Treated as truncated rather
	// than complete so a future caller that bypasses that guard still cannot
	// ship an empty turn as a success.
	return errUpstreamTruncatedResponse
}

// isStreamIntegrityError reports whether err is a soft integrity failure.
// Callers may rotate accounts on these, but must not run them through
// handleAccountFailure: an upstream blip should not mark an account unhealthy.
func isStreamIntegrityError(err error) bool {
	return errors.Is(err, errUpstreamTruncatedResponse)
}

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota")
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
}

func isMonthlyLimitErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return (strings.Contains(msg, "402") && strings.Contains(msg, "monthly_request_count")) ||
		(strings.Contains(msg, "reached the limit") && strings.Contains(msg, "monthly_request_count"))
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	if !account.Enabled && account.BanStatus == banStatus && account.BanReason == banReason {
		return
	}

	if err := config.SetAccountBanStatus(account.ID, banStatus, banReason); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
	if h.suppliers != nil && account.SupplierBatchID != "" {
		h.suppliers.reconcileBatches()
	}
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isMonthlyLimitErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "Monthly request limit reached")
	case isUpstreamPermanentError(err):
		// The request itself is malformed/rejected by the upstream regardless of
		// which account relays it. Penalising the account's health, cooling it
		// down, or rotating to another account would be wrong (the account is
		// healthy) and harmful (scatters cache affinity, wastes upstream hits).
		// Neutral: release the in-flight slot the selection took (RecordPermanentRejection)
		// without recording any success or failure, then return.
		h.pool.RecordPermanentRejection(account.ID)
		return
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case isQuotaErrorMessage(errMsg):
		h.pool.RecordError(account.ID, true)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case isAuthErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}
