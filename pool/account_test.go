package pool

import (
	"errors"
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

func TestOverLimitAccountsAreSkippedByDefault(t *testing.T) {
	p := &AccountPool{}
	normal := config.Account{ID: "normal"}
	overLimit := config.Account{ID: "over", UsageCurrent: 10, UsageLimit: 10}

	p.accounts = []config.Account{normal, overLimit}

	for i := 0; i < 5; i++ {
		acc := p.GetNext()
		if acc == nil {
			t.Fatalf("expected an account")
		}
		if acc.ID == "over" {
			t.Fatalf("expected over-limit account to be skipped when upstream OverageStatus is empty")
		}
	}
}

func TestOverLimitAccountsCanBeSelectedWhenUpstreamOverageEnabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "ENABLED",
	}

	p.accounts = []config.Account{overLimit}

	acc := p.GetNext()
	if acc == nil {
		t.Fatalf("expected upstream-enabled overage account to be selectable")
	}
	if acc.ID != "over" {
		t.Fatalf("expected overage account, got %q", acc.ID)
	}
}

func TestOverLimitAccountsRemainSkippedWhenUpstreamOverageDisabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "DISABLED",
	}

	p.accounts = []config.Account{overLimit}

	if acc := p.GetNext(); acc != nil {
		t.Fatalf("expected nil when upstream OverageStatus=DISABLED, got %q", acc.ID)
	}
}

func TestGetNextKeepsFiveMinuteTokenAvailable(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:          "acct-1",
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 300,
	}

	p.accounts = []config.Account{account}

	got := p.GetNext()
	if got == nil {
		t.Fatalf("expected five-minute token to be available")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

// ---------------------------------------------------------------------------
// IsAuthFailure
// ---------------------------------------------------------------------------

func TestIsAuthFailureRecognizes401And403(t *testing.T) {
	positives := []string{
		"HTTP 401 from server",
		"received 403 Forbidden",
		"bad credentials",
		"invalid_grant",
		"invalid_token",
		"token expired",
		"token has expired",
		"unauthorized",
	}
	for _, msg := range positives {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = false, want true", msg)
		}
	}
}

func TestIsAuthFailureIgnoresFalsePositives(t *testing.T) {
	// hasStatusToken only excludes digit boundaries; e.g. "4011" contains "401"
	// but the trailing '1' is a digit so it does NOT match.
	negatives := []string{
		"status code 4011 found", // digit immediately after 401 → not a standalone token
		"error 14013 exceeded",   // digit before and after 401
		"some random error",
		"status 200 OK",
	}
	for _, msg := range negatives {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = true, want false", msg)
		}
	}
}

func TestIsAuthFailureNilError(t *testing.T) {
	if IsAuthFailure(nil) {
		t.Fatal("IsAuthFailure(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// IsSuspensionError
// ---------------------------------------------------------------------------

func TestIsSuspensionErrorDetectsKnownMessages(t *testing.T) {
	positives := []string{
		"account temporarily_suspended",
		"account temporarily suspended",
		"no available kiro profile",
		"No Available Kiro Profile", // case-insensitive
	}
	for _, msg := range positives {
		if !IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = false, want true", msg)
		}
	}
}

func TestIsSuspensionErrorIgnoresUnrelatedErrors(t *testing.T) {
	negatives := []string{
		"some other error",
		"unauthorized",
		"429 too many requests",
	}
	for _, msg := range negatives {
		if IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = true, want false", msg)
		}
	}
}

func TestIsSuspensionErrorNilError(t *testing.T) {
	if IsSuspensionError(nil) {
		t.Fatal("IsSuspensionError(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// GetNextForModelExcluding
// ---------------------------------------------------------------------------

func newTestPool(accounts ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.accounts = accounts
	return p
}

func TestGetNextForModelExcludingSkipsExcludedAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	excluded := map[string]bool{"a": true}
	for i := 0; i < 5; i++ {
		acc := p.GetNextForModelExcluding("model", excluded)
		if acc == nil {
			t.Fatal("expected account b, got nil")
		}
		if acc.ID == "a" {
			t.Fatalf("excluded account a was returned on iteration %d", i)
		}
	}
}

func TestGetNextForModelExcludingReturnsNilWhenAllExcluded(t *testing.T) {
	p := newTestPool(config.Account{ID: "only"})
	acc := p.GetNextForModelExcluding("model", map[string]bool{"only": true})
	if acc != nil {
		t.Fatalf("expected nil when only account is excluded, got %q", acc.ID)
	}
}

func TestGetNextForModelExcludingReturnsNilOnEmptyPool(t *testing.T) {
	p := newTestPool()
	acc := p.GetNextForModelExcluding("model", map[string]bool{})
	if acc != nil {
		t.Fatalf("expected nil for empty pool, got %q", acc.ID)
	}
}

// ---------------------------------------------------------------------------
// DisableAccount
// ---------------------------------------------------------------------------

func TestDisableAccountSetsCooldown(t *testing.T) {
	// Initialize a temporary config so SetAccountBanStatus can persist safely.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := newTestPool()
	p.DisableAccount("test-id", "test reason")

	p.mu.RLock()
	cooldown, ok := p.cooldowns["test-id"]
	p.mu.RUnlock()

	if !ok {
		t.Fatal("expected cooldown to be set after DisableAccount")
	}
	// Safety-net cooldown must be at least 23 hours from now.
	minExpected := time.Now().Add(23 * time.Hour)
	if cooldown.Before(minExpected) {
		t.Fatalf("expected cooldown >= 23h in future, got %v", cooldown)
	}
}

func TestGetNextExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}

	acc := p.GetNextExcluding(map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

func TestGetNextForModelExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}
	p.SetModelList("a", []string{"claude-sonnet-4.5"})
	p.SetModelList("b", []string{"claude-sonnet-4.5"})

	acc := p.GetNextForModelExcluding("claude-sonnet-4.5", map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

// ---------------------------------------------------------------------------
// Reload over-usage filtering
// ---------------------------------------------------------------------------

func TestReloadKeepsOverQuotaAccountWhenAllowOverUsage(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got == nil || got.ID != "over" {
		t.Fatalf("expected over-quota account to remain routable when allowOverUsage=true, got %#v", got)
	}
}

func TestReloadDropsOverQuotaAccountWhenAllowOverUsageDisabled(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got != nil {
		t.Fatalf("expected over-quota account to be dropped, got %q", got.ID)
	}
}

func TestAcquireAndReleaseLimits(t *testing.T) {
	p := &AccountPool{
		activeSSE:     make(map[string]int),
		reqTimestamps: make(map[string][]time.Time),
	}
	// Setup standard account in the pool
	p.accounts = []config.Account{
		{ID: "standard-acc", Enabled: true, Provider: "Social"},
	}

	accountID := "standard-acc"

	// 1. Concurrency limit test (stream = true)
	// We can acquire up to 3 SSE streams (default standard limit)
	for i := 0; i < 3; i++ {
		if !p.Acquire(accountID, true) {
			t.Fatalf("expected to acquire stream slot %d", i+1)
		}
	}
	// The 4th stream should fail
	if p.Acquire(accountID, true) {
		t.Fatal("expected 4th stream acquire to fail")
	}

	// Release one stream slot
	p.Release(accountID, true)
	// Should be able to acquire again
	if !p.Acquire(accountID, true) {
		t.Fatal("expected to acquire stream slot after release")
	}
	// And fail again on subsequent try
	if p.Acquire(accountID, true) {
		t.Fatal("expected stream slot to be full again")
	}

	// Reset concurrency for RPM testing
	p.Release(accountID, true)
	p.Release(accountID, true)
	p.Release(accountID, true)

	// 2. RPM limit test (stream = false)
	p.reqTimestamps[accountID] = nil

	// We can acquire up to 10 requests (non-stream, default standard limit)
	for i := 0; i < 10; i++ {
		if !p.Acquire(accountID, false) {
			t.Fatalf("expected to acquire request %d", i+1)
		}
	}
	// The 11th request should fail
	if p.Acquire(accountID, false) {
		t.Fatal("expected 11th request acquire to fail")
	}

	// Test RPM sliding window expiration:
	p.mu.Lock()
	oldTime := time.Now().Add(-2 * time.Minute)
	for i := range p.reqTimestamps[accountID] {
		p.reqTimestamps[accountID][i] = oldTime
	}
	p.mu.Unlock()

	// Now we should be able to acquire again because old timestamps will be cleaned up
	if !p.Acquire(accountID, false) {
		t.Fatal("expected to acquire request after timestamp expiration")
	}
}

func TestEnterpriseAccountLimits(t *testing.T) {
	p := &AccountPool{
		activeSSE:     make(map[string]int),
		reqTimestamps: make(map[string][]time.Time),
	}
	// Setup Enterprise credentials in the pool
	p.accounts = []config.Account{
		// Raise RPM only while exercising the independent SSE concurrency limit;
		// otherwise the default 15 RPM guard masks the expected 30-stream cap.
		{ID: "ent-acc", Enabled: true, Provider: "Enterprise", MaxRPM: 31},
	}

	accountID := "ent-acc"

	// 1. Concurrency limit test (stream = true)
	// We can acquire up to 30 SSE streams (default Enterprise limit)
	for i := 0; i < 30; i++ {
		if !p.Acquire(accountID, true) {
			t.Fatalf("expected to acquire stream slot %d", i+1)
		}
	}
	// The 31st stream should fail
	if p.Acquire(accountID, true) {
		t.Fatal("expected 31st stream acquire to fail")
	}

	// 2. RPM limit test (stream = false)
	// Enterprise accounts should have default RPM = 15
	p.accounts[0].MaxRPM = 0
	p.reqTimestamps[accountID] = nil
	for i := 0; i < 15; i++ {
		if !p.Acquire(accountID, false) {
			t.Fatalf("expected to acquire request %d on enterprise (default 15 RPM)", i+1)
		}
	}
	// The 16th request should fail
	if p.Acquire(accountID, false) {
		t.Fatal("expected 16th request acquire to fail on enterprise")
	}
}

func TestCustomOverriddenLimits(t *testing.T) {
	p := &AccountPool{
		activeSSE:     make(map[string]int),
		reqTimestamps: make(map[string][]time.Time),
	}
	// Setup custom overridden limits
	p.accounts = []config.Account{
		{ID: "custom-acc", Enabled: true, MaxSSE: 1, MaxRPM: 2},
	}

	accountID := "custom-acc"

	// MaxSSE override = 1
	if !p.Acquire(accountID, true) {
		t.Fatal("expected to acquire first stream slot")
	}
	// Second stream should fail
	if p.Acquire(accountID, true) {
		t.Fatal("expected second stream to fail")
	}

	p.Release(accountID, true)

	// MaxRPM override = 2
	p.reqTimestamps[accountID] = nil
	if !p.Acquire(accountID, false) {
		t.Fatal("expected to acquire first request")
	}
	if !p.Acquire(accountID, false) {
		t.Fatal("expected to acquire second request")
	}
	// Third request should fail
	if p.Acquire(accountID, false) {
		t.Fatal("expected third request to fail")
	}
}
