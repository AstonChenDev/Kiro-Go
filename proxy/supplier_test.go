package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	publicStock     supplierPublicStock
	publicStockErr  error
	publicOrders    []supplierPublicPurchaseOrder
	publicOrdersErr error
	profile         supplierProfile
	keys            supplierKeysPage
	keysErr         error
	purchase        supplierPurchaseResponse
	purchaseErr     error
	purchaseCalls   int
	purchaseIDs     []string
	purchaseRegions []string
	purchaseCounts  []int
	purchaseOrders  []string
	purchaseSources []string
	purchaseBatches []string
	stockCallCh     chan struct{}
	setWebhookURL   string
	webhookSecret   string
	setWebhookErr   error
	webhookTestErr  error
	webhookTests    int
}

func TestKiroDropSupplierAPIContract(t *testing.T) {
	const webhookSecret = "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
	var purchaseBody map[string]any
	var webhookMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != "usr-contract" {
			t.Errorf("X-API-Key = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("unexpected Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/api/my/profile":
			json.NewEncoder(w).Encode(map[string]any{
				"name": "drop@example.com", "quota": "2000.000000", "remaining": "884.400000", "used_quota": "1115.600000",
			})
		case "/api/me/stock":
			switch r.URL.Query().Get("region") {
			case "us":
				json.NewEncoder(w).Encode(map[string]any{"region": "us-east-1", "stock": 12, "price": "30.00", "balance": "884.40"})
			case "eu":
				json.NewEncoder(w).Encode(map[string]any{"region": "eu-central-1", "stock": 3, "price": "25.00", "balance": "884.40"})
			default:
				t.Errorf("unexpected stock region %q", r.URL.Query().Get("region"))
			}
		case "/api/my/purchase":
			if err := json.NewDecoder(r.Body).Decode(&purchaseBody); err != nil {
				t.Errorf("decode purchase: %v", err)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"client_order_id": "0123456789abcdef0123456789abcdef", "order_id": "store_order_1",
				"region": "us-east-1", "purchased": 2, "remaining": "824.400000", "status": "completed",
				"refunded_amount_cny": "0.000000", "keys": []map[string]any{
					{"key": "ksk_drop_one", "region": "us-east-1"}, {"key": "ksk_drop_two", "region": "us-east-1"},
				},
			})
		case "/api/my/webhook":
			webhookMethod = r.Method
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode webhook body: %v", err)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"ok": "true", "webhook_url": body["webhook_url"], "webhook_secret": webhookSecret,
			})
		case "/api/my/webhook/test":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, fmt.Sprintf("unexpected path %s", r.URL.Path), http.StatusNotFound)
		}
	}))
	defer server.Close()

	api := newHTTPSupplierAPI(config.SupplierProvider{
		BaseURL: server.URL, APIToken: "usr-contract", APIType: config.SupplierAPITypeKiroDrop,
	})
	profile, err := api.GetProfile()
	if err != nil || profile.User.Name != "drop@example.com" || profile.User.Balance != 884.4 {
		t.Fatalf("GetProfile = %+v, %v", profile, err)
	}
	stock, err := api.GetStock()
	if err != nil || stock.Stock != 15 || stock.StockUS != 12 || stock.StockEU != 3 || stock.Balance != 884.4 || stock.PriceMin != 25 || stock.PriceMax != 30 {
		t.Fatalf("GetStock = %+v, %v", stock, err)
	}
	if _, err := api.GetKeys(false, 1, 50); !errors.Is(err, errSupplierOperationUnsupported) {
		t.Fatalf("GetKeys error = %v", err)
	}
	purchase, err := api.Purchase(supplierPurchaseRequest{
		Count: 2, Region: "us", ClientOrderID: "0123456789abcdef0123456789abcdef", SupplierOrderID: "batch_us_1",
	})
	if err != nil || purchase.Purchased != 2 || purchase.OrderID != "store_order_1" || purchase.Remaining != "824.400000" {
		t.Fatalf("Purchase = %+v, %v", purchase, err)
	}
	if purchaseBody["count"] != float64(2) || purchaseBody["region"] != "us" || purchaseBody["order_id"] != "batch_us_1" ||
		purchaseBody["client_order_id"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("purchase body = %#v", purchaseBody)
	}
	secret, err := api.SetWebhook("https://kiro.example/api/supplier-webhooks/kiro-drop")
	if err != nil || secret != webhookSecret || webhookMethod != http.MethodPut {
		t.Fatalf("SetWebhook secret=%q method=%q err=%v", secret, webhookMethod, err)
	}
	if err := api.TestWebhook(); err != nil {
		t.Fatalf("TestWebhook: %v", err)
	}
}

func TestKiroDropPurchaseRejectsWrongRegionAndRefundedReplay(t *testing.T) {
	request := supplierPurchaseRequest{Count: 1, Region: "us", ClientOrderID: "0123456789abcdef0123456789abcdef"}
	tests := []struct {
		name          string
		response      map[string]any
		wantRetryable bool
	}{
		{name: "wrong response region", wantRetryable: true, response: map[string]any{
			"client_order_id": request.ClientOrderID, "order_id": "store_1", "region": "eu-central-1", "status": "completed", "purchased": 1,
			"keys": []map[string]any{{"key": "ksk_wrong_region", "region": "eu-central-1"}},
		}},
		{name: "refunded replay", response: map[string]any{
			"client_order_id": request.ClientOrderID, "order_id": "store_2", "region": "us-east-1", "status": "refunded", "purchased": 1,
			"keys": []map[string]any{{"key": "ksk_refunded", "region": "us-east-1"}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { json.NewEncoder(w).Encode(tt.response) }))
			defer server.Close()
			api := newHTTPSupplierAPI(config.SupplierProvider{BaseURL: server.URL, APIToken: "usr-secret", APIType: config.SupplierAPITypeKiroDrop})
			if _, err := api.Purchase(request); err == nil || isRetryableSupplierError(err) != tt.wantRetryable {
				t.Fatalf("unsafe response error=%v retryable=%v, want %v", err, isRetryableSupplierError(err), tt.wantRetryable)
			}
		})
	}
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

func (f *fakeSupplierAPI) GetPublicStock() (supplierPublicStock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stockCallCh != nil {
		select {
		case f.stockCallCh <- struct{}{}:
		default:
		}
	}
	return f.publicStock, f.publicStockErr
}

func (f *fakeSupplierAPI) GetPublicPurchaseOrders() ([]supplierPublicPurchaseOrder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]supplierPublicPurchaseOrder(nil), f.publicOrders...), f.publicOrdersErr
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

func (f *fakeSupplierAPI) Purchase(request supplierPurchaseRequest) (supplierPurchaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purchaseCalls++
	f.purchaseIDs = append(f.purchaseIDs, request.ClientOrderID)
	f.purchaseRegions = append(f.purchaseRegions, request.Region)
	f.purchaseCounts = append(f.purchaseCounts, request.Count)
	f.purchaseOrders = append(f.purchaseOrders, request.SupplierOrderID)
	f.purchaseSources = append(f.purchaseSources, request.PurchaseSource)
	f.purchaseBatches = append(f.purchaseBatches, request.BatchID)
	return f.purchase, f.purchaseErr
}

func (f *fakeSupplierAPI) SetWebhook(webhookURL string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setWebhookURL = webhookURL
	return f.webhookSecret, f.setWebhookErr
}

func (f *fakeSupplierAPI) TestWebhook() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.webhookTests++
	return f.webhookTestErr
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

func TestSupplierPurchaseSourceCannotChangeWhileOrderIsPending(t *testing.T) {
	fake := &fakeSupplierAPI{}
	h, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	now := supplierNow().Unix()
	if err := manager.store.createIntent(supplierPurchaseIntent{
		ID: "0123456789abcdef0123456789abcdef", ProviderID: updated.ID,
		PurchaseSource: config.SupplierPurchaseSourceOwn, Region: "us", Count: 1,
		ClientOrderID: "0123456789abcdef0123456789abcdef", Status: "pending", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create pending intent: %v", err)
	}

	body := fmt.Sprintf(`{"name":%q,"baseUrl":%q,"apiType":"aws_my","purchaseSource":"public","enabled":true,"priority":1,"autoPurchaseCount":1}`, updated.Name, updated.BaseURL)
	rec := httptest.NewRecorder()
	h.apiUpdateSupplier(rec, httptest.NewRequest(http.MethodPut, "/admin/api/suppliers/"+updated.ID, strings.NewReader(body)), updated.ID)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "purchase source") {
		t.Fatalf("pending source change status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := config.GetSupplierProvider(updated.ID)
	if got == nil || got.PurchaseSource != config.SupplierPurchaseSourceOwn {
		t.Fatalf("pending source change mutated provider: %+v", got)
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

func TestAWSMyManualPurchaseRejectsUnsupportedEUSelection(t *testing.T) {
	fake := &fakeSupplierAPI{}
	_, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	if _, err := manager.startPurchase(provider, 1, "eu", true, "manual"); err == nil || !strings.Contains(err.Error(), "regionless") {
		t.Fatalf("AWS My EU purchase error = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 0 {
		t.Fatalf("unsupported AWS My EU purchase made %d call(s)", fake.purchaseCalls)
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
	if len(pending) != 1 || !pending[0].MustResolve {
		t.Fatalf("pending intents = %+v, want one must-resolve retry", pending)
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

func TestKiroDropManualPurchaseSupportsEUAndImportsRegionalKey(t *testing.T) {
	fake := &fakeSupplierAPI{purchase: supplierPurchaseResponse{
		Purchased: 1, Requested: 1, OrderID: "drop-eu-order", Region: "eu-central-1", Status: "completed", Remaining: "70.00",
		Keys: []supplierKey{{Key: "ksk_drop_eu", Region: "eu-central-1"}},
	}}
	_, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeKiroDrop
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	outcome, err := manager.startPurchase(updated, 1, "eu", true, "manual")
	if err != nil {
		t.Fatalf("Kiro Drop EU purchase: %v", err)
	}
	accounts := config.GetAccounts()
	if outcome.Batch.Region != "eu" || len(accounts) != 1 || accounts[0].Region != "eu-central-1" {
		t.Fatalf("outcome=%+v accounts=%+v", outcome.Batch, accounts)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.purchaseRegions) != 1 || fake.purchaseRegions[0] != "eu" {
		t.Fatalf("purchase regions=%#v", fake.purchaseRegions)
	}
}

func TestPublicPoolManualPurchaseUsesSelectedBatchAndImports(t *testing.T) {
	fake := &fakeSupplierAPI{
		publicStock: supplierPublicStock{Total: 5, Batches: []supplierPublicBatch{
			{BatchID: "1005117684943163392", Available: 2, PublishedAt: "2026-08-05 10:00:00"},
			{BatchID: "1005117779411472384", Available: 3, PublishedAt: "2026-08-05 11:00:00"},
		}},
		purchase: supplierPurchaseResponse{
			BatchID: "1005117779411472384", Purchased: 2, Requested: 2,
			Keys: []supplierKey{{Key: "ksk_public_manual_one"}, {Key: "ksk_public_manual_two"}},
		},
	}
	_, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	provider.PurchaseSource = config.SupplierPurchaseSourcePublic
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}

	outcome, err := manager.startPurchaseFromBatch(updated, 2, "us", true, "manual", "1005117779411472384")
	if err != nil {
		t.Fatalf("public manual purchase: %v", err)
	}
	if outcome.Batch.Imported != 2 || outcome.Batch.PurchaseSource != config.SupplierPurchaseSourcePublic ||
		outcome.Batch.PublicBatchID != "1005117779411472384" {
		t.Fatalf("public batch = %+v", outcome.Batch)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 1 || fake.purchaseSources[0] != config.SupplierPurchaseSourcePublic ||
		fake.purchaseBatches[0] != "1005117779411472384" || fake.purchaseCounts[0] != 2 {
		t.Fatalf("public request sources=%#v batches=%#v counts=%#v", fake.purchaseSources, fake.purchaseBatches, fake.purchaseCounts)
	}
}

func TestPublicPoolAdminPurchasePassesSelectedBatch(t *testing.T) {
	fake := &fakeSupplierAPI{
		publicStock: supplierPublicStock{Total: 1, Batches: []supplierPublicBatch{{BatchID: "admin-selected", Available: 1}}},
		purchase: supplierPurchaseResponse{
			BatchID: "admin-selected", Purchased: 1, Requested: 1, Keys: []supplierKey{{Key: "ksk_admin_public"}},
		},
	}
	h, _, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	provider.PurchaseSource = config.SupplierPurchaseSourcePublic
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/suppliers/"+updated.ID+"/purchase", strings.NewReader(`{"count":1,"region":"us","batchId":"admin-selected","autoImport":false}`))
	rec := httptest.NewRecorder()
	h.apiPurchaseSupplierKeys(rec, req, updated.ID)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"publicBatchId":"admin-selected"`) {
		t.Fatalf("admin public purchase status=%d body=%s", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 1 || fake.purchaseBatches[0] != "admin-selected" || fake.purchaseSources[0] != config.SupplierPurchaseSourcePublic {
		t.Fatalf("admin public request batches=%#v sources=%#v", fake.purchaseBatches, fake.purchaseSources)
	}
}

func TestPublicPoolAutomaticPurchaseSelectsOneBatchAndClampsCount(t *testing.T) {
	fake := &fakeSupplierAPI{
		publicStock: supplierPublicStock{Total: 5, Batches: []supplierPublicBatch{
			{BatchID: "small-old", Available: 2, PublishedAt: "2026-08-05 10:00:00"},
			{BatchID: "large-new", Available: 3, PublishedAt: "2026-08-05 11:00:00"},
		}},
		purchase: supplierPurchaseResponse{
			BatchID: "large-new", Purchased: 3, Requested: 3,
			Keys: []supplierKey{{Key: "ksk_public_auto_one"}, {Key: "ksk_public_auto_two"}, {Key: "ksk_public_auto_three"}},
		},
	}
	_, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	provider.PurchaseSource = config.SupplierPurchaseSourcePublic
	provider.AutoPurchaseCount = 4
	if _, err := config.UpdateSupplierProvider(provider.ID, provider); err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}

	manager.maybeAutoPurchase(provider.ID)
	if liveAPIKeyCount() != 3 {
		t.Fatalf("public automatic import live=%d, want 3", liveAPIKeyCount())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 1 || fake.purchaseBatches[0] != "large-new" || fake.purchaseCounts[0] != 3 ||
		fake.purchaseSources[0] != config.SupplierPurchaseSourcePublic {
		t.Fatalf("public automatic requests batches=%#v counts=%#v sources=%#v", fake.purchaseBatches, fake.purchaseCounts, fake.purchaseSources)
	}
}

func TestPublicPoolStockContentionRetriesNextPollWithNewOrder(t *testing.T) {
	fake := &fakeSupplierAPI{
		publicStock: supplierPublicStock{Total: 1, Batches: []supplierPublicBatch{{BatchID: "contended", Available: 1}}},
		purchaseErr: &supplierAPIError{StatusCode: http.StatusConflict, Code: "STOCK_CHANGED", Message: "stock changed"},
	}
	_, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	provider.PurchaseSource = config.SupplierPurchaseSourcePublic
	provider.AutoPurchaseCount = 1
	if _, err := config.UpdateSupplierProvider(provider.ID, provider); err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}

	manager.maybeAutoPurchase(provider.ID)
	manager.maybeAutoPurchase(provider.ID)
	if manager.autoPurchaseBlocked(provider.ID) {
		t.Fatal("expected public inventory contention to remain eligible for the next poll")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 2 || fake.purchaseIDs[0] == fake.purchaseIDs[1] {
		t.Fatalf("contention calls=%d ids=%#v", fake.purchaseCalls, fake.purchaseIDs)
	}
}

func TestPublicPoolAmbiguousFailurePersistsExactRetryAcrossRestart(t *testing.T) {
	fake := &fakeSupplierAPI{
		publicStock: supplierPublicStock{Total: 2, Batches: []supplierPublicBatch{{BatchID: "restart-batch", Available: 2}}},
		purchaseErr: errors.New("connection reset after request write"),
	}
	_, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	provider.PurchaseSource = config.SupplierPurchaseSourcePublic
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}

	outcome, err := manager.startPurchaseFromBatch(updated, 2, "us", false, "manual", "restart-batch")
	if err == nil || !outcome.Pending {
		t.Fatalf("initial public purchase = %+v, %v", outcome, err)
	}
	reloaded, err := newSupplierStateStore(filepath.Dir(manager.store.path))
	if err != nil {
		t.Fatalf("reload supplier state: %v", err)
	}
	manager.store = reloaded
	pending := manager.store.pendingIntents()
	if len(pending) != 1 || pending[0].PurchaseSource != config.SupplierPurchaseSourcePublic ||
		pending[0].PublicBatchID != "restart-batch" || pending[0].Count != 2 {
		t.Fatalf("persisted public intent = %+v", pending)
	}

	fake.mu.Lock()
	fake.purchaseErr = nil
	fake.purchase = supplierPurchaseResponse{
		BatchID: "restart-batch", Purchased: 2, Requested: 2,
		Keys: []supplierKey{{Key: "ksk_public_restart_one"}, {Key: "ksk_public_restart_two"}},
	}
	fake.mu.Unlock()
	if _, err := manager.executeIntent(pending[0]); err != nil {
		t.Fatalf("retry public purchase: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 2 || fake.purchaseIDs[0] != fake.purchaseIDs[1] ||
		fake.purchaseBatches[0] != fake.purchaseBatches[1] || fake.purchaseCounts[0] != fake.purchaseCounts[1] ||
		fake.purchaseSources[0] != config.SupplierPurchaseSourcePublic || fake.purchaseSources[1] != config.SupplierPurchaseSourcePublic {
		t.Fatalf("public retries ids=%#v batches=%#v counts=%#v sources=%#v", fake.purchaseIDs, fake.purchaseBatches, fake.purchaseCounts, fake.purchaseSources)
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

func TestAutomaticPurchaseUsesEachSupplierLocalLiveCount(t *testing.T) {
	primary := &fakeSupplierAPI{
		stock: supplierStock{StockUS: 5, Balance: 100},
		purchase: supplierPurchaseResponse{
			Purchased: 1, Requested: 1, OrderID: "primary-should-not-run",
			Keys: []supplierKey{{Key: "ksk_primary_should_not_run"}},
		},
	}
	secondary := &fakeSupplierAPI{
		stock: supplierStock{StockUS: 1, Balance: 100},
		purchase: supplierPurchaseResponse{
			Purchased: 1, Requested: 1, OrderID: "secondary-local-replenishment",
			Keys: []supplierKey{{Key: "ksk_secondary_local_replenishment"}},
		},
	}
	_, manager, primaryProvider := newSupplierTestManager(t, primary)
	secondaryProvider, err := config.AddSupplierProvider(config.SupplierProvider{
		ID: "vendor-b", Name: "Vendor B", BaseURL: "https://vendor-b.example", APIToken: "km_b",
		Enabled: true, Priority: 2, AutoPurchaseCount: 1,
	})
	if err != nil {
		t.Fatalf("AddSupplierProvider vendor-b: %v", err)
	}
	if added, skipped, err := config.AddAccounts([]config.Account{
		{ID: "primary-live", AuthMethod: "api_key", KiroApiKey: "ksk_primary_live", SupplierID: primaryProvider.ID, Enabled: true},
		{ID: "secondary-dead", AuthMethod: "api_key", KiroApiKey: "ksk_secondary_dead", SupplierID: secondaryProvider.ID, Enabled: false},
		{ID: "manual-live", AuthMethod: "api_key", KiroApiKey: "ksk_manual_live", Enabled: true},
	}); err != nil || added != 3 || skipped != 0 {
		t.Fatalf("AddAccounts added=%d skipped=%d err=%v", added, skipped, err)
	}
	manager.handler.pool.Reload()
	manager.apiFactory = func(provider config.SupplierProvider) supplierAPI {
		if provider.ID == secondaryProvider.ID {
			return secondary
		}
		return primary
	}

	if liveAPIKeyCount() != 2 || liveSupplierAPIKeyCount(primaryProvider.ID) != 1 || liveSupplierAPIKeyCount(secondaryProvider.ID) != 0 {
		t.Fatalf("unexpected initial liveness: global=%d primary=%d secondary=%d",
			liveAPIKeyCount(), liveSupplierAPIKeyCount(primaryProvider.ID), liveSupplierAPIKeyCount(secondaryProvider.ID))
	}
	manager.maybeAutoPurchase(secondaryProvider.ID)

	primary.mu.Lock()
	primaryCalls := primary.purchaseCalls
	primary.mu.Unlock()
	secondary.mu.Lock()
	secondaryCalls := secondary.purchaseCalls
	secondary.mu.Unlock()
	if primaryCalls != 0 || secondaryCalls != 1 {
		t.Fatalf("supplier-scoped purchase calls: primary=%d secondary=%d", primaryCalls, secondaryCalls)
	}
	if liveSupplierAPIKeyCount(primaryProvider.ID) != 1 || liveSupplierAPIKeyCount(secondaryProvider.ID) != 1 || liveAPIKeyCount() != 3 {
		t.Fatalf("unexpected replenished liveness: global=%d primary=%d secondary=%d",
			liveAPIKeyCount(), liveSupplierAPIKeyCount(primaryProvider.ID), liveSupplierAPIKeyCount(secondaryProvider.ID))
	}

	// The final in-lock guard must also refuse a stale automatic decision if a
	// caller already has a provider snapshot but that supplier now has a key.
	if _, err := manager.startPurchase(primaryProvider, 1, supplierPurchaseRegionUS, true, "auto"); !errors.Is(err, errSupplierAutoPurchaseNotNeeded) {
		t.Fatalf("stale automatic purchase error = %v, want not-needed guard", err)
	}
}

func TestAutomaticPurchaseReplenishesAllEmptySuppliersInOnePass(t *testing.T) {
	primary := &fakeSupplierAPI{
		stock: supplierStock{StockUS: 1, Balance: 100},
		purchase: supplierPurchaseResponse{
			Purchased: 1, Requested: 1, OrderID: "primary-independent-order",
			Keys: []supplierKey{{Key: "ksk_primary_independent"}},
		},
	}
	secondary := &fakeSupplierAPI{
		stock: supplierStock{StockUS: 1, Balance: 100},
		purchase: supplierPurchaseResponse{
			Purchased: 1, Requested: 1, OrderID: "secondary-independent-order",
			Keys: []supplierKey{{Key: "ksk_secondary_independent"}},
		},
	}
	_, manager, primaryProvider := newSupplierTestManager(t, primary)
	secondaryProvider, err := config.AddSupplierProvider(config.SupplierProvider{
		ID: "vendor-b", Name: "Vendor B", BaseURL: "https://vendor-b.example", APIToken: "km_b",
		Enabled: true, Priority: 2, AutoPurchaseCount: 1,
	})
	if err != nil {
		t.Fatalf("AddSupplierProvider vendor-b: %v", err)
	}
	manager.apiFactory = func(provider config.SupplierProvider) supplierAPI {
		if provider.ID == secondaryProvider.ID {
			return secondary
		}
		return primary
	}

	manager.maybeAutoPurchase(secondaryProvider.ID)
	manager.maybeAutoPurchase("")

	primary.mu.Lock()
	primaryCalls, primaryRegion := primary.purchaseCalls, append([]string(nil), primary.purchaseRegions...)
	primary.mu.Unlock()
	secondary.mu.Lock()
	secondaryCalls, secondaryRegion := secondary.purchaseCalls, append([]string(nil), secondary.purchaseRegions...)
	secondary.mu.Unlock()
	if primaryCalls != 1 || secondaryCalls != 1 {
		t.Fatalf("independent purchase calls: primary=%d secondary=%d", primaryCalls, secondaryCalls)
	}
	if len(primaryRegion) != 1 || primaryRegion[0] != supplierPurchaseRegionUS || len(secondaryRegion) != 1 || secondaryRegion[0] != supplierPurchaseRegionUS {
		t.Fatalf("automatic regions: primary=%#v secondary=%#v", primaryRegion, secondaryRegion)
	}
	if liveSupplierAPIKeyCount(primaryProvider.ID) != 1 || liveSupplierAPIKeyCount(secondaryProvider.ID) != 1 {
		t.Fatalf("independent live counts: primary=%d secondary=%d",
			liveSupplierAPIKeyCount(primaryProvider.ID), liveSupplierAPIKeyCount(secondaryProvider.ID))
	}
}

func TestAmbiguousAutomaticPurchaseStopsOtherSuppliersUntilResolved(t *testing.T) {
	primary := &fakeSupplierAPI{
		stock:       supplierStock{StockUS: 1, Balance: 100},
		purchaseErr: errors.New("connection reset after request write"),
	}
	secondary := &fakeSupplierAPI{
		stock: supplierStock{StockUS: 1, Balance: 100},
		purchase: supplierPurchaseResponse{
			Purchased: 1, Requested: 1, OrderID: "must-wait-for-primary",
			Keys: []supplierKey{{Key: "ksk_must_wait_for_primary"}},
		},
	}
	_, manager, _ := newSupplierTestManager(t, primary)
	secondaryProvider, err := config.AddSupplierProvider(config.SupplierProvider{
		ID: "vendor-b", Name: "Vendor B", BaseURL: "https://vendor-b.example", APIToken: "km_b",
		Enabled: true, Priority: 2, AutoPurchaseCount: 1,
	})
	if err != nil {
		t.Fatalf("AddSupplierProvider vendor-b: %v", err)
	}
	manager.apiFactory = func(provider config.SupplierProvider) supplierAPI {
		if provider.ID == secondaryProvider.ID {
			return secondary
		}
		return primary
	}

	manager.maybeAutoPurchase("vendor-a")
	primary.mu.Lock()
	primaryCalls := primary.purchaseCalls
	primary.mu.Unlock()
	secondary.mu.Lock()
	secondaryCalls := secondary.purchaseCalls
	secondary.mu.Unlock()
	if primaryCalls != 1 || secondaryCalls != 0 || len(manager.store.pendingIntents()) != 1 {
		t.Fatalf("ambiguous purchase safety: primary=%d secondary=%d pending=%d", primaryCalls, secondaryCalls, len(manager.store.pendingIntents()))
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
	if ok, _ := response["ok"].(bool); !ok {
		t.Fatalf("normal webhook did not include strict success acknowledgement: %#v", response)
	}
}

func TestAWSMyWebhookNeedsNoCredentialsAndExtractsExactOrderOnce(t *testing.T) {
	fake := &fakeSupplierAPI{purchase: supplierPurchaseResponse{
		Purchased: 2, Requested: 2, Keys: []supplierKey{{Key: "ksk_webhook_one"}, {Key: "ksk_webhook_two"}},
	}}
	h, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	provider = updated

	body := `{"event":"new_keys_available","event_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","purchase_order_id":"0123456789ABCDEF0123456789ABCDEF","message":"new keys","new_keys":2}`
	call := func(eventBody, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/"+provider.ID, strings.NewReader(eventBody))
		if token != "" {
			req.Header.Set("X-API-Key", token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	healthCheck := call(`{"event":"webhook_test","event_id":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","message":"Webhook测试消息"}`, "")
	if healthCheck.Code != http.StatusOK || strings.TrimSpace(healthCheck.Body.String()) != `{"ok":"true"}` {
		t.Fatalf("AWS My webhook_test status=%d body=%s", healthCheck.Code, healthCheck.Body.String())
	}

	// The supplier contract deliberately sends no API credential on callbacks.
	// Kiro-Go uses the token stored on the provider only for the outbound extract.
	first := call(body, "")
	var ack map[string]any
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &ack) != nil || ack["ok"] != true || ack["queued"] != true {
		t.Fatalf("first webhook status=%d body=%s", first.Code, first.Body.String())
	}
	reloadedStore, err := newSupplierStateStore(filepath.Dir(manager.store.path))
	if err != nil {
		t.Fatalf("reload webhook state: %v", err)
	}
	manager.store = reloadedStore
	pending := manager.store.pendingIntents()
	if len(pending) != 1 || pending[0].ClientOrderID != "0123456789ABCDEF0123456789ABCDEF" || pending[0].Count != 2 || pending[0].SupplierOrderID != "" {
		t.Fatalf("queued intent = %+v", pending)
	}
	manager.processPendingIntents()
	if liveAPIKeyCount() != 2 {
		t.Fatalf("webhook import live count = %d", liveAPIKeyCount())
	}

	// An unrelated header must not change callback semantics either.
	duplicate := call(body, "wrong-token")
	ack = nil
	if duplicate.Code != http.StatusOK || json.Unmarshal(duplicate.Body.Bytes(), &ack) != nil || ack["ok"] != true || ack["duplicate"] != true {
		t.Fatalf("duplicate webhook status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	manager.processPendingIntents()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 1 || len(fake.purchaseIDs) != 1 || fake.purchaseIDs[0] != "0123456789ABCDEF0123456789ABCDEF" ||
		fake.purchaseCounts[0] != 2 || fake.purchaseRegions[0] != "us" || fake.purchaseOrders[0] != "" ||
		fake.purchaseSources[0] != config.SupplierPurchaseSourceOwn || fake.purchaseBatches[0] != "" {
		t.Fatalf("exact webhook purchase calls=%d ids=%#v counts=%#v regions=%#v orders=%#v", fake.purchaseCalls, fake.purchaseIDs, fake.purchaseCounts, fake.purchaseRegions, fake.purchaseOrders)
	}
}

func TestWebhookRetryRepairsLegacyEventWithoutDurableWork(t *testing.T) {
	fake := &fakeSupplierAPI{purchase: supplierPurchaseResponse{
		Purchased: 1, Requested: 1, Keys: []supplierKey{{Key: "ksk_repaired_legacy_event"}},
	}}
	h, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	provider = updated

	eventID := "abababababababababababababababab"
	manager.store.state.Events[supplierEventKey(provider.ID, eventID)] = supplierEventRecord{
		ProviderID: provider.ID, EventID: eventID, Event: "new_keys_available", ReceivedAt: supplierNow().Unix(),
	}
	if err := manager.store.saveCandidate(manager.store.state); err != nil {
		t.Fatalf("persist legacy event: %v", err)
	}
	body := `{"event":"new_keys_available","event_id":"` + eventID + `","purchase_order_id":"ABCDEF0123456789ABCDEF0123456789","new_keys":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/"+provider.ID, strings.NewReader(body))
	req.Header.Set("X-API-Key", provider.APIToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"duplicate":true`) || !strings.Contains(rec.Body.String(), `"queued":true`) {
		t.Fatalf("legacy retry status=%d body=%s", rec.Code, rec.Body.String())
	}
	manager.processPendingIntents()
	if liveAPIKeyCount() != 1 || len(manager.store.pendingIntents()) != 0 {
		t.Fatalf("legacy repair live=%d pending=%d", liveAPIKeyCount(), len(manager.store.pendingIntents()))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 1 || fake.purchaseIDs[0] != "ABCDEF0123456789ABCDEF0123456789" {
		t.Fatalf("legacy repair purchase calls=%d ids=%#v", fake.purchaseCalls, fake.purchaseIDs)
	}
}

func TestAWSMyWebhookSetupUsesOnlyThePermanentProviderPath(t *testing.T) {
	fake := &fakeSupplierAPI{}
	h, _, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	provider = updated

	invalid := httptest.NewRecorder()
	h.apiSetupSupplierWebhook(invalid, httptest.NewRequest(http.MethodPost, "/admin/api/suppliers/vendor-a/webhook/setup", strings.NewReader(`{"webhookUrl":"https://kiro.example/wrong"}`)), provider.ID)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid webhook setup status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	webhookURL := "https://kiro.example/api/supplier-webhooks/" + provider.ID
	valid := httptest.NewRecorder()
	h.apiSetupSupplierWebhook(valid, httptest.NewRequest(http.MethodPost, "/admin/api/suppliers/vendor-a/webhook/setup", strings.NewReader(`{"webhookUrl":"`+webhookURL+`"}`)), provider.ID)
	if valid.Code != http.StatusOK || !strings.Contains(valid.Body.String(), `"success":true`) {
		t.Fatalf("valid webhook setup status=%d body=%s", valid.Code, valid.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.setWebhookURL != webhookURL || fake.webhookTests != 1 {
		t.Fatalf("webhook setup url=%q tests=%d", fake.setWebhookURL, fake.webhookTests)
	}
}

func TestKiroDropWebhookSetupPersistsSigningSecretBeforeTesting(t *testing.T) {
	const secret = "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
	fake := &fakeSupplierAPI{webhookSecret: secret}
	h, _, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeKiroDrop
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	webhookURL := "https://kiro.example/api/supplier-webhooks/" + updated.ID
	rec := httptest.NewRecorder()
	h.apiSetupSupplierWebhook(rec, httptest.NewRequest(http.MethodPost, "/admin/api/suppliers/vendor-a/webhook/setup", strings.NewReader(`{"webhookUrl":"`+webhookURL+`"}`)), updated.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored := config.GetSupplierProvider(updated.ID)
	if stored == nil || stored.WebhookSecret != secret {
		t.Fatalf("stored provider = %+v", stored)
	}
	overview := httptest.NewRecorder()
	h.apiGetSupplierOverview(overview, httptest.NewRequest(http.MethodGet, "/admin/api/suppliers/overview", nil))
	if strings.Contains(overview.Body.String(), secret) || !strings.Contains(overview.Body.String(), `"hasWebhookSecret":true`) {
		t.Fatalf("overview leaked or omitted signing-secret state: %s", overview.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.setWebhookURL != webhookURL || fake.webhookTests != 1 {
		t.Fatalf("webhook url=%q tests=%d", fake.setWebhookURL, fake.webhookTests)
	}
}

func signedKiroDropWebhookRequest(path, body, secret string, timestamp int64) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	var envelope struct {
		EventID string `json:"event_id"`
	}
	_ = json.Unmarshal([]byte(body), &envelope)
	timestampText := fmt.Sprintf("%d", timestamp)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestampText + "." + body))
	req.Header.Set("X-Kiro-Event-Id", envelope.EventID)
	req.Header.Set("X-Kiro-Timestamp", timestampText)
	req.Header.Set("X-Kiro-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func TestKiroDropWebhookVerifiesSignatureAndAcceptsSingleAndDualEvents(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fake := &fakeSupplierAPI{}
	h, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeKiroDrop
	provider.WebhookSecret = secret
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	provider = updated
	originalNow := supplierNow
	defer func() { supplierNow = originalNow }()
	supplierNow = func() time.Time { return time.Unix(1_800_000_000, 0) }
	path := "/api/supplier-webhooks/" + provider.ID

	testBody := `{"event":"test","event_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","message":"Webhook test"}`
	testRec := httptest.NewRecorder()
	h.ServeHTTP(testRec, signedKiroDropWebhookRequest(path, testBody, secret, supplierNow().Unix()))
	if testRec.Code != http.StatusOK || strings.TrimSpace(testRec.Body.String()) != `{"ok":"true"}` {
		t.Fatalf("test status=%d body=%s", testRec.Code, testRec.Body.String())
	}
	if len(manager.store.state.Events) != 0 || len(manager.wake) != 0 {
		t.Fatalf("test event changed state: events=%d wake=%d", len(manager.store.state.Events), len(manager.wake))
	}

	single := `{"event":"new_keys_available","event_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","purchase_order_id":"11111111111111111111111111111111","order_id":"batch_us_1","region":"us-east-1","new_keys":10}`
	singleRec := httptest.NewRecorder()
	h.ServeHTTP(singleRec, signedKiroDropWebhookRequest(path, single, secret, supplierNow().Unix()))
	if singleRec.Code != http.StatusOK || !strings.Contains(singleRec.Body.String(), `"queued":true`) {
		t.Fatalf("single status=%d body=%s", singleRec.Code, singleRec.Body.String())
	}

	dual := `{"event":"new_keys_available","event_id":"cccccccccccccccccccccccccccccccc","dispatch_id":"monitor_auto_1","region":"dual","regions":["us-east-1","eu-central-1"],"new_keys":22,"new_keys_by_region":{"us-east-1":10,"eu-central-1":12},"batch_ids_by_region":{"us-east-1":["batch_us_1"],"eu-central-1":["batch_eu_1"]},"purchase_order_ids_by_region":{"us-east-1":"22222222222222222222222222222222","eu-central-1":"33333333333333333333333333333333"}}`
	dualRec := httptest.NewRecorder()
	h.ServeHTTP(dualRec, signedKiroDropWebhookRequest(path, dual, secret, supplierNow().Unix()))
	if dualRec.Code != http.StatusOK || !strings.Contains(dualRec.Body.String(), `"accepted":true`) {
		t.Fatalf("dual status=%d body=%s", dualRec.Code, dualRec.Body.String())
	}
	if len(manager.store.state.Events) != 2 {
		t.Fatalf("stored events=%d, want 2", len(manager.store.state.Events))
	}

	unsigned := httptest.NewRecorder()
	h.ServeHTTP(unsigned, httptest.NewRequest(http.MethodPost, path, strings.NewReader(single)))
	if unsigned.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned status=%d body=%s", unsigned.Code, unsigned.Body.String())
	}
	staleBody := strings.Replace(single, strings.Repeat("b", 32), strings.Repeat("d", 32), 1)
	stale := httptest.NewRecorder()
	h.ServeHTTP(stale, signedKiroDropWebhookRequest(path, staleBody, secret, supplierNow().Unix()-301))
	if stale.Code != http.StatusUnauthorized {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
	tamperedBody := strings.Replace(single, strings.Repeat("b", 32), strings.Repeat("e", 32), 1)
	tampered := signedKiroDropWebhookRequest(path, tamperedBody, secret, supplierNow().Unix())
	tampered.Header.Set("X-Kiro-Event-Id", strings.Repeat("f", 32))
	tamperedRec := httptest.NewRecorder()
	h.ServeHTTP(tamperedRec, tampered)
	if tamperedRec.Code != http.StatusUnauthorized {
		t.Fatalf("mismatched event ID status=%d body=%s", tamperedRec.Code, tamperedRec.Body.String())
	}
}

func TestKiroDropUsesLocalImportedKeysWithoutRemoteHistoryAPI(t *testing.T) {
	fake := &fakeSupplierAPI{
		stock:   supplierStock{Stock: 2, StockUS: 2, Balance: 100},
		keysErr: errSupplierOperationUnsupported,
		purchase: supplierPurchaseResponse{
			Purchased: 1, Requested: 1, OrderID: "drop-order", Region: "us-east-1", Status: "completed", Remaining: "70.00",
			Keys: []supplierKey{{Key: "ksk_drop_local", Region: "us-east-1"}},
		},
	}
	h, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeKiroDrop
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	provider = updated
	if _, err := manager.startPurchase(provider, 1, "us", true, "manual"); err != nil {
		t.Fatalf("startPurchase: %v", err)
	}
	fake.mu.Lock()
	fake.purchase = supplierPurchaseResponse{
		Purchased: 1, Requested: 1, OrderID: "drop-order-copy", Region: "us-east-1", Status: "completed", Remaining: "40.00",
		Keys: []supplierKey{{Key: "ksk_drop_copy_only", Region: "us-east-1"}},
	}
	fake.mu.Unlock()
	if _, err := manager.startPurchase(provider, 1, "us", false, "manual"); err != nil {
		t.Fatalf("copy-only startPurchase: %v", err)
	}
	if _, err := manager.refreshProviderStatus(provider, true); err != nil {
		t.Fatalf("refreshProviderStatus: %v", err)
	}
	status := manager.store.providerStatuses()[provider.ID]
	if status.KeyCount != 2 {
		t.Fatalf("local key count=%d", status.KeyCount)
	}
	rec := httptest.NewRecorder()
	h.apiGetSupplierKeys(rec, httptest.NewRequest(http.MethodGet, "/admin/api/suppliers/vendor-a/keys?page=1&page_size=50", nil), provider.ID)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ksk_drop_local") || !strings.Contains(rec.Body.String(), "ksk_drop_copy_only") {
		t.Fatalf("local keys status=%d body=%s", rec.Code, rec.Body.String())
	}
	overview := httptest.NewRecorder()
	h.apiGetSupplierOverview(overview, httptest.NewRequest(http.MethodGet, "/admin/api/suppliers/overview", nil))
	if overview.Code != http.StatusOK || strings.Contains(overview.Body.String(), "ksk_drop_local") || strings.Contains(overview.Body.String(), "ksk_drop_copy_only") {
		t.Fatalf("overview exposed persisted purchase keys: %s", overview.Body.String())
	}
}

func TestKiroAppWebhookPreservesLegacyZeroPoolInventoryWake(t *testing.T) {
	fake := &fakeSupplierAPI{purchase: supplierPurchaseResponse{
		Purchased: 1, Requested: 1, OrderID: "batch-original", Keys: []supplierKey{{Key: "ksk_original_webhook"}},
	}, stock: supplierStock{Stock: 1, StockUS: 1}}
	h, manager, provider := newSupplierTestManager(t, fake)
	body := `{"event":"new_keys_available","event_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","new_keys":1,"order_id":"batch-original","purchase_order_id":"fedcba9876543210fedcba9876543210"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/"+provider.ID, strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(manager.store.pendingIntents()) != 0 || len(manager.wake) != 1 {
		t.Fatalf("legacy webhook pending=%d wake=%d", len(manager.store.pendingIntents()), len(manager.wake))
	}
	manager.maybeAutoPurchase(provider.ID)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 1 || fake.purchaseIDs[0] == "fedcba9876543210fedcba9876543210" || fake.purchaseOrders[0] != "" || fake.purchaseCounts[0] != 1 {
		t.Fatalf("legacy KiroApp webhook changed purchase semantics: ids=%#v orders=%#v counts=%#v", fake.purchaseIDs, fake.purchaseOrders, fake.purchaseCounts)
	}
}

func TestPublicPoolWebhookWakesFreshPublicExtractionInsteadOfUsingOwnerOrder(t *testing.T) {
	fake := &fakeSupplierAPI{
		publicStock: supplierPublicStock{Total: 1, Batches: []supplierPublicBatch{{BatchID: "public-notified-batch", Available: 1}}},
		purchase: supplierPurchaseResponse{
			BatchID: "public-notified-batch", Purchased: 1, Requested: 1,
			Keys: []supplierKey{{Key: "ksk_from_public_notification"}},
		},
	}
	h, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	provider.PurchaseSource = config.SupplierPurchaseSourcePublic
	provider.AutoPurchaseCount = 1
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	provider = updated

	ownerOrderID := "33333333333333333333333333333333"
	body := `{"event":"new_keys_available","event_id":"abababababababababababababababab","purchase_order_id":"` + ownerOrderID + `","batch_id":"public-notified-batch","new_keys":1}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/"+provider.ID, strings.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"queued":true`) {
		t.Fatalf("public webhook status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(manager.store.pendingIntents()) != 0 || len(manager.wake) != 1 {
		t.Fatalf("public webhook pending=%d wake=%d", len(manager.store.pendingIntents()), len(manager.wake))
	}

	manager.maybeAutoPurchase(provider.ID)
	if liveAPIKeyCount() != 1 {
		t.Fatalf("public notification import live=%d", liveAPIKeyCount())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 1 || fake.purchaseIDs[0] == ownerOrderID || fake.purchaseSources[0] != config.SupplierPurchaseSourcePublic ||
		fake.purchaseBatches[0] != "public-notified-batch" {
		t.Fatalf("public notification ids=%#v sources=%#v batches=%#v", fake.purchaseIDs, fake.purchaseSources, fake.purchaseBatches)
	}
}

func TestWebhookPurchaseIsIndependentOfZeroPoolAutoReplenishmentSwitch(t *testing.T) {
	fake := &fakeSupplierAPI{purchase: supplierPurchaseResponse{
		Purchased: 1, Requested: 1, Keys: []supplierKey{{Key: "ksk_after_auto_enabled"}},
	}}
	h, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	provider = updated
	if err := config.UpdateSupplierFeature(true, false); err != nil {
		t.Fatalf("disable automatic replenishment: %v", err)
	}
	body := `{"event":"new_keys_available","event_id":"dddddddddddddddddddddddddddddddd","purchase_order_id":"11111111111111111111111111111111","new_keys":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/"+provider.ID, strings.NewReader(body))
	req.Header.Set("X-API-Key", provider.APIToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status=%d body=%s", rec.Code, rec.Body.String())
	}
	manager.processPendingIntents()
	if liveAPIKeyCount() != 1 || len(manager.store.pendingIntents()) != 0 {
		t.Fatalf("exact webhook purchase live=%d pending=%d", liveAPIKeyCount(), len(manager.store.pendingIntents()))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.purchaseCalls != 1 {
		t.Fatalf("automatic replenishment switch suppressed exact webhook extraction: calls=%d", fake.purchaseCalls)
	}
}

func TestWebhookPurchaseObeysMasterSwitchBeforeFirstAttempt(t *testing.T) {
	fake := &fakeSupplierAPI{purchase: supplierPurchaseResponse{
		Purchased: 1, Requested: 1, Keys: []supplierKey{{Key: "ksk_after_master_enabled"}},
	}}
	h, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	provider = updated
	body := `{"event":"new_keys_available","event_id":"99999999999999999999999999999999","purchase_order_id":"22222222222222222222222222222222","new_keys":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/"+provider.ID, strings.NewReader(body))
	req.Header.Set("X-API-Key", provider.APIToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := config.UpdateSupplierFeature(false, false); err != nil {
		t.Fatalf("disable supplier feature: %v", err)
	}
	manager.processPendingIntents()
	fake.mu.Lock()
	callsWhileDisabled := fake.purchaseCalls
	fake.mu.Unlock()
	if callsWhileDisabled != 0 || len(manager.store.pendingIntents()) != 1 {
		t.Fatalf("disabled master calls=%d pending=%d", callsWhileDisabled, len(manager.store.pendingIntents()))
	}
	if err := config.UpdateSupplierFeature(true, false); err != nil {
		t.Fatalf("enable supplier feature: %v", err)
	}
	manager.processPendingIntents()
	if liveAPIKeyCount() != 1 || len(manager.store.pendingIntents()) != 0 {
		t.Fatalf("re-enabled exact extraction live=%d pending=%d", liveAPIKeyCount(), len(manager.store.pendingIntents()))
	}
}

func TestAWSMyAllKeysDeadIsDurableAndDisablesMatchingAccounts(t *testing.T) {
	originalNow := supplierNow
	current := time.Unix(10_000, 0)
	supplierNow = func() time.Time { return current }
	defer func() { supplierNow = originalNow }()

	fake := &fakeSupplierAPI{keysErr: errors.New("history temporarily unavailable")}
	h, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	provider = updated
	if added, _, err := config.AddAccounts([]config.Account{
		{ID: "dead-one", AuthMethod: "api_key", KiroApiKey: "ksk_dead_one", AccessToken: "ksk_dead_one", SupplierID: provider.ID, SupplierBatchID: "batch-dead", Enabled: true},
		{ID: "dead-two", AuthMethod: "api_key", KiroApiKey: "ksk_dead_two", AccessToken: "ksk_dead_two", SupplierID: provider.ID, SupplierBatchID: "batch-dead", Enabled: true},
		{ID: "unrelated", AuthMethod: "api_key", KiroApiKey: "ksk_unrelated", AccessToken: "ksk_unrelated", SupplierID: "other", Enabled: true},
	}); err != nil || added != 3 {
		t.Fatalf("AddAccounts = %d, %v", added, err)
	}
	if err := manager.store.upsertBatch(supplierBatch{
		ID: "batch-dead", ProviderID: provider.ID, ClientOrderID: "batch-dead", Region: "us", Trigger: "webhook",
		Purchased: 2, Imported: 2, AccountIDs: []string{"dead-one", "dead-two"}, Status: "active", ActiveCount: 2, CreatedAt: current.Unix() - 100,
	}); err != nil {
		t.Fatalf("upsertBatch: %v", err)
	}

	body := `{"event":"all_keys_dead","event_id":"cccccccccccccccccccccccccccccccc","message":"all dead","dead":2}`
	req := httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/"+provider.ID, strings.NewReader(body))
	req.Header.Set("X-API-Key", provider.APIToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("all_keys_dead status=%d body=%s", rec.Code, rec.Body.String())
	}
	manager.processPendingWebhookEvents()
	if len(manager.store.pendingWebhookEvents()) != 1 {
		t.Fatalf("failed history sync was not kept durable: %+v", manager.store.pendingWebhookEvents())
	}

	fake.mu.Lock()
	fake.keysErr = nil
	fake.keys = supplierKeysPage{
		Items: []supplierKey{{Key: "ksk_dead_one", Status: "dead"}, {Key: "ksk_dead_two", Status: "dead"}, {Key: "ksk_unrelated", Status: "dead"}},
		Total: 3, Page: 1, PageSize: 500, Pages: 1,
	}
	fake.mu.Unlock()
	current = current.Add(6 * time.Second)
	manager.processPendingWebhookEvents()
	if len(manager.store.pendingWebhookEvents()) != 0 {
		t.Fatalf("successful dead-key sync remained pending: %+v", manager.store.pendingWebhookEvents())
	}
	byID := make(map[string]config.Account)
	for _, account := range config.GetAccounts() {
		byID[account.ID] = account
	}
	if byID["dead-one"].Enabled || byID["dead-two"].Enabled || !byID["unrelated"].Enabled {
		t.Fatalf("dead-key sync accounts = %+v", byID)
	}
	batches := manager.store.batches()
	if len(batches) != 1 || batches[0].Status != "dead" || batches[0].ActiveCount != 0 || batches[0].LifetimeSeconds <= 0 {
		t.Fatalf("dead-key batch lifecycle = %+v", batches)
	}
}

func TestKiroAppAllKeysDeadKeepsLegacyNotificationOnlyBehavior(t *testing.T) {
	fake := &fakeSupplierAPI{keys: supplierKeysPage{
		Items: []supplierKey{{Key: "ksk_legacy_stays_enabled", Status: "dead"}}, Total: 1, Pages: 1,
	}}
	h, manager, provider := newSupplierTestManager(t, fake)
	if added, _, err := config.AddAccounts([]config.Account{{
		ID: "legacy-key", AuthMethod: "api_key", KiroApiKey: "ksk_legacy_stays_enabled", AccessToken: "ksk_legacy_stays_enabled",
		SupplierID: provider.ID, Enabled: true,
	}}); err != nil || added != 1 {
		t.Fatalf("AddAccounts = %d, %v", added, err)
	}
	body := `{"event":"all_keys_dead","event_id":"legacy-event-id","message":"legacy notification","dead":1}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/"+provider.ID, strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy all_keys_dead status=%d body=%s", rec.Code, rec.Body.String())
	}
	manager.processPendingWebhookEvents()
	if len(manager.store.pendingWebhookEvents()) != 0 {
		t.Fatalf("legacy notification unexpectedly queued reconciliation: %+v", manager.store.pendingWebhookEvents())
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 || !accounts[0].Enabled {
		t.Fatalf("legacy notification changed accounts: %+v", accounts)
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

func TestSupplierWebhookTestEventIsAReadOnlyHealthCheck(t *testing.T) {
	fake := &fakeSupplierAPI{}
	h, manager, _ := newSupplierTestManager(t, fake)
	call := func() *httptest.ResponseRecorder {
		body := `{"event":"webhook_test","event_id":"087f03fc29ec4ca89a26b984fe977fb9","message":"Webhook测试消息"}`
		req := httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/vendor-a", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	assertReadOnlyOK := func(label string, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"ok":"true"}` {
			t.Fatalf("%s status=%d body=%q", label, response.Code, response.Body.String())
		}
		if len(manager.store.state.Events) != 0 || len(manager.wake) != 0 {
			t.Fatalf("%s changed webhook state: events=%d wake=%d", label, len(manager.store.state.Events), len(manager.wake))
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.purchaseCalls != 0 {
			t.Fatalf("%s triggered %d purchases", label, fake.purchaseCalls)
		}
	}

	assertReadOnlyOK("enabled test", call())
	assertReadOnlyOK("repeated test", call())
	if err := config.UpdateSupplierFeature(false, true); err != nil {
		t.Fatalf("disable supplier feature: %v", err)
	}
	assertReadOnlyOK("disabled test", call())

	missingEventID := httptest.NewRecorder()
	h.ServeHTTP(missingEventID, httptest.NewRequest(http.MethodPost, "/api/supplier-webhooks/vendor-a", strings.NewReader(`{"event":"webhook_test"}`)))
	if missingEventID.Code != http.StatusBadRequest {
		t.Fatalf("missing event_id status=%d body=%s", missingEventID.Code, missingEventID.Body.String())
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
	var disabledAck map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &disabledAck); err != nil || disabledAck["ok"] != true || disabledAck["accepted"] != false {
		t.Fatalf("disabled webhook acknowledgement=%s err=%v", rec.Body.String(), err)
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
	if !strings.Contains(body, `"apiType":"kiroapp"`) || !strings.Contains(body, `"capabilities":{"balance":true`) {
		t.Fatalf("overview missing protocol capabilities: %s", body)
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
	var purchaseOrderID string
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
			purchaseOrderID, _ = body["order_id"].(string)
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
	purchased, err := api.Purchase(supplierPurchaseRequest{Count: 1, Region: "us", ClientOrderID: "0123456789abcdef0123456789abcdef", SupplierOrderID: "batch-1"})
	if err != nil || purchased.Purchased != 1 || purchaseID != "0123456789abcdef0123456789abcdef" || purchaseOrderID != "batch-1" {
		t.Fatalf("Purchase = %+v, %v id=%q order=%q", purchased, err, purchaseID, purchaseOrderID)
	}
}

func TestAWSMySupplierAPIContract(t *testing.T) {
	var purchaseBody map[string]any
	var publicPurchaseBody map[string]any
	var configuredWebhook string
	var webhookTestCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != "aws_contract" {
			t.Errorf("X-API-Key = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("unexpected Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/api/my/profile":
			json.NewEncoder(w).Encode(map[string]any{"name": "AWS商户A", "webhook_url": "https://example.com/webhook"})
		case "/api/my/stock":
			json.NewEncoder(w).Encode(map[string]any{"max": 12})
		case "/api/public/stock":
			json.NewEncoder(w).Encode(map[string]any{
				"total": 3,
				"batches": []map[string]any{
					{"batch_id": "1005117684943163392", "available": 2, "published_at": "2026-08-05 10:00:00", "last_health_check_at": "2026-08-05 10:05:00"},
					{"batch_id": "1005117779411472384", "available": 1, "published_at": "2026-08-05 11:00:00", "last_health_check_at": nil},
				},
			})
		case "/api/public/purchase-orders":
			json.NewEncoder(w).Encode([]map[string]any{{
				"client_order_id": "fedcba9876543210fedcba9876543210", "requested": 1, "purchased": 1,
				"source_ip": "203.0.113.10", "created_at": "2026-08-05 10:30:00",
			}})
		case "/api/my/keys":
			if r.URL.Query().Get("history") != "1" || r.URL.Query().Get("page") != "" || r.URL.Query().Get("page_size") != "" {
				t.Errorf("unexpected keys query: %s", r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"count": 3, "active": 2,
				"keys": []map[string]any{
					{"key": "ksk_aws_one", "status": "active", "created_at": "2026-08-04 10:30:00"},
					{"key": "ksk_aws_two", "status": "active", "created_at": "2026-08-04 10:31:00"},
					{"key": "ksk_aws_dead", "status": "dead", "created_at": "2026-08-03 09:20:00"},
				},
			})
		case "/api/my/purchase":
			if err := json.NewDecoder(r.Body).Decode(&purchaseBody); err != nil {
				t.Errorf("decode purchase: %v", err)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"client_order_id": "0123456789abcdef0123456789abcdef",
				"purchased":       2,
				"keys":            []map[string]any{{"key": "ksk_new_one"}, {"key": "ksk_new_two"}},
			})
		case "/api/public/purchase":
			if err := json.NewDecoder(r.Body).Decode(&publicPurchaseBody); err != nil {
				t.Errorf("decode public purchase: %v", err)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"batch_id":        "1005117684943163392",
				"client_order_id": "fedcba9876543210fedcba9876543210",
				"purchased":       1,
				"keys":            []map[string]any{{"key": "ksk_public_contract"}},
			})
		case "/api/my/webhook":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode webhook setup: %v", err)
			}
			configuredWebhook = body["webhook_url"]
			json.NewEncoder(w).Encode(map[string]any{"name": "AWS商户A", "webhook_url": configuredWebhook})
		case "/api/my/webhook/test":
			webhookTestCalls++
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.Error(w, fmt.Sprintf("unexpected path %s", r.URL.Path), http.StatusNotFound)
		}
	}))
	defer server.Close()

	api := newHTTPSupplierAPI(config.SupplierProvider{
		BaseURL: server.URL, APIToken: "aws_contract", APIType: config.SupplierAPITypeAWSMy,
	})
	profile, err := api.GetProfile()
	if err != nil || profile.User.Name != "AWS商户A" || profile.User.Balance != 0 {
		t.Fatalf("GetProfile = %+v, %v", profile, err)
	}
	stock, err := api.GetStock()
	if err != nil || stock.Stock != 12 || stock.StockUS != 12 || stock.StockEU != 0 {
		t.Fatalf("GetStock = %+v, %v", stock, err)
	}
	publicStock, err := api.GetPublicStock()
	if err != nil || publicStock.Total != 3 || len(publicStock.Batches) != 2 || publicStock.Batches[1].Available != 1 {
		t.Fatalf("GetPublicStock = %+v, %v", publicStock, err)
	}
	publicOrders, err := api.GetPublicPurchaseOrders()
	if err != nil || len(publicOrders) != 1 || publicOrders[0].Purchased != 1 || publicOrders[0].SourceIP != "203.0.113.10" {
		t.Fatalf("GetPublicPurchaseOrders = %+v, %v", publicOrders, err)
	}
	keys, err := api.GetKeys(true, 2, 2)
	if err != nil || keys.Total != 3 || keys.Pages != 2 || len(keys.Items) != 1 || keys.Items[0].Value() != "ksk_aws_dead" {
		t.Fatalf("GetKeys = %+v, %v", keys, err)
	}
	purchased, err := api.Purchase(supplierPurchaseRequest{
		Count: 2, Region: "us", ClientOrderID: "0123456789abcdef0123456789abcdef", SupplierOrderID: "must-not-be-sent",
	})
	if err != nil || purchased.Purchased != 2 || purchased.Requested != 2 || len(purchased.Keys) != 2 {
		t.Fatalf("Purchase = %+v, %v", purchased, err)
	}
	if purchaseBody["client_order_id"] != "0123456789abcdef0123456789abcdef" || purchaseBody["count"] != float64(2) {
		t.Fatalf("purchase body = %#v", purchaseBody)
	}
	if _, sent := purchaseBody["region"]; sent {
		t.Fatalf("AWS My purchase sent unsupported region: %#v", purchaseBody)
	}
	if _, sent := purchaseBody["order_id"]; sent {
		t.Fatalf("AWS My purchase sent unsupported order_id: %#v", purchaseBody)
	}
	publicPurchased, err := api.Purchase(supplierPurchaseRequest{
		Count: 1, Region: "us", PurchaseSource: config.SupplierPurchaseSourcePublic,
		BatchID: "1005117684943163392", ClientOrderID: "fedcba9876543210fedcba9876543210",
	})
	if err != nil || publicPurchased.Purchased != 1 || publicPurchased.BatchID != "1005117684943163392" {
		t.Fatalf("public Purchase = %+v, %v", publicPurchased, err)
	}
	if publicPurchaseBody["batch_id"] != "1005117684943163392" || publicPurchaseBody["count"] != float64(1) ||
		publicPurchaseBody["client_order_id"] != "fedcba9876543210fedcba9876543210" {
		t.Fatalf("public purchase body = %#v", publicPurchaseBody)
	}
	if _, err := api.SetWebhook("https://kiro.example/api/supplier-webhooks/aws-vendor"); err != nil {
		t.Fatalf("SetWebhook: %v", err)
	}
	if err := api.TestWebhook(); err != nil {
		t.Fatalf("TestWebhook: %v", err)
	}
	if configuredWebhook != "https://kiro.example/api/supplier-webhooks/aws-vendor" || webhookTestCalls != 1 {
		t.Fatalf("webhook setup url=%q testCalls=%d", configuredWebhook, webhookTestCalls)
	}
}

func TestAWSMyPurchaseRejectsMalformedSuccessfulResponses(t *testing.T) {
	request := supplierPurchaseRequest{Count: 2, Region: "us", ClientOrderID: "0123456789abcdef0123456789abcdef"}
	tests := []struct {
		name     string
		response map[string]any
	}{
		{
			name: "wrong order id",
			response: map[string]any{"client_order_id": "fedcba9876543210fedcba9876543210", "purchased": 2,
				"keys": []map[string]string{{"key": "ksk_one"}, {"key": "ksk_two"}}},
		},
		{
			name: "partial delivery",
			response: map[string]any{"client_order_id": request.ClientOrderID, "purchased": 1,
				"keys": []map[string]string{{"key": "ksk_one"}}},
		},
		{
			name: "empty key",
			response: map[string]any{"client_order_id": request.ClientOrderID, "purchased": 2,
				"keys": []map[string]string{{"key": "ksk_one"}, {"key": ""}}},
		},
		{
			name: "duplicate key",
			response: map[string]any{"client_order_id": request.ClientOrderID, "purchased": 2,
				"keys": []map[string]string{{"key": "ksk_same"}, {"key": "ksk_same"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				json.NewEncoder(w).Encode(tt.response)
			}))
			defer server.Close()
			api := newHTTPSupplierAPI(config.SupplierProvider{
				BaseURL: server.URL, APIToken: "secret", APIType: config.SupplierAPITypeAWSMy,
			})
			if _, err := api.Purchase(request); err == nil {
				t.Fatalf("malformed response was accepted: %#v", tt.response)
			}
		})
	}
}

func TestAWSPublicAPIRejectsMalformedInventoryAndOrderHistory(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		response any
		call     func(supplierAPI) error
	}{
		{
			name: "stock total mismatch", path: "/api/public/stock",
			response: map[string]any{"total": 2, "batches": []map[string]any{{"batch_id": "batch-one", "available": 1}}},
			call:     func(api supplierAPI) error { _, err := api.GetPublicStock(); return err },
		},
		{
			name: "duplicate batch", path: "/api/public/stock",
			response: map[string]any{"total": 2, "batches": []map[string]any{{"batch_id": "same", "available": 1}, {"batch_id": "same", "available": 1}}},
			call:     func(api supplierAPI) error { _, err := api.GetPublicStock(); return err },
		},
		{
			name: "invalid order id", path: "/api/public/purchase-orders",
			response: []map[string]any{{"client_order_id": "not-hex", "requested": 1, "purchased": 1}},
			call:     func(api supplierAPI) error { _, err := api.GetPublicPurchaseOrders(); return err },
		},
		{
			name: "partial order", path: "/api/public/purchase-orders",
			response: []map[string]any{{"client_order_id": "0123456789abcdef0123456789abcdef", "requested": 2, "purchased": 1}},
			call:     func(api supplierAPI) error { _, err := api.GetPublicPurchaseOrders(); return err },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.path {
					t.Fatalf("path=%s, want %s", r.URL.Path, tt.path)
				}
				json.NewEncoder(w).Encode(tt.response)
			}))
			defer server.Close()
			api := newHTTPSupplierAPI(config.SupplierProvider{BaseURL: server.URL, APIToken: "secret", APIType: config.SupplierAPITypeAWSMy})
			if err := tt.call(api); err == nil {
				t.Fatalf("malformed response was accepted: %#v", tt.response)
			}
		})
	}
}

func TestAWSMyWebhookManagementRequiresPositiveConfirmation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/my/webhook":
			json.NewEncoder(w).Encode(map[string]any{"webhook_url": "https://wrong.example/api/supplier-webhooks/vendor"})
		case "/api/my/webhook/test":
			json.NewEncoder(w).Encode(map[string]any{"ok": false})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api := newHTTPSupplierAPI(config.SupplierProvider{BaseURL: server.URL, APIToken: "secret", APIType: config.SupplierAPITypeAWSMy})
	if _, err := api.SetWebhook("https://kiro.example/api/supplier-webhooks/vendor"); err == nil {
		t.Fatal("mismatched webhook confirmation was accepted")
	}
	if err := api.TestWebhook(); err == nil {
		t.Fatal("ok=false webhook test was accepted")
	}
}

func TestAWSMySupplierErrorCodeControlsRetrySafety(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"code": "ORDER_PROCESSING", "message": "订单正在处理"})
	}))
	defer server.Close()
	api := newHTTPSupplierAPI(config.SupplierProvider{BaseURL: server.URL, APIToken: "secret", APIType: config.SupplierAPITypeAWSMy})
	_, err := api.GetStock()
	if err == nil || !isRetryableSupplierError(err) || !strings.Contains(err.Error(), "ORDER_PROCESSING") {
		t.Fatalf("ORDER_PROCESSING classification = %v", err)
	}
}

func TestAWSPublicStockChangedCodeIsRecognizedWithDocumentedFieldCasing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"Code": "STOCK_CHANGED", "Message": "库存发生变化"})
	}))
	defer server.Close()
	api := newHTTPSupplierAPI(config.SupplierProvider{BaseURL: server.URL, APIToken: "secret", APIType: config.SupplierAPITypeAWSMy})
	_, err := api.Purchase(supplierPurchaseRequest{
		Count: 1, PurchaseSource: config.SupplierPurchaseSourcePublic, BatchID: "batch",
		ClientOrderID: "0123456789abcdef0123456789abcdef",
	})
	if err == nil || !isSupplierPublicInventoryRaceError(err) || isRetryableSupplierError(err) || !strings.Contains(err.Error(), "STOCK_CHANGED") {
		t.Fatalf("STOCK_CHANGED classification = %v", err)
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

func TestPublicSupplierConnectionCheckCoversStockKeysAndOrderHistory(t *testing.T) {
	fake := &fakeSupplierAPI{
		publicStock: supplierPublicStock{Total: 1, Batches: []supplierPublicBatch{{BatchID: "public-health", Available: 1}}},
		keys:        supplierKeysPage{Total: 4},
		publicOrders: []supplierPublicPurchaseOrder{{
			ClientOrderID: "0123456789abcdef0123456789abcdef", Requested: 1, Purchased: 1,
		}},
	}
	_, manager, provider := newSupplierTestManager(t, fake)
	provider.APIType = config.SupplierAPITypeAWSMy
	provider.PurchaseSource = config.SupplierPurchaseSourcePublic
	updated, err := config.UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}

	stock, err := manager.refreshProviderStatus(updated, true)
	if err != nil || stock.Stock != 1 || len(stock.Batches) != 1 {
		t.Fatalf("public connection check = %+v, %v", stock, err)
	}
	status := manager.store.providerStatuses()[provider.ID]
	if status.PurchaseSource != config.SupplierPurchaseSourcePublic || status.KeyCount != 4 || status.PublicOrderCount != 1 ||
		len(status.PublicBatches) != 1 || status.PublicBatches[0].BatchID != "public-health" {
		t.Fatalf("public connection status = %+v", status)
	}

	fake.mu.Lock()
	fake.publicOrdersErr = errors.New("public order endpoint unavailable")
	fake.mu.Unlock()
	if _, err := manager.refreshProviderStatus(updated, true); err == nil || !strings.Contains(err.Error(), "public order endpoint unavailable") {
		t.Fatalf("missing public order access was accepted: %v", err)
	}
	status = manager.store.providerStatuses()[provider.ID]
	if !strings.Contains(status.LastError, "public order endpoint unavailable") {
		t.Fatalf("public order error not visible in status: %+v", status)
	}
}
