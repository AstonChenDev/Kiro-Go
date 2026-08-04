package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSupplierAPI struct {
	mu              sync.Mutex
	stock           supplierStock
	stockErr        error
	profile         supplierProfile
	keys            supplierKeysPage
	keysErr         error
	purchase        supplierPurchaseResponse
	purchaseErr     error
	purchaseCalls   int
	purchaseIDs     []string
	purchaseRegions []string
	stockCallCh     chan struct{}
}

func (f *fakeSupplierAPI) GetStock() (supplierStock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stockCallCh != nil {
		select {
		case f.stockCallCh <- struct{}{}:
		default:
		}
	}
	return f.stock, f.stockErr
}

func (f *fakeSupplierAPI) GetProfile() (supplierProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.profile, nil
}

func (f *fakeSupplierAPI) GetKeys(bool, int, int) (supplierKeysPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys, f.keysErr
}

func (f *fakeSupplierAPI) Purchase(_ int, region, clientOrderID string) (supplierPurchaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purchaseCalls++
	f.purchaseIDs = append(f.purchaseIDs, clientOrderID)
	f.purchaseRegions = append(f.purchaseRegions, region)
	return f.purchase, f.purchaseErr
}

func newSupplierTestManager(t *testing.T, fake *fakeSupplierAPI) (*Handler, *supplierManager, config.SupplierProvider) {
	t.Helper()
	dir := t.TempDir()
	if err := config.Init(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	provider, err := config.AddSupplierProvider(config.SupplierProvider{
		ID:                "vendor-a",
		Name:              "Vendor A",
		BaseURL:           "https://vendor.example",
		APIToken:          "km_supplier_secret",
		Enabled:           true,
		Priority:          1,
		AutoPurchaseCount: 2,
	})
	if err != nil {
		t.Fatalf("AddSupplierProvider: %v", err)
	}
	if err := config.UpdateSupplierFeature(true, true); err != nil {
		t.Fatalf("UpdateSupplierFeature: %v", err)
	}
	p := pool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	store, err := newSupplierStateStore(dir)
	if err != nil {
		t.Fatalf("newSupplierStateStore: %v", err)
	}
	m := &supplierManager{
		handler: h,
		store:   store,
		apiFactory: func(config.SupplierProvider) supplierAPI {
			return fake
		},
		wake: make(chan supplierWake, 1),
	}
	h.suppliers = m
	return h, m, provider
}

func TestSupplierPurchaseImportsAccountsWithRequiredDefaults(t *testing.T) {
	fake := &fakeSupplierAPI{purchase: supplierPurchaseResponse{
		Purchased:  2,
		Requested:  2,
		OrderID:    "order-1",
		TotalDebit: 40,
		UnitPrice:  20,
		Keys: []supplierKey{
			{Key: "ksk_supplier_one"},
			{Key: "ksk_supplier_two"},
		},
	}}
	_, manager, provider := newSupplierTestManager(t, fake)
	outcome, err := manager.startPurchase(provider, 2, "us", true, "manual")
	if err != nil {
		t.Fatalf("startPurchase: %v", err)
	}
	if outcome.Batch.Imported != 2 || outcome.Batch.Status != "active" {
		t.Fatalf("unexpected batch: %+v", outcome.Batch)
	}
	accounts := config.GetAccounts()
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	}
	for _, account := range accounts {
		if account.MaxSSE != 300 || account.MaxRPM != 200 {
			t.Fatalf("supplier defaults not applied: %+v", account)
		}
		if account.SupplierID != provider.ID || account.SupplierBatchID != outcome.Intent.ID {
			t.Fatalf("supplier provenance not applied: %+v", account)
		}
		if !account.Enabled || account.AuthMethod != "api_key" || account.Region != "us-east-1" {
			t.Fatalf("invalid imported account: %+v", account)
		}
	}
}

func TestSupplierSettingsUpdatePollIntervalAndPreserveItForOlderClients(t *testing.T) {
	fake := &fakeSupplierAPI{}
	h, manager, _ := newSupplierTestManager(t, fake)

	request := httptest.NewRequest(http.MethodPost, "/admin/api/suppliers/settings", strings.NewReader(`{"enabled":true,"autoPurchaseEnabled":true,"pollIntervalSeconds":1}`))
	response := httptest.NewRecorder()
	h.apiUpdateSupplierFeature(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", response.Code, response.Body.String())
	}
	if got := config.GetSupplierIntegration().PollIntervalSeconds; got != 1 {
		t.Fatalf("poll interval = %d, want 1", got)
	}
	if len(manager.wake) != 1 {
		t.Fatalf("settings update queued %d wakes, want 1", len(manager.wake))
	}

	invalid := httptest.NewRequest(http.MethodPost, "/admin/api/suppliers/settings", strings.NewReader(`{"enabled":true,"autoPurchaseEnabled":true,"pollIntervalSeconds":0}`))
	invalidResponse := httptest.NewRecorder()
	h.apiUpdateSupplierFeature(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest || config.GetSupplierIntegration().PollIntervalSeconds != 1 {
		t.Fatalf("invalid settings status=%d config=%+v", invalidResponse.Code, config.GetSupplierIntegration())
	}

	legacy := httptest.NewRequest(http.MethodPost, "/admin/api/suppliers/settings", strings.NewReader(`{"enabled":false,"autoPurchaseEnabled":false}`))
	legacyResponse := httptest.NewRecorder()
	h.apiUpdateSupplierFeature(legacyResponse, legacy)
	if legacyResponse.Code != http.StatusOK || config.GetSupplierIntegration().PollIntervalSeconds != 1 {
		t.Fatalf("legacy settings status=%d config=%+v", legacyResponse.Code, config.GetSupplierIntegration())
	}
}

func TestSupplierManagerReschedulesPollIntervalWithoutRestart(t *testing.T) {
	calls := make(chan struct{}, 4)
	fake := &fakeSupplierAPI{stockCallCh: calls}
	_, manager, _ := newSupplierTestManager(t, fake)

	var intervalMu sync.RWMutex
	interval := time.Hour
	manager.pollInterval = func() time.Duration {
		intervalMu.RLock()
		defer intervalMu.RUnlock()
		return interval
	}
	stop := make(chan struct{})
	manager.stop = stop
	done := make(chan struct{})
	go func() {
		manager.run()
		close(done)
	}()

	intervalMu.Lock()
	interval = 20 * time.Millisecond
	intervalMu.Unlock()
	manager.signal("")

	for call := 1; call <= 2; call++ {
		select {
		case <-calls:
		case <-time.After(500 * time.Millisecond):
			close(stop)
			<-done
			t.Fatalf("timed out waiting for stock call %d after dynamic reschedule", call)
		}
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supplier manager did not stop")
	}
}

func TestSupplierPurchaseEUUsesExpectedAccountRegion(t *testing.T) {
	fake := &fakeSupplierAPI{purchase: supplierPurchaseResponse{
		Purchased: 1,
		Requested: 1,
		OrderID:   "order-eu",
		Keys:      []supplierKey{{Key: "ksk_supplier_eu"}},
	}}
	_, manager, provider := newSupplierTestManager(t, fake)
	outcome, err := manager.startPurchase(provider, 1, "eu", true, "manual")
	if err != nil {
		t.Fatalf("startPurchase: %v", err)
	}
	accounts := config.GetAccounts()
	if outcome.Batch.Imported != 1 || len(accounts) != 1 || accounts[0].Region != "eu-central-1" {
		t.Fatalf("unexpected EU import: batch=%+v accounts=%+v", outcome.Batch, accounts)
	}
}

func TestManualPurchaseCanSkipAutomaticImport(t *testing.T) {
	fake := &fakeSupplierAPI{purchase: supplierPurchaseResponse{
		Purchased: 1,
		Requested: 1,
		OrderID:   "order-copy-only",
		Keys:      []supplierKey{{Key: "ksk_copy_only"}},
	}}
	_, manager, provider := newSupplierTestManager(t, fake)
	outcome, err := manager.startPurchase(provider, 1, "us", false, "manual")
	if err != nil {
		t.Fatalf("startPurchase: %v", err)
	}
	if outcome.Batch.Status != "not_imported" || outcome.Batch.Imported != 0 || len(config.GetAccounts()) != 0 {
		t.Fatalf("copy-only purchase imported an account: batch=%+v accounts=%+v", outcome.Batch, config.GetAccounts())
	}
	if len(outcome.Response.Keys) != 1 || outcome.Response.Keys[0].Value() != "ksk_copy_only" {
		t.Fatalf("copy-only purchase did not return the actual key: %+v", outcome.Response)
	}
}

func TestSupplierPurchaseRetriesSameIdAfterAmbiguousFailure(t *testing.T) {
	fake := &fakeSupplierAPI{purchaseErr: errors.New("connection reset after write")}
	_, manager, provider := newSupplierTestManager(t, fake)
	outcome, err := manager.startPurchase(provider, 1, "us", true, "manual")
	if err == nil || !outcome.Pending {
		t.Fatalf("first purchase = %+v, %v; want pending error", outcome, err)
	}
	pending := manager.store.pendingIntents()
	if len(pending) != 1 {
		t.Fatalf("pending intents = %d, want 1", len(pending))
	}

	fake.mu.Lock()
	fake.purchaseErr = nil
	fake.purchase = supplierPurchaseResponse{
		Purchased: 1,
		Requested: 1,
		OrderID:   "order-replayed",
		Replayed:  true,
		Keys:      []supplierKey{{Key: "ksk_recovered"}},
	}
	fake.mu.Unlock()
	second, err := manager.executeIntent(pending[0])
	if err != nil {
		t.Fatalf("executeIntent retry: %v", err)
	}
	if second.Batch.Imported != 1 || len(config.GetAccounts()) != 1 {
		t.Fatalf("retry did not import exactly once: %+v accounts=%d", second.Batch, len(config.GetAccounts()))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 2 || fake.purchaseIDs[0] != fake.purchaseIDs[1] {
		t.Fatalf("idempotency IDs = %#v", fake.purchaseIDs)
	}
}

func TestAutomaticPurchaseRequiresZeroLiveKeysAndUsesUS(t *testing.T) {
	fake := &fakeSupplierAPI{
		stock: supplierStock{Stock: 4, StockUS: 4, StockEU: 9, Balance: 100},
		purchase: supplierPurchaseResponse{
			Purchased: 2,
			Requested: 2,
			OrderID:   "auto-order",
			Keys:      []supplierKey{{Key: "ksk_auto_one"}, {Key: "ksk_auto_two"}},
		},
	}
	_, manager, _ := newSupplierTestManager(t, fake)
	manager.maybeAutoPurchase("vendor-a")
	if got := len(config.GetAccounts()); got != 2 {
		t.Fatalf("automatic import accounts = %d, want 2", got)
	}
	manager.maybeAutoPurchase("vendor-a")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 1 {
		t.Fatalf("purchase calls = %d, want 1 after live-key guard", fake.purchaseCalls)
	}
	if len(fake.purchaseRegions) != 1 || fake.purchaseRegions[0] != "us" {
		t.Fatalf("purchase regions = %#v", fake.purchaseRegions)
	}
}

func TestOAuthAccountDoesNotPreventAutomaticAPIKeyReplenishment(t *testing.T) {
	fake := &fakeSupplierAPI{
		stock: supplierStock{StockUS: 2, Balance: 100},
		purchase: supplierPurchaseResponse{
			Purchased: 1,
			Requested: 2,
			OrderID:   "auto-with-oauth",
			Keys:      []supplierKey{{Key: "ksk_auto_with_oauth"}},
		},
	}
	_, manager, _ := newSupplierTestManager(t, fake)
	if err := config.AddAccount(config.Account{ID: "oauth-account", Email: "oauth@example.com", AuthMethod: "social", AccessToken: "oauth-token", Enabled: true}); err != nil {
		t.Fatalf("AddAccount OAuth: %v", err)
	}
	manager.handler.pool.Reload()
	manager.maybeAutoPurchase("vendor-a")
	if liveAPIKeyCount() != 1 {
		t.Fatalf("live API key count = %d, want 1 after replenishment", liveAPIKeyCount())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 1 {
		t.Fatalf("OAuth account incorrectly prevented replenishment, calls=%d", fake.purchaseCalls)
	}
}

func TestAutomaticPurchaseFallsThroughCompatibleSuppliers(t *testing.T) {
	primary := &fakeSupplierAPI{stock: supplierStock{StockUS: 0, Balance: 100}}
	secondary := &fakeSupplierAPI{
		stock: supplierStock{StockUS: 1, Balance: 100},
		purchase: supplierPurchaseResponse{
			Purchased: 1,
			Requested: 1,
			OrderID:   "secondary-order",
			Keys:      []supplierKey{{Key: "ksk_secondary"}},
		},
	}
	_, manager, _ := newSupplierTestManager(t, primary)
	if _, err := config.AddSupplierProvider(config.SupplierProvider{
		ID: "vendor-b", Name: "Vendor B", BaseURL: "https://vendor-b.example", APIToken: "km_b", Enabled: true, Priority: 2, AutoPurchaseCount: 1,
	}); err != nil {
		t.Fatalf("AddSupplierProvider vendor-b: %v", err)
	}
	manager.apiFactory = func(provider config.SupplierProvider) supplierAPI {
		if provider.ID == "vendor-b" {
			return secondary
		}
		return primary
	}
	manager.maybeAutoPurchase("")

	primary.mu.Lock()
	primaryCalls := primary.purchaseCalls
	primary.mu.Unlock()
	secondary.mu.Lock()
	secondaryCalls := secondary.purchaseCalls
	secondary.mu.Unlock()
	if primaryCalls != 0 || secondaryCalls != 1 || liveAPIKeyCount() != 1 {
		t.Fatalf("unexpected supplier fallback: primary=%d secondary=%d live=%d", primaryCalls, secondaryCalls, liveAPIKeyCount())
	}
}

func TestAutomaticPurchaseCircuitBreakerPreventsRepeatedCharges(t *testing.T) {
	fake := &fakeSupplierAPI{
		stock: supplierStock{Stock: 3, StockUS: 3, Balance: 100},
		purchase: supplierPurchaseResponse{
			Purchased: 1,
			Requested: 1,
			OrderID:   "empty-auto-order",
			// A malformed or faulty supplier may claim a purchase while returning
			// no usable keys. This must never trigger another charge next tick.
			Keys: nil,
		},
	}
	_, manager, _ := newSupplierTestManager(t, fake)
	manager.maybeAutoPurchase("vendor-a")
	manager.maybeAutoPurchase("vendor-a")

	fake.mu.Lock()
	calls := fake.purchaseCalls
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("purchase calls = %d, want exactly 1 after safety block", calls)
	}
	if !manager.autoPurchaseBlocked("vendor-a") {
		t.Fatal("automatic purchase was not blocked after an empty import")
	}

	// The circuit breaker is durable across a process restart.
	reloaded, err := newSupplierStateStore(filepath.Dir(manager.store.path))
	if err != nil {
		t.Fatalf("reload supplier state: %v", err)
	}
	if _, ok := reloaded.autoBlocks()["vendor-a"]; !ok {
		t.Fatal("automatic purchase safety block was not persisted")
	}
}

func TestAutomaticPurchaseRejectionPausesUntilConnectionTest(t *testing.T) {
	fake := &fakeSupplierAPI{
		stock:       supplierStock{StockUS: 2, Balance: 0},
		purchaseErr: &supplierAPIError{StatusCode: http.StatusForbidden, Message: "insufficient balance"},
	}
	h, manager, provider := newSupplierTestManager(t, fake)
	manager.maybeAutoPurchase(provider.ID)
	manager.maybeAutoPurchase(provider.ID)
	fake.mu.Lock()
	callsWhileRejected := fake.purchaseCalls
	fake.purchaseErr = nil
	fake.purchase = supplierPurchaseResponse{
		Purchased: 1,
		Requested: 1,
		OrderID:   "recovered-order",
		Keys:      []supplierKey{{Key: "ksk_after_recharge"}},
	}
	fake.stock.Balance = 100
	fake.mu.Unlock()
	block, blocked := manager.automaticPurchaseBlock(provider.ID)
	if callsWhileRejected != 1 || !blocked || block.Reason != "purchase_rejected" {
		t.Fatalf("rejected purchase was not safely paused: calls=%d block=%+v blocked=%v", callsWhileRejected, block, blocked)
	}

	testResponse := httptest.NewRecorder()
	h.apiTestSupplier(testResponse, httptest.NewRequest(http.MethodPost, "/admin/api/suppliers/vendor-a/test", nil), provider.ID)
	if testResponse.Code != http.StatusOK || manager.autoPurchaseBlocked(provider.ID) {
		t.Fatalf("successful connection test did not restore supplier: status=%d body=%s", testResponse.Code, testResponse.Body.String())
	}
	manager.maybeAutoPurchase(provider.ID)
	if liveAPIKeyCount() != 1 {
		t.Fatalf("automatic purchase did not recover after connection test, live=%d", liveAPIKeyCount())
	}
}

func TestSuccessfulManualImportClearsAutomaticPurchaseCircuitBreaker(t *testing.T) {
	fake := &fakeSupplierAPI{purchase: supplierPurchaseResponse{
		Purchased: 1,
		Requested: 1,
		OrderID:   "manual-recovery",
		Keys:      []supplierKey{{Key: "ksk_manual_recovery"}},
	}}
	_, manager, provider := newSupplierTestManager(t, fake)
	if err := manager.blockAutomaticPurchases(provider.ID, "failed-auto-batch", "no_importable_keys"); err != nil {
		t.Fatalf("block automatic purchase: %v", err)
	}
	if _, err := manager.startPurchase(provider, 1, "us", true, "manual"); err != nil {
		t.Fatalf("manual recovery purchase: %v", err)
	}
	if manager.autoPurchaseBlocked(provider.ID) {
		t.Fatal("successful manual import did not clear automatic purchase safety block")
	}
	if _, ok := manager.store.autoBlocks()[provider.ID]; ok {
		t.Fatal("cleared automatic purchase safety block remained on disk")
	}
}

func TestSupplierWebhookIsProviderScopedDurableAndDeduplicated(t *testing.T) {
	fake := &fakeSupplierAPI{}
	h, _, _ := newSupplierTestManager(t, fake)
	if _, err := config.AddSupplierProvider(config.SupplierProvider{
		ID: "vendor-b", Name: "Vendor B", BaseURL: "https://vendor-b.example", APIToken: "km_b", Enabled: true, AutoPurchaseCount: 1,
	}); err != nil {
		t.Fatalf("AddSupplierProvider vendor-b: %v", err)
	}
	body := []byte(`{"event":"new_keys_available","event_id":"evt-1","visibility":"public","new_keys":3,"order_id":"batch-1","purchase_order_id":"0123456789abcdef0123456789abcdef","mother_id":"mother-1","message":"new keys","finished_at":"2026-07-31 14:30:05 CST","stock_us":3,"stock_eu":0,"price_us":20,"price_eu":15}`)
	call := func(providerID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/"+providerID, bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first := call("vendor-a")
	if first.Code != http.StatusOK {
		t.Fatalf("first webhook status=%d body=%s", first.Code, first.Body.String())
	}
	second := call("vendor-a")
	if second.Code != http.StatusOK {
		t.Fatalf("second webhook status=%d body=%s", second.Code, second.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(second.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if duplicate, _ := response["duplicate"].(bool); !duplicate {
		t.Fatalf("duplicate response = %#v", response)
	}
	otherProvider := call("vendor-b")
	if otherProvider.Code != http.StatusOK {
		t.Fatalf("other provider webhook status=%d body=%s", otherProvider.Code, otherProvider.Body.String())
	}
	response = nil
	if err := json.Unmarshal(otherProvider.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode other provider response: %v", err)
	}
	if duplicate, _ := response["duplicate"].(bool); duplicate {
		t.Fatalf("event id should be scoped per permanent provider route: %#v", response)
	}
}

func TestSupplierWebhookEmptyBodyIsAReadOnlyHealthCheck(t *testing.T) {
	fake := &fakeSupplierAPI{}
	h, manager, _ := newSupplierTestManager(t, fake)
	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/vendor-a", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for _, body := range []string{"", " \n\t "} {
		response := call(body)
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"ok":"true"}` {
			t.Fatalf("empty webhook status=%d body=%q", response.Code, response.Body.String())
		}
	}
	if len(manager.store.state.Events) != 0 || len(manager.wake) != 0 {
		t.Fatalf("health check changed webhook state: events=%d wake=%d", len(manager.store.state.Events), len(manager.wake))
	}

	if err := config.UpdateSupplierFeature(false, true); err != nil {
		t.Fatalf("disable supplier feature: %v", err)
	}
	disabledResponse := call("")
	if disabledResponse.Code != http.StatusOK || strings.TrimSpace(disabledResponse.Body.String()) != `{"ok":"true"}` {
		t.Fatalf("disabled health check status=%d body=%q", disabledResponse.Code, disabledResponse.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 0 {
		t.Fatalf("health check triggered %d purchases", fake.purchaseCalls)
	}
}

func TestSupplierWebhookDisabledAcknowledgesWithoutQueueing(t *testing.T) {
	fake := &fakeSupplierAPI{}
	h, manager, _ := newSupplierTestManager(t, fake)
	if err := config.UpdateSupplierFeature(false, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/vendor-a", bytes.NewBufferString(`not-json`))
	rec := httptest.NewRecorder()
	h.handleSupplierWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(manager.store.state.Events) != 0 {
		t.Fatalf("disabled integration persisted events: %+v", manager.store.state.Events)
	}
	refresh := httptest.NewRecorder()
	refreshRequest := httptest.NewRequest(http.MethodPost, "/admin/api/suppliers/refresh", nil)
	refreshRequest.Header.Set("X-Admin-Password", config.GetPassword())
	h.ServeHTTP(refresh, refreshRequest)
	if refresh.Code != http.StatusConflict {
		t.Fatalf("disabled inventory refresh status=%d body=%s", refresh.Code, refresh.Body.String())
	}
}

func TestSupplierWebhookRejectsOversizedOrAmbiguousBodies(t *testing.T) {
	fake := &fakeSupplierAPI{}
	h, _, _ := newSupplierTestManager(t, fake)

	oversizedBody := []byte(`{"event":"test","event_id":"large","message":"` + strings.Repeat("x", int(supplierWebhookBodyLimit)) + `"}`)
	oversized := httptest.NewRecorder()
	h.ServeHTTP(oversized, httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/vendor-a", bytes.NewReader(oversizedBody)))
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized webhook status=%d body=%s", oversized.Code, oversized.Body.String())
	}

	ambiguous := httptest.NewRecorder()
	h.ServeHTTP(ambiguous, httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/vendor-a", bytes.NewBufferString(`{"event":"test","event_id":"one"}{"event":"test","event_id":"two"}`)))
	if ambiguous.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous webhook status=%d body=%s", ambiguous.Code, ambiguous.Body.String())
	}
}

func TestSupplierAdminOverviewRequiresAuthAndDoesNotExposeToken(t *testing.T) {
	fake := &fakeSupplierAPI{}
	h, _, _ := newSupplierTestManager(t, fake)
	config.SetPassword("admin-secret")

	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/admin/api/suppliers/overview", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/suppliers/overview", nil)
	req.Header.Set("X-Admin-Password", "admin-secret")
	authorized := httptest.NewRecorder()
	h.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status=%d body=%s", authorized.Code, authorized.Body.String())
	}
	body := authorized.Body.String()
	if strings.Contains(body, "km_supplier_secret") || strings.Contains(body, `"apiToken"`) {
		t.Fatalf("overview exposed write-only supplier token: %s", body)
	}
	if !strings.Contains(body, `"tokenMasked"`) || !strings.Contains(body, `"webhookPath":"/api/supplier-webhooks/vendor-a"`) {
		t.Fatalf("overview missing safe supplier metadata: %s", body)
	}
}

func TestSupplierBatchLifetimeExcludesManualShutdowns(t *testing.T) {
	store, err := newSupplierStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newSupplierStateStore: %v", err)
	}
	originalNow := supplierNow
	defer func() { supplierNow = originalNow }()
	supplierNow = func() time.Time { return time.Unix(200, 0) }

	for _, batch := range []supplierBatch{
		{ID: "natural", ProviderID: "vendor-a", AccountIDs: []string{"natural-account"}, Imported: 1, ActiveCount: 1, Status: "active", CreatedAt: 100},
		{ID: "manual", ProviderID: "vendor-a", AccountIDs: []string{"manual-account"}, Imported: 1, ActiveCount: 1, Status: "active", CreatedAt: 120},
	} {
		if err := store.upsertBatch(batch); err != nil {
			t.Fatalf("upsert batch: %v", err)
		}
	}
	accounts := []config.Account{
		{ID: "natural-account", Enabled: false, BanStatus: "BANNED", BanReason: "unauthorized"},
		{ID: "manual-account", Enabled: false},
	}
	if err := store.reconcileBatches(accounts); err != nil {
		t.Fatalf("reconcileBatches: %v", err)
	}
	batches := store.batches()
	statuses := make(map[string]string)
	for _, batch := range batches {
		statuses[batch.ID] = batch.Status
	}
	if statuses["natural"] != "dead" || statuses["manual"] != "ended_manual" {
		t.Fatalf("unexpected lifetime statuses: %#v", statuses)
	}
	summary := supplierLifetimeSummary(batches)
	if summary["samples"] != 1 || summary["averageSeconds"] != int64(100) || summary["medianSeconds"] != int64(100) {
		t.Fatalf("manual batch polluted lifetime summary: %#v", summary)
	}
}

func TestHTTPSupplierAPIContract(t *testing.T) {
	var purchaseID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer km_contract" {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/api/me/profile":
			json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{"id": "u1", "name": "alice", "balance": 90, "min_purchase": 1, "max_purchase": 10}})
		case "/api/me/stock":
			json.NewEncoder(w).Encode(map[string]any{"stock": 3, "stock_us": 2, "stock_eu": 1, "balance": 90, "price_min": 20, "price_max": 30})
		case "/api/me/keys":
			if r.URL.Query().Get("history") != "1" || r.URL.Query().Get("page_size") != "500" {
				t.Errorf("unexpected keys query: %s", r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"key_value": "ksk_list"}}, "total": 1, "page": 1, "page_size": 500, "pages": 1})
		case "/api/me/purchase":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode purchase: %v", err)
			}
			purchaseID, _ = body["client_order_id"].(string)
			json.NewEncoder(w).Encode(map[string]any{"purchased": 1, "requested": 1, "order_id": "o1", "keys": []map[string]any{{"key": "ksk_purchase"}}})
		default:
			http.Error(w, fmt.Sprintf("unexpected path %s", r.URL.Path), http.StatusNotFound)
		}
	}))
	defer server.Close()
	api := newHTTPSupplierAPI(config.SupplierProvider{BaseURL: server.URL, APIToken: "km_contract"})
	profile, err := api.GetProfile()
	if err != nil || profile.User.Balance != 90 || profile.User.MaxPurchase != 10 {
		t.Fatalf("GetProfile = %+v, %v", profile, err)
	}
	stock, err := api.GetStock()
	if err != nil || stock.StockUS != 2 || stock.Balance != 90 {
		t.Fatalf("GetStock = %+v, %v", stock, err)
	}
	keys, err := api.GetKeys(true, 1, 999)
	if err != nil || len(keys.Items) != 1 || keys.Items[0].Value() != "ksk_list" {
		t.Fatalf("GetKeys = %+v, %v", keys, err)
	}
	purchased, err := api.Purchase(1, "us", "0123456789abcdef0123456789abcdef")
	if err != nil || purchased.Purchased != 1 || purchaseID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("Purchase = %+v, %v id=%q", purchased, err, purchaseID)
	}
}

func TestSupplierAPIErrorClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "rate limited"})
	}))
	defer server.Close()
	api := newHTTPSupplierAPI(config.SupplierProvider{BaseURL: server.URL, APIToken: "km_contract"})
	_, err := api.GetStock()
	if err == nil || !isRetryableSupplierError(err) {
		t.Fatalf("expected retryable 429, got %v", err)
	}
}

func TestSupplierConnectionCheckRequiresStockAndKeyAccess(t *testing.T) {
	fake := &fakeSupplierAPI{
		stock:   supplierStock{StockUS: 2, Balance: 100},
		keysErr: errors.New("keys endpoint unavailable"),
	}
	_, manager, provider := newSupplierTestManager(t, fake)
	stock, err := manager.refreshProviderStatus(provider, true)
	if err == nil || stock.StockUS != 2 {
		t.Fatalf("connection check = %+v, %v; want stock plus key-access error", stock, err)
	}
	status := manager.store.providerStatuses()[provider.ID]
	if !strings.Contains(status.LastError, "keys endpoint unavailable") {
		t.Fatalf("connection status did not preserve key-access failure: %+v", status)
	}
}
