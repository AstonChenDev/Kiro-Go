package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	supplierStateFilename = "supplier_state.json"
	maxSupplierEvents     = 5000
	maxSupplierBatches    = 2000
	maxSupplierIntents    = 3000
)

type supplierWebhookEvent struct {
	Event                    string              `json:"event"`
	EventID                  string              `json:"event_id"`
	DispatchID               string              `json:"dispatch_id,omitempty"`
	BatchID                  string              `json:"batch_id,omitempty"`
	Visibility               string              `json:"visibility,omitempty"`
	NewKeys                  int                 `json:"new_keys,omitempty"`
	SuppliedCount            int                 `json:"supplied_count,omitempty"`
	OrderID                  string              `json:"order_id,omitempty"`
	PurchaseOrderID          string              `json:"purchase_order_id,omitempty"`
	Region                   string              `json:"region,omitempty"`
	Regions                  []string            `json:"regions,omitempty"`
	NewKeysByRegion          map[string]int      `json:"new_keys_by_region,omitempty"`
	BatchIDsByRegion         map[string][]string `json:"batch_ids_by_region,omitempty"`
	PurchaseOrderIDsByRegion map[string]string   `json:"purchase_order_ids_by_region,omitempty"`
	CreatedAt                string              `json:"created_at,omitempty"`
	MotherID                 string              `json:"mother_id,omitempty"`
	Message                  string              `json:"message,omitempty"`
	FinishedAt               string              `json:"finished_at,omitempty"`
	StockUS                  int                 `json:"stock_us,omitempty"`
	StockEU                  int                 `json:"stock_eu,omitempty"`
	PriceUS                  float64             `json:"price_us,omitempty"`
	PriceEU                  float64             `json:"price_eu,omitempty"`
	Dead                     int                 `json:"dead,omitempty"`
}

type supplierEventRecord struct {
	ProviderID string               `json:"providerId"`
	EventID    string               `json:"eventId"`
	Event      string               `json:"event"`
	Payload    supplierWebhookEvent `json:"payload,omitempty"`
	Status     string               `json:"status,omitempty"` // pending, complete
	Attempts   int                  `json:"attempts,omitempty"`
	LastError  string               `json:"lastError,omitempty"`
	ReceivedAt int64                `json:"receivedAt"`
	UpdatedAt  int64                `json:"updatedAt,omitempty"`
}

type supplierProviderStatus struct {
	ProviderID       string                `json:"providerId"`
	PurchaseSource   string                `json:"purchaseSource"`
	CheckedAt        int64                 `json:"checkedAt"`
	Stock            int                   `json:"stock"`
	StockUS          int                   `json:"stockUs"`
	StockEU          int                   `json:"stockEu"`
	PriceMin         float64               `json:"priceMin"`
	PriceMax         float64               `json:"priceMax"`
	Balance          float64               `json:"balance"`
	KeyCount         int                   `json:"keyCount"`
	PublicOrderCount int                   `json:"publicOrderCount,omitempty"`
	PublicBatches    []supplierPublicBatch `json:"publicBatches,omitempty"`
	LastError        string                `json:"lastError,omitempty"`
}

type supplierPurchaseIntent struct {
	ID              string `json:"id"`
	ProviderID      string `json:"providerId"`
	PurchaseSource  string `json:"purchaseSource,omitempty"`
	PublicBatchID   string `json:"publicBatchId,omitempty"`
	Region          string `json:"region"`
	Count           int    `json:"count"`
	ClientOrderID   string `json:"clientOrderId"`
	SupplierOrderID string `json:"supplierOrderId,omitempty"`
	AutoImport      bool   `json:"autoImport"`
	Trigger         string `json:"trigger"`
	Status          string `json:"status"` // pending, complete, failed
	Attempts        int    `json:"attempts"`
	MustResolve     bool   `json:"mustResolve,omitempty"`
	LastError       string `json:"lastError,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

// supplierAutoBlock is a durable circuit breaker. A supplier that reports a
// successful automatic purchase without yielding any new importable key must
// not be charged again every five seconds. Non-retryable supplier rejections
// are paused as well. A later successful manual import or explicit connection
// test clears the corresponding block safely.
type supplierAutoBlock struct {
	ProviderID string `json:"providerId"`
	BatchID    string `json:"batchId,omitempty"`
	Reason     string `json:"reason"`
	BlockedAt  int64  `json:"blockedAt"`
}

type supplierBatch struct {
	ID              string   `json:"id"`
	ProviderID      string   `json:"providerId"`
	PurchaseSource  string   `json:"purchaseSource,omitempty"`
	PublicBatchID   string   `json:"publicBatchId,omitempty"`
	SupplierOrderID string   `json:"supplierOrderId,omitempty"`
	ClientOrderID   string   `json:"clientOrderId"`
	Region          string   `json:"region"`
	Trigger         string   `json:"trigger"`
	Requested       int      `json:"requested"`
	Purchased       int      `json:"purchased"`
	Imported        int      `json:"imported"`
	Skipped         int      `json:"skipped"`
	TotalDebit      float64  `json:"totalDebit"`
	UnitPrice       float64  `json:"unitPrice"`
	AccountIDs      []string `json:"accountIds,omitempty"`
	Status          string   `json:"status"` // active, dead, ended_manual, not_imported
	ActiveCount     int      `json:"activeCount"`
	Replayed        bool     `json:"replayed"`
	CreatedAt       int64    `json:"createdAt"`
	EndedAt         int64    `json:"endedAt,omitempty"`
	LifetimeSeconds int64    `json:"lifetimeSeconds,omitempty"`
}

type supplierPersistentState struct {
	Version         int                               `json:"version"`
	Events          map[string]supplierEventRecord    `json:"events,omitempty"`
	ProviderStatus  map[string]supplierProviderStatus `json:"providerStatus,omitempty"`
	PurchaseIntents map[string]supplierPurchaseIntent `json:"purchaseIntents,omitempty"`
	AutoBlocks      map[string]supplierAutoBlock      `json:"autoBlocks,omitempty"`
	// PurchasedKeys is intentionally excluded from every overview response. It
	// retains Kiro Drop purchase results because that API has no key-history
	// endpoint, including manual purchases where automatic import was disabled.
	PurchasedKeys map[string][]supplierKey `json:"purchasedKeys,omitempty"`
	Batches       []supplierBatch          `json:"batches,omitempty"`
}

type supplierStateStore struct {
	mu    sync.RWMutex
	path  string
	state supplierPersistentState
}

func newSupplierStateStore(dir string) (*supplierStateStore, error) {
	store := &supplierStateStore{path: filepath.Join(dir, supplierStateFilename)}
	store.state = newSupplierPersistentState()
	data, err := os.ReadFile(store.path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, fmt.Errorf("read supplier state: %w", err)
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("decode supplier state: %w", err)
	}
	store.ensureMapsLocked()
	return store, nil
}

func newSupplierPersistentState() supplierPersistentState {
	return supplierPersistentState{
		Version:         4,
		Events:          make(map[string]supplierEventRecord),
		ProviderStatus:  make(map[string]supplierProviderStatus),
		PurchaseIntents: make(map[string]supplierPurchaseIntent),
		AutoBlocks:      make(map[string]supplierAutoBlock),
		PurchasedKeys:   make(map[string][]supplierKey),
		Batches:         []supplierBatch{},
	}
}

func (s *supplierStateStore) ensureMapsLocked() {
	if s.state.Version < 4 {
		s.state.Version = 4
	}
	if s.state.Events == nil {
		s.state.Events = make(map[string]supplierEventRecord)
	}
	if s.state.ProviderStatus == nil {
		s.state.ProviderStatus = make(map[string]supplierProviderStatus)
	}
	if s.state.PurchaseIntents == nil {
		s.state.PurchaseIntents = make(map[string]supplierPurchaseIntent)
	}
	if s.state.AutoBlocks == nil {
		s.state.AutoBlocks = make(map[string]supplierAutoBlock)
	}
	if s.state.PurchasedKeys == nil {
		s.state.PurchasedKeys = make(map[string][]supplierKey)
	}
	if s.state.Batches == nil {
		s.state.Batches = []supplierBatch{}
	}
}

func cloneSupplierState(in supplierPersistentState) supplierPersistentState {
	out := newSupplierPersistentState()
	out.Version = in.Version
	for k, v := range in.Events {
		out.Events[k] = v
	}
	for k, v := range in.ProviderStatus {
		v.PublicBatches = append([]supplierPublicBatch(nil), v.PublicBatches...)
		out.ProviderStatus[k] = v
	}
	for k, v := range in.PurchaseIntents {
		out.PurchaseIntents[k] = v
	}
	for k, v := range in.AutoBlocks {
		out.AutoBlocks[k] = v
	}
	for k, v := range in.PurchasedKeys {
		out.PurchasedKeys[k] = append([]supplierKey(nil), v...)
	}
	out.Batches = make([]supplierBatch, len(in.Batches))
	for i := range in.Batches {
		out.Batches[i] = in.Batches[i]
		out.Batches[i].AccountIDs = append([]string(nil), in.Batches[i].AccountIDs...)
	}
	return out
}

func (s *supplierStateStore) saveCandidate(candidate supplierPersistentState) error {
	data, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *supplierStateStore) mutate(fn func(*supplierPersistentState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := cloneSupplierState(s.state)
	if err := fn(&candidate); err != nil {
		return err
	}
	if err := s.saveCandidate(candidate); err != nil {
		return err
	}
	s.state = candidate
	return nil
}

func supplierEventKey(providerID, eventID string) string {
	return providerID + ":" + eventID
}

func supplierIntentKey(providerID, intentID string) string {
	return providerID + ":" + intentID
}

func findSupplierIntent(intents map[string]supplierPurchaseIntent, providerID, intentID string) (string, supplierPurchaseIntent, bool) {
	key := supplierIntentKey(providerID, intentID)
	if intent, ok := intents[key]; ok {
		return key, intent, true
	}
	// Version-1 state used the bare client order ID as the map key. Continue to
	// update those records in place while all newly written entries are scoped.
	if intent, ok := intents[intentID]; ok && intent.ProviderID == providerID {
		return intentID, intent, true
	}
	return key, supplierPurchaseIntent{}, false
}

// recordEvent returns false for an already-processed event. The record is
// persisted before the caller acknowledges the webhook, so retries after a
// process restart remain harmless.
func (s *supplierStateStore) recordEvent(providerID string, event supplierWebhookEvent) (bool, error) {
	added, _, err := s.recordEventAndIntent(providerID, event, nil, false)
	return added, err
}

// recordEventAndIntent atomically records a webhook and its exact supplier
// purchase intent. This closes the crash window where an acknowledged callback
// could otherwise be durable while the corresponding extraction order was not.
// The second return value reports newly queued work. It also repairs version-1
// event records that were persisted before durable webhook work existed.
// pendingEvent is used for non-purchase work such as all_keys_dead syncing.
func (s *supplierStateStore) recordEventAndIntent(providerID string, event supplierWebhookEvent, intent *supplierPurchaseIntent, pendingEvent bool) (bool, bool, error) {
	key := supplierEventKey(providerID, event.EventID)
	added := false
	workAdded := false
	err := s.mutate(func(candidate *supplierPersistentState) error {
		existingEvent, duplicate := candidate.Events[key]
		if intent != nil {
			storageKey, existing, duplicate := findSupplierIntent(candidate.PurchaseIntents, intent.ProviderID, intent.ID)
			if duplicate {
				if existing.ProviderID != intent.ProviderID || existing.ClientOrderID != intent.ClientOrderID ||
					existing.Count != intent.Count || existing.SupplierOrderID != intent.SupplierOrderID ||
					config.EffectiveSupplierPurchaseSource(existing.PurchaseSource) != config.EffectiveSupplierPurchaseSource(intent.PurchaseSource) ||
					existing.PublicBatchID != intent.PublicBatchID {
					return errors.New("purchase_order_id conflicts with an existing supplier purchase")
				}
			} else {
				candidate.PurchaseIntents[storageKey] = *intent
				workAdded = true
				trimSupplierIntents(candidate.PurchaseIntents)
			}
		}
		if duplicate {
			// Version-1 records have no status or payload. A supplier retry after
			// upgrading must repair the missing durable work instead of being
			// discarded merely because its event ID was already observed.
			if existingEvent.Status == "" && (workAdded || pendingEvent) {
				existingEvent.Payload = event
				existingEvent.UpdatedAt = supplierNow().Unix()
				if pendingEvent {
					existingEvent.Status = "pending"
					workAdded = true
				} else {
					existingEvent.Status = "complete"
				}
				candidate.Events[key] = existingEvent
			}
			return nil
		}
		now := supplierNow().Unix()
		status := "complete"
		if pendingEvent {
			status = "pending"
			workAdded = true
		}
		candidate.Events[key] = supplierEventRecord{
			ProviderID: providerID,
			EventID:    event.EventID,
			Event:      event.Event,
			Payload:    event,
			Status:     status,
			ReceivedAt: now,
			UpdatedAt:  now,
		}
		added = true
		trimSupplierEvents(candidate.Events)
		return nil
	})
	return added, workAdded, err
}

func (s *supplierStateStore) pendingWebhookEvents() []supplierEventRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]supplierEventRecord, 0)
	for _, event := range s.state.Events {
		if event.Status == "pending" {
			items = append(items, event)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ReceivedAt < items[j].ReceivedAt })
	return items
}

func (s *supplierStateStore) updateWebhookEvent(event supplierEventRecord) error {
	key := supplierEventKey(event.ProviderID, event.EventID)
	return s.mutate(func(candidate *supplierPersistentState) error {
		if _, exists := candidate.Events[key]; !exists {
			return errors.New("supplier webhook event not found")
		}
		candidate.Events[key] = event
		return nil
	})
}

func trimSupplierEvents(events map[string]supplierEventRecord) {
	if len(events) <= maxSupplierEvents {
		return
	}
	type eventKeyTime struct {
		key string
		ts  int64
	}
	ordered := make([]eventKeyTime, 0, len(events))
	for key, event := range events {
		if event.Status == "pending" {
			continue
		}
		ordered = append(ordered, eventKeyTime{key: key, ts: event.ReceivedAt})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ts < ordered[j].ts })
	remove := len(events) - maxSupplierEvents
	for i := 0; i < len(ordered) && i < remove; i++ {
		delete(events, ordered[i].key)
	}
}

func (s *supplierStateStore) setProviderStatus(status supplierProviderStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, exists := s.state.ProviderStatus[status.ProviderID]
	// Status is operational telemetry, not a financial record. Keep every check
	// visible in memory but persist an unchanged snapshot at most once a minute
	// to avoid a disk write every five seconds while the pool is empty.
	unchanged := exists &&
		previous.PurchaseSource == status.PurchaseSource && supplierPublicBatchesEqual(previous.PublicBatches, status.PublicBatches) &&
		previous.Stock == status.Stock && previous.StockUS == status.StockUS && previous.StockEU == status.StockEU &&
		previous.PriceMin == status.PriceMin && previous.PriceMax == status.PriceMax && previous.Balance == status.Balance &&
		previous.KeyCount == status.KeyCount && previous.PublicOrderCount == status.PublicOrderCount && previous.LastError == status.LastError
	if unchanged && status.CheckedAt-previous.CheckedAt < 60 {
		status.PublicBatches = append([]supplierPublicBatch(nil), status.PublicBatches...)
		s.state.ProviderStatus[status.ProviderID] = status
		return nil
	}
	candidate := cloneSupplierState(s.state)
	status.PublicBatches = append([]supplierPublicBatch(nil), status.PublicBatches...)
	candidate.ProviderStatus[status.ProviderID] = status
	if err := s.saveCandidate(candidate); err != nil {
		return err
	}
	s.state = candidate
	return nil
}

func (s *supplierStateStore) providerStatuses() map[string]supplierProviderStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]supplierProviderStatus, len(s.state.ProviderStatus))
	for k, v := range s.state.ProviderStatus {
		v.PublicBatches = append([]supplierPublicBatch(nil), v.PublicBatches...)
		out[k] = v
	}
	return out
}

func supplierPublicBatchesEqual(left, right []supplierPublicBatch) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].BatchID != right[i].BatchID || left[i].Available != right[i].Available ||
			left[i].PublishedAt != right[i].PublishedAt {
			return false
		}
		if (left[i].LastHealthCheckAt == nil) != (right[i].LastHealthCheckAt == nil) {
			return false
		}
		if left[i].LastHealthCheckAt != nil && *left[i].LastHealthCheckAt != *right[i].LastHealthCheckAt {
			return false
		}
	}
	return true
}

func (s *supplierStateStore) createIntent(intent supplierPurchaseIntent) error {
	return s.mutate(func(candidate *supplierPersistentState) error {
		key, _, exists := findSupplierIntent(candidate.PurchaseIntents, intent.ProviderID, intent.ID)
		if exists {
			return errors.New("purchase intent already exists")
		}
		candidate.PurchaseIntents[key] = intent
		trimSupplierIntents(candidate.PurchaseIntents)
		return nil
	})
}

func trimSupplierIntents(intents map[string]supplierPurchaseIntent) {
	if len(intents) <= maxSupplierIntents {
		return
	}
	type keyedIntent struct {
		key    string
		intent supplierPurchaseIntent
	}
	completed := make([]keyedIntent, 0, len(intents))
	for key, intent := range intents {
		if intent.Status != "pending" {
			completed = append(completed, keyedIntent{key: key, intent: intent})
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].intent.UpdatedAt < completed[j].intent.UpdatedAt })
	remove := len(intents) - maxSupplierIntents
	for i := 0; i < len(completed) && i < remove; i++ {
		delete(intents, completed[i].key)
	}
}

func (s *supplierStateStore) updateIntent(intent supplierPurchaseIntent) error {
	return s.mutate(func(candidate *supplierPersistentState) error {
		key, _, exists := findSupplierIntent(candidate.PurchaseIntents, intent.ProviderID, intent.ID)
		if !exists {
			return errors.New("purchase intent not found")
		}
		candidate.PurchaseIntents[key] = intent
		return nil
	})
}

func (s *supplierStateStore) pendingIntents() []supplierPurchaseIntent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]supplierPurchaseIntent, 0)
	for _, intent := range s.state.PurchaseIntents {
		if intent.Status == "pending" {
			items = append(items, intent)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt < items[j].CreatedAt })
	return items
}

func (s *supplierStateStore) hasPendingIntentForProvider(providerID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, intent := range s.state.PurchaseIntents {
		if intent.ProviderID == providerID && intent.Status == "pending" {
			return true
		}
	}
	return false
}

func (s *supplierStateStore) autoBlocks() map[string]supplierAutoBlock {
	s.mu.RLock()
	defer s.mu.RUnlock()
	blocks := make(map[string]supplierAutoBlock, len(s.state.AutoBlocks))
	for providerID, block := range s.state.AutoBlocks {
		blocks[providerID] = block
	}
	return blocks
}

func (s *supplierStateStore) setAutoBlock(block supplierAutoBlock) error {
	if strings.TrimSpace(block.ProviderID) == "" {
		return errors.New("auto-purchase block requires a provider")
	}
	return s.mutate(func(candidate *supplierPersistentState) error {
		candidate.AutoBlocks[block.ProviderID] = block
		return nil
	})
}

func (s *supplierStateStore) clearAutoBlock(providerID string) error {
	s.mu.RLock()
	_, exists := s.state.AutoBlocks[providerID]
	s.mu.RUnlock()
	if !exists {
		return nil
	}
	return s.mutate(func(candidate *supplierPersistentState) error {
		delete(candidate.AutoBlocks, providerID)
		return nil
	})
}

func (s *supplierStateStore) upsertBatch(batch supplierBatch) error {
	return s.upsertBatchWithKeys(batch, nil, false)
}

func (s *supplierStateStore) upsertBatchWithKeys(batch supplierBatch, keys []supplierKey, replaceKeys bool) error {
	return s.mutate(func(candidate *supplierPersistentState) error {
		storageKey := supplierIntentKey(batch.ProviderID, batch.ID)
		if replaceKeys {
			if len(keys) == 0 {
				delete(candidate.PurchasedKeys, storageKey)
			} else {
				candidate.PurchasedKeys[storageKey] = append([]supplierKey(nil), keys...)
			}
		}
		for i := range candidate.Batches {
			if candidate.Batches[i].ID == batch.ID {
				candidate.Batches[i] = batch
				return nil
			}
		}
		candidate.Batches = append(candidate.Batches, batch)
		if len(candidate.Batches) > maxSupplierBatches {
			candidate.Batches = append([]supplierBatch(nil), candidate.Batches[len(candidate.Batches)-maxSupplierBatches:]...)
			retained := make(map[string]struct{}, len(candidate.Batches))
			for _, retainedBatch := range candidate.Batches {
				retained[supplierIntentKey(retainedBatch.ProviderID, retainedBatch.ID)] = struct{}{}
			}
			for key := range candidate.PurchasedKeys {
				if _, ok := retained[key]; !ok {
					delete(candidate.PurchasedKeys, key)
				}
			}
		}
		return nil
	})
}

func (s *supplierStateStore) purchasedKeys(providerID, batchID string) []supplierKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]supplierKey(nil), s.state.PurchasedKeys[supplierIntentKey(providerID, batchID)]...)
}

func (s *supplierStateStore) reconcileBatches(accounts []config.Account) error {
	accountMap := make(map[string]config.Account, len(accounts))
	for _, account := range accounts {
		accountMap[account.ID] = account
	}
	now := supplierNow().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := cloneSupplierState(s.state)
	changed := false
	for i := range candidate.Batches {
		batch := &candidate.Batches[i]
		if batch.Status != "active" || len(batch.AccountIDs) == 0 {
			continue
		}
		active := 0
		manualEnd := false
		for _, accountID := range batch.AccountIDs {
			account, exists := accountMap[accountID]
			if !exists {
				manualEnd = true
				continue
			}
			if account.Enabled {
				active++
				continue
			}
			reason := strings.ToLower(account.BanReason)
			if account.BanStatus != "BANNED" || strings.Contains(reason, "operator") {
				manualEnd = true
			}
		}
		if batch.ActiveCount != active {
			batch.ActiveCount = active
			changed = true
		}
		if active == 0 {
			batch.EndedAt = now
			batch.LifetimeSeconds = now - batch.CreatedAt
			if manualEnd {
				batch.Status = "ended_manual"
			} else {
				batch.Status = "dead"
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := s.saveCandidate(candidate); err != nil {
		return err
	}
	s.state = candidate
	return nil
}

func (s *supplierStateStore) batches() []supplierBatch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]supplierBatch, len(s.state.Batches))
	for i := range s.state.Batches {
		out[i] = s.state.Batches[i]
		out[i].AccountIDs = append([]string(nil), s.state.Batches[i].AccountIDs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

func supplierLifetimeSummary(batches []supplierBatch) map[string]any {
	completed := make([]int64, 0)
	for _, batch := range batches {
		if batch.Status == "dead" && batch.LifetimeSeconds > 0 {
			completed = append(completed, batch.LifetimeSeconds)
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i] < completed[j] })
	if len(completed) == 0 {
		return map[string]any{"samples": 0, "averageSeconds": int64(0), "medianSeconds": int64(0)}
	}
	var total int64
	for _, value := range completed {
		total += value
	}
	median := completed[len(completed)/2]
	if len(completed)%2 == 0 {
		median = (completed[len(completed)/2-1] + completed[len(completed)/2]) / 2
	}
	return map[string]any{
		"samples":        len(completed),
		"averageSeconds": total / int64(len(completed)),
		"medianSeconds":  median,
	}
}
