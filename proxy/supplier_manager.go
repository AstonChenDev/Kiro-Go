package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	supplierPendingRetryBase = 5 * time.Second
	supplierImportedMaxSSE   = 300
	supplierImportedMaxRPM   = 200
	supplierPurchaseRegionUS = "us"
)

type supplierWake struct {
	ProviderID string
}

type supplierWebhookWindow struct {
	StartedAt time.Time
	Count     int
}

type supplierPurchaseOutcome struct {
	Intent   supplierPurchaseIntent   `json:"intent"`
	Response supplierPurchaseResponse `json:"response"`
	Batch    supplierBatch            `json:"batch"`
	Pending  bool                     `json:"pending"`
}

type supplierManager struct {
	handler      *Handler
	store        *supplierStateStore
	apiFactory   func(config.SupplierProvider) supplierAPI
	wake         chan supplierWake
	stop         <-chan struct{}
	workflowMu   sync.Mutex
	purchaseMu   sync.Mutex
	webhookMu    sync.Mutex
	webhooks     map[string]supplierWebhookWindow
	autoBlockMu  sync.RWMutex
	autoBlocks   map[string]supplierAutoBlock
	pollInterval func() time.Duration
}

func newSupplierManager(handler *Handler, stop <-chan struct{}) (*supplierManager, error) {
	store, err := newSupplierStateStore(config.GetConfigDir())
	if err != nil {
		return nil, err
	}
	return &supplierManager{
		handler:      handler,
		store:        store,
		apiFactory:   newHTTPSupplierAPI,
		wake:         make(chan supplierWake, 1),
		stop:         stop,
		webhooks:     make(map[string]supplierWebhookWindow),
		autoBlocks:   store.autoBlocks(),
		pollInterval: currentSupplierPollInterval,
	}, nil
}

func currentSupplierPollInterval() time.Duration {
	seconds := config.GetSupplierIntegration().PollIntervalSeconds
	return time.Duration(seconds) * time.Second
}

func (m *supplierManager) currentPollInterval() time.Duration {
	if m.pollInterval != nil {
		if interval := m.pollInterval(); interval > 0 {
			return interval
		}
	}
	return time.Duration(config.DefaultSupplierPollIntervalSeconds) * time.Second
}

func resetSupplierPollTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

func (m *supplierManager) automaticPurchaseBlock(providerID string) (supplierAutoBlock, bool) {
	m.autoBlockMu.RLock()
	defer m.autoBlockMu.RUnlock()
	block, blocked := m.autoBlocks[providerID]
	return block, blocked
}

func (m *supplierManager) automaticPurchaseBlocks() map[string]supplierAutoBlock {
	m.autoBlockMu.RLock()
	defer m.autoBlockMu.RUnlock()
	blocks := make(map[string]supplierAutoBlock, len(m.autoBlocks))
	for providerID, block := range m.autoBlocks {
		blocks[providerID] = block
	}
	return blocks
}

func (m *supplierManager) autoPurchaseBlocked(providerID string) bool {
	_, blocked := m.automaticPurchaseBlock(providerID)
	return blocked
}

func (m *supplierManager) blockAutomaticPurchases(providerID, batchID, reason string) error {
	block := supplierAutoBlock{ProviderID: providerID, BatchID: batchID, Reason: reason, BlockedAt: supplierNow().Unix()}
	// Set the in-memory circuit breaker first so even a disk error cannot cause
	// another charge on the next polling tick in this process.
	m.autoBlockMu.Lock()
	if m.autoBlocks == nil {
		m.autoBlocks = make(map[string]supplierAutoBlock)
	}
	m.autoBlocks[providerID] = block
	m.autoBlockMu.Unlock()
	return m.store.setAutoBlock(block)
}

func (m *supplierManager) clearAutomaticPurchaseBlock(providerID string) error {
	if err := m.store.clearAutoBlock(providerID); err != nil {
		return err
	}
	m.autoBlockMu.Lock()
	delete(m.autoBlocks, providerID)
	m.autoBlockMu.Unlock()
	return nil
}

func (m *supplierManager) allowWebhook(providerID string) bool {
	const maxPerMinute = 120
	now := supplierNow()
	m.webhookMu.Lock()
	defer m.webhookMu.Unlock()
	if m.webhooks == nil {
		m.webhooks = make(map[string]supplierWebhookWindow)
	}
	window := m.webhooks[providerID]
	if window.StartedAt.IsZero() || now.Sub(window.StartedAt) >= time.Minute {
		m.webhooks[providerID] = supplierWebhookWindow{StartedAt: now, Count: 1}
		return true
	}
	if window.Count >= maxPerMinute {
		return false
	}
	window.Count++
	m.webhooks[providerID] = window
	return true
}

func (m *supplierManager) run() {
	timer := time.NewTimer(m.currentPollInterval())
	defer timer.Stop()
	// Resolve any response lost after an idempotent purchase before creating a
	// new purchase. This is safe even when automatic purchasing is disabled.
	m.processPendingWebhookEvents()
	m.processPendingIntents()
	for {
		select {
		case wake := <-m.wake:
			m.processPendingWebhookEvents()
			m.processPendingIntents()
			m.maybeAutoPurchase(wake.ProviderID)
			resetSupplierPollTimer(timer, m.currentPollInterval())
		case <-timer.C:
			m.processPendingWebhookEvents()
			m.processPendingIntents()
			m.reconcileBatches()
			m.maybeAutoPurchase("")
			timer.Reset(m.currentPollInterval())
		case <-m.stop:
			return
		}
	}
}

func (m *supplierManager) signal(providerID string) {
	select {
	case m.wake <- supplierWake{ProviderID: providerID}:
	default:
		// A wake is already queued. The worker always re-checks all authoritative
		// conditions, so coalescing bursts is correct and prevents webhook storms.
	}
}

func liveAPIKeyCount() int {
	count := 0
	for _, account := range config.GetAccounts() {
		if account.Enabled && config.IsAPIKeyAccount(&account) {
			count++
		}
	}
	return count
}

func (m *supplierManager) maybeAutoPurchase(preferredProviderID string) {
	integration := config.GetSupplierIntegration()
	if !integration.Enabled || !integration.AutoPurchaseEnabled || liveAPIKeyCount() != 0 {
		return
	}
	if len(m.store.pendingIntents()) != 0 {
		return
	}
	providers := config.SortedEnabledSupplierProviders(preferredProviderID)
	for _, provider := range providers {
		if m.autoPurchaseBlocked(provider.ID) {
			continue
		}
		stock, err := m.refreshProviderStatus(provider, false)
		if err != nil {
			logger.Warnf("[Supplier] stock check failed for %s: %v", provider.ID, err)
			if !isRetryableSupplierError(err) {
				if blockErr := m.blockAutomaticPurchases(provider.ID, "", "connection_rejected"); blockErr != nil {
					logger.Warnf("[Supplier] automatic purchase safety block for %s could not be persisted: %v", provider.ID, blockErr)
				}
			}
			continue
		}
		if liveAPIKeyCount() != 0 {
			return
		}
		if stock.StockUS <= 0 {
			continue
		}
		count := provider.AutoPurchaseCount
		if count < 1 {
			count = config.DefaultSupplierPurchaseCount
		}
		if count > stock.StockUS {
			count = stock.StockUS
		}
		outcome, err := m.startPurchase(provider, count, supplierPurchaseRegionUS, true, "auto")
		if err != nil {
			logger.Warnf("[Supplier] automatic purchase failed for %s: %v", provider.ID, err)
			if !outcome.Pending && !isRetryableSupplierError(err) {
				if blockErr := m.blockAutomaticPurchases(provider.ID, outcome.Intent.ID, "purchase_rejected"); blockErr != nil {
					logger.Warnf("[Supplier] automatic purchase safety block for %s could not be persisted: %v", provider.ID, blockErr)
				}
				continue
			}
			return
		}
		if outcome.Pending {
			logger.Warnf("[Supplier] automatic purchase for %s is pending idempotent retry", provider.ID)
			return
		}
		if outcome.Response.Purchased > 0 {
			if outcome.Batch.Imported == 0 {
				if blockErr := m.blockAutomaticPurchases(provider.ID, outcome.Batch.ID, "no_importable_keys"); blockErr != nil {
					logger.Warnf("[Supplier] automatic purchase safety block for %s could not be persisted: %v", provider.ID, blockErr)
				}
				logger.Warnf("[Supplier] automatic purchases paused for %s: purchase returned no new importable keys", provider.ID)
				continue
			}
			logger.Infof("[Supplier] automatically purchased %d US key(s) from %s and imported %d", outcome.Response.Purchased, provider.ID, outcome.Batch.Imported)
			return
		}
	}
}

func (m *supplierManager) refreshProviderStatus(provider config.SupplierProvider, includeKeyCount bool) (supplierStock, error) {
	api := m.apiFactory(provider)
	stock, err := api.GetStock()
	var keyCountErr error
	status := supplierProviderStatus{
		ProviderID: provider.ID,
		CheckedAt:  supplierNow().Unix(),
	}
	if err != nil {
		if previous, ok := m.store.providerStatuses()[provider.ID]; ok {
			status = previous
			status.CheckedAt = supplierNow().Unix()
		}
		status.LastError = err.Error()
		_ = m.store.setProviderStatus(status)
		return supplierStock{}, err
	}
	status.Stock = stock.Stock
	status.StockUS = stock.StockUS
	status.StockEU = stock.StockEU
	status.PriceMin = stock.PriceMin
	if status.PriceMin == 0 {
		status.PriceMin = stock.Price
	}
	status.PriceMax = stock.PriceMax
	if status.PriceMax == 0 {
		status.PriceMax = status.PriceMin
	}
	status.Balance = stock.Balance
	if includeKeyCount {
		keys, keyErr := api.GetKeys(false, 1, 1)
		if keyErr != nil {
			keyCountErr = keyErr
			status.LastError = "stock available; key count failed: " + keyErr.Error()
		} else {
			status.KeyCount = keys.Total
		}
	}
	if saveErr := m.store.setProviderStatus(status); saveErr != nil {
		return supplierStock{}, fmt.Errorf("persist supplier status: %w", saveErr)
	}
	if keyCountErr != nil {
		return stock, fmt.Errorf("read supplier keys: %w", keyCountErr)
	}
	return stock, nil
}

func (m *supplierManager) refreshAllProviderStatuses() map[string]string {
	integration := config.GetSupplierIntegration()
	errorsByProvider := make(map[string]string)
	var errorsMu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, provider := range integration.Providers {
		if !provider.Enabled || strings.TrimSpace(provider.APIToken) == "" {
			continue
		}
		provider := provider
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if _, err := m.refreshProviderStatus(provider, true); err != nil {
				errorsMu.Lock()
				errorsByProvider[provider.ID] = err.Error()
				errorsMu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errorsByProvider
}

func newSupplierClientOrderID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func isSupplier32HexID(value string) bool {
	if len(value) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16
}

func (m *supplierManager) startPurchase(provider config.SupplierProvider, count int, region string, autoImport bool, trigger string) (supplierPurchaseOutcome, error) {
	m.workflowMu.Lock()
	defer m.workflowMu.Unlock()
	if len(m.store.pendingIntents()) != 0 {
		return supplierPurchaseOutcome{}, errors.New("another supplier purchase is awaiting an idempotent retry")
	}
	if count < 1 || count > config.MaxSupplierPurchaseCount {
		return supplierPurchaseOutcome{}, fmt.Errorf("count must be between 1 and %d", config.MaxSupplierPurchaseCount)
	}
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		region = supplierPurchaseRegionUS
	}
	if region != "us" && region != "eu" {
		return supplierPurchaseOutcome{}, errors.New("region must be us or eu")
	}
	if config.EffectiveSupplierAPIType(provider.APIType) == config.SupplierAPITypeAWSMy && region != supplierPurchaseRegionUS {
		return supplierPurchaseOutcome{}, errors.New("this supplier exposes a regionless US-preferred key pool")
	}
	clientOrderID, err := newSupplierClientOrderID()
	if err != nil {
		return supplierPurchaseOutcome{}, fmt.Errorf("generate client order id: %w", err)
	}
	now := supplierNow().Unix()
	intent := supplierPurchaseIntent{
		ID:            clientOrderID,
		ProviderID:    provider.ID,
		Region:        region,
		Count:         count,
		ClientOrderID: clientOrderID,
		AutoImport:    autoImport,
		Trigger:       trigger,
		Status:        "pending",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := m.store.createIntent(intent); err != nil {
		return supplierPurchaseOutcome{}, fmt.Errorf("persist purchase intent: %w", err)
	}
	return m.executeIntent(intent)
}

func (m *supplierManager) executeIntent(intent supplierPurchaseIntent) (supplierPurchaseOutcome, error) {
	m.purchaseMu.Lock()
	defer m.purchaseMu.Unlock()

	provider := config.GetSupplierProvider(intent.ProviderID)
	if provider == nil || strings.TrimSpace(provider.APIToken) == "" {
		intent.Status = "failed"
		intent.LastError = "supplier is missing or has no API token"
		intent.UpdatedAt = supplierNow().Unix()
		_ = m.store.updateIntent(intent)
		return supplierPurchaseOutcome{Intent: intent}, errors.New(intent.LastError)
	}
	intent.Attempts++
	// Persist this before the POST. If the process loses the response, replaying
	// the same idempotency key is required even when an operator subsequently
	// disables the feature or provider.
	intent.MustResolve = true
	intent.UpdatedAt = supplierNow().Unix()
	if err := m.store.updateIntent(intent); err != nil {
		return supplierPurchaseOutcome{}, fmt.Errorf("persist purchase attempt: %w", err)
	}

	response, err := m.apiFactory(*provider).Purchase(supplierPurchaseRequest{
		Count:           intent.Count,
		Region:          intent.Region,
		ClientOrderID:   intent.ClientOrderID,
		SupplierOrderID: intent.SupplierOrderID,
	})
	if err != nil {
		intent.LastError = err.Error()
		intent.MustResolve = isRetryableSupplierError(err)
		intent.UpdatedAt = supplierNow().Unix()
		if !isRetryableSupplierError(err) && intent.Trigger != "webhook" {
			intent.Status = "failed"
		}
		if persistErr := m.store.updateIntent(intent); persistErr != nil {
			return supplierPurchaseOutcome{Intent: intent, Pending: intent.Status == "pending"}, fmt.Errorf("%v; persist failure: %w", err, persistErr)
		}
		return supplierPurchaseOutcome{Intent: intent, Pending: intent.Status == "pending"}, err
	}

	batch := supplierBatch{
		ID:              intent.ID,
		ProviderID:      intent.ProviderID,
		SupplierOrderID: response.OrderID,
		ClientOrderID:   intent.ClientOrderID,
		Region:          intent.Region,
		Trigger:         intent.Trigger,
		Requested:       response.Requested,
		Purchased:       response.Purchased,
		TotalDebit:      response.TotalDebit,
		UnitPrice:       response.UnitPrice,
		Replayed:        response.Replayed,
		CreatedAt:       intent.CreatedAt,
		Status:          "not_imported",
	}
	if batch.Requested == 0 {
		batch.Requested = intent.Count
	}
	if intent.AutoImport && response.Purchased > 0 {
		accountIDs, imported, skipped, importErr := m.importKeys(*provider, intent, response.Keys)
		if importErr != nil {
			// Keep the intent pending. Replaying the supplier order returns identical
			// keys, and account-level fingerprint dedup makes another import safe.
			intent.LastError = "purchase succeeded but import failed: " + importErr.Error()
			intent.UpdatedAt = supplierNow().Unix()
			if persistErr := m.store.updateIntent(intent); persistErr != nil {
				return supplierPurchaseOutcome{Intent: intent, Response: response, Batch: batch, Pending: true}, fmt.Errorf("%v; persist failure: %w", importErr, persistErr)
			}
			return supplierPurchaseOutcome{Intent: intent, Response: response, Batch: batch, Pending: true}, importErr
		}
		batch.AccountIDs = accountIDs
		batch.Imported = imported
		batch.Skipped = skipped
		batch.ActiveCount = len(accountIDs)
		if len(accountIDs) > 0 {
			batch.Status = "active"
		}
	}
	if err := m.store.upsertBatch(batch); err != nil {
		return supplierPurchaseOutcome{Intent: intent, Response: response, Batch: batch, Pending: true}, fmt.Errorf("persist supplier batch: %w", err)
	}
	intent.Status = "complete"
	intent.MustResolve = false
	intent.LastError = ""
	intent.UpdatedAt = supplierNow().Unix()
	if err := m.store.updateIntent(intent); err != nil {
		return supplierPurchaseOutcome{Intent: intent, Response: response, Batch: batch, Pending: true}, fmt.Errorf("complete purchase intent: %w", err)
	}
	block, blocked := m.automaticPurchaseBlock(provider.ID)
	shouldClearBlock := batch.Imported > 0 || (blocked && block.Reason != "no_importable_keys" && response.Purchased > 0)
	if shouldClearBlock {
		if err := m.clearAutomaticPurchaseBlock(provider.ID); err != nil {
			logger.Warnf("[Supplier] successful recovery but could not clear automatic purchase safety block for %s: %v", provider.ID, err)
		}
	}
	return supplierPurchaseOutcome{Intent: intent, Response: response, Batch: batch}, nil
}

func (m *supplierManager) importKeys(provider config.SupplierProvider, intent supplierPurchaseIntent, keys []supplierKey) ([]string, int, int, error) {
	region := "us-east-1"
	if intent.Region == "eu" {
		region = "eu-central-1"
	}
	accounts := make([]config.Account, 0, len(keys))
	for _, item := range keys {
		key := item.Value()
		if key == "" {
			continue
		}
		accounts = append(accounts, config.Account{
			ID:              auth.GenerateAccountID(),
			Nickname:        provider.Name + " / " + intent.ID[:8],
			KiroApiKey:      key,
			AccessToken:     key,
			AuthMethod:      "api_key",
			Provider:        "APIKey",
			SupplierID:      provider.ID,
			SupplierBatchID: intent.ID,
			Region:          region,
			Enabled:         true,
			MaxSSE:          supplierImportedMaxSSE,
			MaxRPM:          supplierImportedMaxRPM,
		})
	}
	if len(accounts) == 0 {
		return nil, 0, len(keys), nil
	}
	_, _, err := config.AddAccounts(accounts)
	if err != nil {
		return nil, 0, 0, err
	}
	m.handler.pool.Reload()
	allAccounts := config.GetAccounts()
	accountIDs := make([]string, 0, len(accounts))
	for _, account := range allAccounts {
		if account.SupplierID == provider.ID && account.SupplierBatchID == intent.ID {
			accountIDs = append(accountIDs, account.ID)
		}
	}
	sort.Strings(accountIDs)
	imported := len(accountIDs)
	skipped := len(accounts) - imported
	if skipped < 0 {
		skipped = 0
	}
	return accountIDs, imported, skipped, nil
}

func newSupplierWebhookPurchaseIntent(provider config.SupplierProvider, event supplierWebhookEvent) (supplierPurchaseIntent, error) {
	// The supplier requires this value to be reused byte-for-byte as the API
	// client_order_id. Trim surrounding JSON whitespace, but never normalize the
	// hexadecimal digits themselves.
	clientOrderID := strings.TrimSpace(event.PurchaseOrderID)
	if !isSupplier32HexID(clientOrderID) {
		return supplierPurchaseIntent{}, errors.New("purchase_order_id must be a 32-character hexadecimal string")
	}
	if event.NewKeys < 1 {
		return supplierPurchaseIntent{}, errors.New("new_keys must be greater than zero")
	}
	supplierOrderID := strings.TrimSpace(event.OrderID)
	if len(supplierOrderID) > 256 {
		return supplierPurchaseIntent{}, errors.New("order_id is too long")
	}
	if config.EffectiveSupplierAPIType(provider.APIType) == config.SupplierAPITypeKiroApp && supplierOrderID == "" {
		return supplierPurchaseIntent{}, errors.New("order_id is required for KiroApp webhook purchases")
	}
	now := supplierNow().Unix()
	return supplierPurchaseIntent{
		ID:              clientOrderID,
		ProviderID:      provider.ID,
		Region:          supplierPurchaseRegionUS,
		Count:           event.NewKeys,
		ClientOrderID:   clientOrderID,
		SupplierOrderID: supplierOrderID,
		AutoImport:      true,
		Trigger:         "webhook",
		Status:          "pending",
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

func supplierRetryBackoff(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	shift := attempts - 1
	if shift > 6 {
		shift = 6
	}
	backoff := supplierPendingRetryBase * time.Duration(1<<shift)
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	return backoff
}

func (m *supplierManager) processPendingWebhookEvents() {
	integration := config.GetSupplierIntegration()
	if !integration.Enabled {
		return
	}
	for _, record := range m.store.pendingWebhookEvents() {
		provider := config.GetSupplierProvider(record.ProviderID)
		if provider == nil || !provider.Enabled || strings.TrimSpace(provider.APIToken) == "" {
			continue
		}
		if backoff := supplierRetryBackoff(record.Attempts); backoff > 0 &&
			supplierNow().Unix()-record.UpdatedAt < int64(backoff/time.Second) {
			continue
		}
		record.Attempts++
		record.UpdatedAt = supplierNow().Unix()
		if err := m.store.updateWebhookEvent(record); err != nil {
			logger.Warnf("[Supplier] persist webhook event attempt %s/%s: %v", record.ProviderID, record.EventID, err)
			continue
		}

		var err error
		switch record.Event {
		case "all_keys_dead":
			err = m.syncProviderDeadKeys(*provider, record.Payload.Dead)
		default:
			err = fmt.Errorf("unsupported pending webhook event %q", record.Event)
		}
		if err != nil {
			record.LastError = err.Error()
			record.UpdatedAt = supplierNow().Unix()
			if persistErr := m.store.updateWebhookEvent(record); persistErr != nil {
				logger.Warnf("[Supplier] persist webhook event failure %s/%s: %v", record.ProviderID, record.EventID, persistErr)
			}
			logger.Warnf("[Supplier] webhook event %s/%s processing failed: %v", record.ProviderID, record.EventID, err)
			continue
		}
		record.Status = "complete"
		record.LastError = ""
		record.UpdatedAt = supplierNow().Unix()
		if err := m.store.updateWebhookEvent(record); err != nil {
			logger.Warnf("[Supplier] complete webhook event %s/%s: %v", record.ProviderID, record.EventID, err)
		}
	}
}

func (m *supplierManager) syncProviderDeadKeys(provider config.SupplierProvider, reportedDead int) error {
	api := m.apiFactory(provider)
	first, err := api.GetKeys(true, 1, 100000)
	if err != nil {
		return fmt.Errorf("load supplier key history: %w", err)
	}
	items := append([]supplierKey(nil), first.Items...)
	pages := first.Pages
	if pages < 1 {
		pages = 1
	}
	if pages > 200 {
		return errors.New("supplier key history exceeds the safe 100,000-key reconciliation limit")
	}
	for page := 2; page <= pages; page++ {
		next, pageErr := api.GetKeys(true, page, 100000)
		if pageErr != nil {
			return fmt.Errorf("load supplier key history page %d: %w", page, pageErr)
		}
		items = append(items, next.Items...)
	}
	deadKeys := make([]string, 0)
	for _, item := range items {
		status := strings.ToLower(strings.TrimSpace(item.Status))
		switch status {
		case "dead", "expired", "revoked", "inactive", "disabled", "invalid":
			if key := item.Value(); key != "" {
				deadKeys = append(deadKeys, key)
			}
		}
	}
	if reportedDead > 0 && len(deadKeys) < reportedDead {
		return fmt.Errorf("supplier key history exposes %d of %d reported dead keys", len(deadKeys), reportedDead)
	}
	disabled, err := config.DisableSupplierAPIKeyAccounts(provider.ID, deadKeys, "supplier reported all_keys_dead")
	if err != nil {
		return fmt.Errorf("disable supplier-dead accounts: %w", err)
	}
	if disabled > 0 {
		m.handler.pool.Reload()
		logger.Infof("[Supplier] disabled %d dead key account(s) reported by %s", disabled, provider.ID)
	}
	m.reconcileBatches()
	return nil
}

func (m *supplierManager) processPendingIntents() {
	m.workflowMu.Lock()
	defer m.workflowMu.Unlock()
	integration := config.GetSupplierIntegration()
	for _, intent := range m.store.pendingIntents() {
		// An AWS My callback is an exact extraction instruction, independent of
		// zero-pool polling. The master integration/provider switches still control
		// whether a new instruction may start. Once an API attempt was sent, retries
		// must continue with the same idempotency key to resolve ambiguous outcomes.
		if intent.Trigger == "webhook" && intent.Attempts == 0 && !integration.Enabled {
			continue
		}
		provider := config.GetSupplierProvider(intent.ProviderID)
		if !intent.MustResolve && (!integration.Enabled || provider == nil || !provider.Enabled) {
			continue
		}
		if intent.Attempts == 0 && (provider == nil || !provider.Enabled) {
			continue
		}
		if backoff := supplierRetryBackoff(intent.Attempts); backoff > 0 &&
			supplierNow().Unix()-intent.UpdatedAt < int64(backoff/time.Second) {
			continue
		}
		if _, err := m.executeIntent(intent); err != nil {
			logger.Warnf("[Supplier] pending purchase %s retry failed: %v", intent.ID, err)
		}
	}
}

func (m *supplierManager) reconcileBatches() {
	if err := m.store.reconcileBatches(config.GetAccounts()); err != nil {
		logger.Warnf("[Supplier] reconcile batch lifetimes failed: %v", err)
	}
}
