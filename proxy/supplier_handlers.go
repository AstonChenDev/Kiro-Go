package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const supplierWebhookBodyLimit int64 = 64 << 10

func maskSupplierToken(token string) string {
	token = strings.TrimSpace(token)
	if len(token) <= 10 {
		if token == "" {
			return ""
		}
		return "••••••••"
	}
	return token[:6] + "…" + token[len(token)-4:]
}

func supplierProviderCapabilities(provider config.SupplierProvider) map[string]bool {
	apiType := config.EffectiveSupplierAPIType(provider.APIType)
	return map[string]bool{
		"balance":           apiType != config.SupplierAPITypeAWSMy,
		"regions":           apiType != config.SupplierAPITypeAWSMy,
		"publicPool":        apiType == config.SupplierAPITypeAWSMy,
		"webhookManagement": apiType != config.SupplierAPITypeKiroApp,
		"remoteKeys":        apiType != config.SupplierAPITypeKiroDrop,
		"signedWebhook":     apiType == config.SupplierAPITypeKiroDrop,
		"purchaseRemaining": apiType == config.SupplierAPITypeKiroDrop,
	}
}

func (h *Handler) supplierReady(w http.ResponseWriter) bool {
	if h.suppliers != nil {
		return true
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "supplier integration is unavailable"})
	return false
}

func (h *Handler) apiGetSupplierOverview(w http.ResponseWriter, _ *http.Request) {
	if !h.supplierReady(w) {
		return
	}
	h.suppliers.reconcileBatches()
	integration := config.GetSupplierIntegration()
	statuses := h.suppliers.store.providerStatuses()
	autoBlocks := h.suppliers.automaticPurchaseBlocks()
	batches := h.suppliers.store.batches()
	accounts := config.GetAccounts()

	providerAccountCounts := make(map[string]int)
	providerAliveCounts := make(map[string]int)
	for _, account := range accounts {
		if account.SupplierID == "" {
			continue
		}
		providerAccountCounts[account.SupplierID]++
		if account.Enabled && config.IsAPIKeyAccount(&account) {
			providerAliveCounts[account.SupplierID]++
		}
	}
	providers := make([]map[string]any, 0, len(integration.Providers))
	for _, provider := range integration.Providers {
		status := statuses[provider.ID]
		autoBlock, autoBlocked := autoBlocks[provider.ID]
		providers = append(providers, map[string]any{
			"id":                  provider.ID,
			"name":                provider.Name,
			"baseUrl":             provider.BaseURL,
			"apiType":             config.EffectiveSupplierAPIType(provider.APIType),
			"purchaseSource":      config.EffectiveSupplierPurchaseSource(provider.PurchaseSource),
			"capabilities":        supplierProviderCapabilities(provider),
			"enabled":             provider.Enabled,
			"priority":            provider.Priority,
			"autoPurchaseCount":   provider.AutoPurchaseCount,
			"hasToken":            strings.TrimSpace(provider.APIToken) != "",
			"hasWebhookSecret":    strings.TrimSpace(provider.WebhookSecret) != "",
			"tokenMasked":         maskSupplierToken(provider.APIToken),
			"webhookPath":         "/api/supplier-webhooks/" + provider.ID,
			"createdAt":           provider.CreatedAt,
			"updatedAt":           provider.UpdatedAt,
			"accountCount":        providerAccountCounts[provider.ID],
			"aliveCount":          providerAliveCounts[provider.ID],
			"status":              status,
			"autoPurchaseBlocked": autoBlocked,
			"autoPurchaseBlock":   autoBlock,
		})
	}
	pending := h.suppliers.store.pendingIntents()
	json.NewEncoder(w).Encode(map[string]any{
		"enabled":             integration.Enabled,
		"autoPurchaseEnabled": integration.AutoPurchaseEnabled,
		"pollIntervalSeconds": integration.PollIntervalSeconds,
		"liveApiKeyCount":     liveAPIKeyCount(),
		"providers":           providers,
		"pendingPurchases":    len(pending),
		"batches":             batches,
		"lifetime":            supplierLifetimeSummary(batches),
		"importDefaults": map[string]int{
			"maxSSE": supplierImportedMaxSSE,
			"maxRPM": supplierImportedMaxRPM,
		},
	})
}

func (h *Handler) apiUpdateSupplierFeature(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled             bool `json:"enabled"`
		AutoPurchaseEnabled bool `json:"autoPurchaseEnabled"`
		PollIntervalSeconds *int `json:"pollIntervalSeconds"`
	}
	if err := decodeSupplierJSON(r, &body); err != nil {
		writeSupplierError(w, http.StatusBadRequest, err)
		return
	}
	pollIntervalSeconds := config.GetSupplierIntegration().PollIntervalSeconds
	if body.PollIntervalSeconds != nil {
		pollIntervalSeconds = *body.PollIntervalSeconds
	}
	if pollIntervalSeconds < config.MinSupplierPollIntervalSeconds || pollIntervalSeconds > config.MaxSupplierPollIntervalSeconds {
		writeSupplierError(w, http.StatusBadRequest, fmt.Errorf("pollIntervalSeconds must be between %d and %d", config.MinSupplierPollIntervalSeconds, config.MaxSupplierPollIntervalSeconds))
		return
	}
	if err := config.UpdateSupplierSettings(body.Enabled, body.AutoPurchaseEnabled, pollIntervalSeconds); err != nil {
		writeSupplierError(w, http.StatusInternalServerError, err)
		return
	}
	if body.Enabled && body.AutoPurchaseEnabled && h.suppliers != nil {
		h.suppliers.signal("")
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true})
}

type supplierProviderRequest struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	BaseURL           string `json:"baseUrl"`
	APIToken          string `json:"apiToken"`
	APIType           string `json:"apiType"`
	PurchaseSource    string `json:"purchaseSource"`
	Enabled           bool   `json:"enabled"`
	Priority          int    `json:"priority"`
	AutoPurchaseCount int    `json:"autoPurchaseCount"`
}

func (r supplierProviderRequest) configValue(id string) config.SupplierProvider {
	if id == "" {
		id = r.ID
	}
	return config.SupplierProvider{
		ID:                id,
		Name:              r.Name,
		BaseURL:           r.BaseURL,
		APIToken:          r.APIToken,
		APIType:           r.APIType,
		PurchaseSource:    r.PurchaseSource,
		Enabled:           r.Enabled,
		Priority:          r.Priority,
		AutoPurchaseCount: r.AutoPurchaseCount,
	}
}

func (h *Handler) apiCreateSupplier(w http.ResponseWriter, r *http.Request) {
	var body supplierProviderRequest
	if err := decodeSupplierJSON(r, &body); err != nil {
		writeSupplierError(w, http.StatusBadRequest, err)
		return
	}
	provider, err := config.AddSupplierProvider(body.configValue(""))
	if err != nil {
		writeSupplierError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"success":     true,
		"id":          provider.ID,
		"webhookPath": "/api/supplier-webhooks/" + provider.ID,
	})
}

func (h *Handler) apiUpdateSupplier(w http.ResponseWriter, r *http.Request, id string) {
	var body supplierProviderRequest
	if err := decodeSupplierJSON(r, &body); err != nil {
		writeSupplierError(w, http.StatusBadRequest, err)
		return
	}
	existing := config.GetSupplierProvider(id)
	if existing == nil {
		writeSupplierError(w, http.StatusNotFound, config.ErrSupplierNotFound)
		return
	}
	requestedAPIType := config.EffectiveSupplierAPIType(body.APIType)
	if strings.TrimSpace(body.APIType) == "" {
		requestedAPIType = config.EffectiveSupplierAPIType(existing.APIType)
	}
	requestedPurchaseSource := config.EffectiveSupplierPurchaseSource(body.PurchaseSource)
	if strings.TrimSpace(body.PurchaseSource) == "" {
		requestedPurchaseSource = config.EffectiveSupplierPurchaseSource(existing.PurchaseSource)
	}
	if h.suppliers != nil && h.suppliers.store.hasPendingIntentForProvider(existing.ID) {
		if requestedAPIType != config.EffectiveSupplierAPIType(existing.APIType) {
			writeSupplierError(w, http.StatusConflict, errors.New("cannot change supplier API protocol while a purchase is pending"))
			return
		}
		if requestedPurchaseSource != config.EffectiveSupplierPurchaseSource(existing.PurchaseSource) {
			writeSupplierError(w, http.StatusConflict, errors.New("cannot change supplier purchase source while a purchase is pending"))
			return
		}
	}
	provider, err := config.UpdateSupplierProvider(id, body.configValue(id))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, config.ErrSupplierNotFound) {
			status = http.StatusNotFound
		}
		writeSupplierError(w, status, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"success":     true,
		"id":          provider.ID,
		"webhookPath": "/api/supplier-webhooks/" + provider.ID,
	})
}

func (h *Handler) apiTestSupplier(w http.ResponseWriter, _ *http.Request, id string) {
	if !h.supplierReady(w) {
		return
	}
	provider := config.GetSupplierProvider(id)
	if provider == nil {
		writeSupplierError(w, http.StatusNotFound, config.ErrSupplierNotFound)
		return
	}
	stock, err := h.suppliers.refreshProviderStatus(*provider, true)
	if err != nil {
		writeSupplierError(w, http.StatusBadGateway, err)
		return
	}
	if block, blocked := h.suppliers.automaticPurchaseBlock(provider.ID); blocked && block.Reason != "no_importable_keys" {
		if err := h.suppliers.clearAutomaticPurchaseBlock(provider.ID); err != nil {
			writeSupplierError(w, http.StatusInternalServerError, fmt.Errorf("clear supplier safety block: %w", err))
			return
		}
		integration := config.GetSupplierIntegration()
		if integration.Enabled && integration.AutoPurchaseEnabled {
			h.suppliers.signal(provider.ID)
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "stock": stock})
}

func (h *Handler) apiSetupSupplierWebhook(w http.ResponseWriter, r *http.Request, id string) {
	if !h.supplierReady(w) {
		return
	}
	provider := config.GetSupplierProvider(id)
	if provider == nil {
		writeSupplierError(w, http.StatusNotFound, config.ErrSupplierNotFound)
		return
	}
	apiType := config.EffectiveSupplierAPIType(provider.APIType)
	if apiType != config.SupplierAPITypeAWSMy && apiType != config.SupplierAPITypeKiroDrop {
		writeSupplierError(w, http.StatusBadRequest, errSupplierOperationUnsupported)
		return
	}
	var body struct {
		WebhookURL string `json:"webhookUrl"`
	}
	if err := decodeSupplierJSON(r, &body); err != nil {
		writeSupplierError(w, http.StatusBadRequest, err)
		return
	}
	body.WebhookURL = strings.TrimSpace(body.WebhookURL)
	u, err := url.Parse(body.WebhookURL)
	expectedPath := "/api/supplier-webhooks/" + provider.ID
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.Path != expectedPath || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		writeSupplierError(w, http.StatusBadRequest, fmt.Errorf("webhookUrl must be a public absolute URL ending in %s", expectedPath))
		return
	}
	api := h.suppliers.apiFactory(*provider)
	secret, err := api.SetWebhook(body.WebhookURL)
	if err != nil {
		writeSupplierError(w, http.StatusBadGateway, fmt.Errorf("save supplier webhook: %w", err))
		return
	}
	if apiType == config.SupplierAPITypeKiroDrop {
		if err := config.UpdateSupplierWebhookSecret(provider.ID, secret); err != nil {
			writeSupplierError(w, http.StatusInternalServerError, fmt.Errorf("persist supplier webhook signing secret: %w", err))
			return
		}
	}
	if err := api.TestWebhook(); err != nil {
		writeSupplierError(w, http.StatusBadGateway, fmt.Errorf("webhook was saved but its connection test failed: %w", err))
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "webhookUrl": body.WebhookURL})
}

func (h *Handler) apiRefreshSuppliers(w http.ResponseWriter, _ *http.Request) {
	if !h.supplierReady(w) {
		return
	}
	if !config.GetSupplierIntegration().Enabled {
		writeSupplierError(w, http.StatusConflict, errors.New("supplier feature must be enabled before refreshing inventory"))
		return
	}
	errorsByProvider := h.suppliers.refreshAllProviderStatuses()
	json.NewEncoder(w).Encode(map[string]any{
		"success": len(errorsByProvider) == 0,
		"errors":  errorsByProvider,
	})
}

func (h *Handler) apiGetSupplierKeys(w http.ResponseWriter, r *http.Request, id string) {
	if !h.supplierReady(w) {
		return
	}
	integration := config.GetSupplierIntegration()
	provider := config.GetSupplierProvider(id)
	if provider == nil {
		writeSupplierError(w, http.StatusNotFound, config.ErrSupplierNotFound)
		return
	}
	if !integration.Enabled || !provider.Enabled {
		writeSupplierError(w, http.StatusConflict, errors.New("supplier feature and provider must be enabled"))
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	history := r.URL.Query().Get("history") == "1"
	var keys supplierKeysPage
	if config.EffectiveSupplierAPIType(provider.APIType) == config.SupplierAPITypeKiroDrop {
		keys = h.suppliers.localProviderKeys(provider.ID, history, page, pageSize)
	} else {
		var err error
		keys, err = h.suppliers.apiFactory(*provider).GetKeys(history, page, pageSize)
		if err != nil {
			writeSupplierError(w, http.StatusBadGateway, err)
			return
		}
	}
	for i := range keys.Items {
		if keys.Items[i].Key == "" {
			keys.Items[i].Key = keys.Items[i].KeyValue
		}
		keys.Items[i].KeyValue = ""
	}
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(keys)
}

func (h *Handler) apiPurchaseSupplierKeys(w http.ResponseWriter, r *http.Request, id string) {
	if !h.supplierReady(w) {
		return
	}
	integration := config.GetSupplierIntegration()
	provider := config.GetSupplierProvider(id)
	if provider == nil {
		writeSupplierError(w, http.StatusNotFound, config.ErrSupplierNotFound)
		return
	}
	if !integration.Enabled || !provider.Enabled {
		writeSupplierError(w, http.StatusConflict, errors.New("supplier feature and provider must be enabled"))
		return
	}
	var body struct {
		Count      int    `json:"count"`
		Region     string `json:"region"`
		BatchID    string `json:"batchId"`
		AutoImport bool   `json:"autoImport"`
	}
	if err := decodeSupplierJSON(r, &body); err != nil {
		writeSupplierError(w, http.StatusBadRequest, err)
		return
	}
	outcome, err := h.suppliers.startPurchaseFromBatch(*provider, body.Count, body.Region, body.AutoImport, "manual", body.BatchID)
	if err != nil {
		if outcome.Pending {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"pending": true,
				"message": "purchase outcome is pending a safe idempotent retry",
				"intent":  outcome.Intent,
			})
			return
		}
		writeSupplierError(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"success":  true,
		"pending":  false,
		"purchase": outcome.Response,
		"batch":    outcome.Batch,
	})
}

func (h *Handler) apiGetSupplierBatches(w http.ResponseWriter, _ *http.Request) {
	if !h.supplierReady(w) {
		return
	}
	h.suppliers.reconcileBatches()
	batches := h.suppliers.store.batches()
	json.NewEncoder(w).Encode(map[string]any{
		"items":    batches,
		"lifetime": supplierLifetimeSummary(batches),
	})
}

func (h *Handler) handleSupplierWebhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeSupplierError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if h.suppliers == nil {
		writeSupplierError(w, http.StatusServiceUnavailable, errors.New("supplier integration is unavailable"))
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/supplier-webhooks/")
	if id == "" || strings.Contains(id, "/") {
		writeSupplierError(w, http.StatusNotFound, config.ErrSupplierNotFound)
		return
	}
	provider := config.GetSupplierProvider(id)
	if provider == nil {
		writeSupplierError(w, http.StatusNotFound, config.ErrSupplierNotFound)
		return
	}
	apiType := config.EffectiveSupplierAPIType(provider.APIType)
	awsMyProtocol := apiType == config.SupplierAPITypeAWSMy
	kiroDropProtocol := apiType == config.SupplierAPITypeKiroDrop
	if !kiroDropProtocol && !h.suppliers.allowWebhook(provider.ID) {
		w.Header().Set("Retry-After", "60")
		writeSupplierError(w, http.StatusTooManyRequests, errors.New("webhook rate limit exceeded"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, supplierWebhookBodyLimit)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeSupplierError(w, http.StatusRequestEntityTooLarge, errors.New("webhook body exceeds 64 KiB"))
			return
		}
		writeSupplierError(w, http.StatusBadRequest, err)
		return
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
		return
	}
	if kiroDropProtocol {
		// Kiro Drop may send its read-only endpoint check before a signing secret
		// has been exchanged. Recognize only the two documented/legacy test names
		// before HMAC verification; all business events still require a signature.
		r.Body = io.NopCloser(bytes.NewReader(payload))
		var probe supplierWebhookEvent
		probeErr := decodeSupplierJSON(r, &probe)
		r.Body = io.NopCloser(bytes.NewReader(payload))
		probe.Event = strings.TrimSpace(probe.Event)
		probe.EventID = strings.TrimSpace(probe.EventID)
		if probeErr == nil && (probe.Event == "test" || probe.Event == "webhook_test") {
			if !isSupplier32HexID(probe.EventID) {
				writeSupplierError(w, http.StatusBadRequest, errors.New("event_id must be a 32-character hexadecimal string"))
				return
			}
			if !h.suppliers.allowWebhook(provider.ID + "#test") {
				w.Header().Set("Retry-After", "60")
				writeSupplierError(w, http.StatusTooManyRequests, errors.New("webhook test rate limit exceeded"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
		if err := verifyKiroDropWebhook(*provider, r.Header, payload); err != nil {
			writeSupplierError(w, http.StatusUnauthorized, err)
			return
		}
		if !h.suppliers.allowWebhook(provider.ID) {
			w.Header().Set("Retry-After", "60")
			writeSupplierError(w, http.StatusTooManyRequests, errors.New("webhook rate limit exceeded"))
			return
		}
	}

	integration := config.GetSupplierIntegration()
	r.Body = io.NopCloser(bytes.NewReader(payload))
	var event supplierWebhookEvent
	decodeErr := decodeSupplierJSON(r, &event)
	if decodeErr == nil {
		event.Event = strings.TrimSpace(event.Event)
		event.EventID = strings.TrimSpace(event.EventID)
		// Providers use this event to verify a configured callback URL. It must
		// remain a read-only health check even while the integration or provider
		// is disabled, and must never enter the durable event/purchase workflow.
		if event.Event == "webhook_test" || (kiroDropProtocol && event.Event == "test") {
			if event.EventID == "" {
				writeSupplierError(w, http.StatusBadRequest, errors.New("event_id is required"))
				return
			}
			if (awsMyProtocol || kiroDropProtocol) && !isSupplier32HexID(event.EventID) {
				writeSupplierError(w, http.StatusBadRequest, errors.New("event_id must be a 32-character hexadecimal string"))
				return
			}
			if len(event.EventID) > 256 {
				writeSupplierError(w, http.StatusBadRequest, errors.New("event_id is too long"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
			return
		}
	}
	if !integration.Enabled || !provider.Enabled {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"accepted": false,
			"reason":   "supplier integration is disabled",
		})
		return
	}
	if decodeErr != nil {
		err := decodeErr
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeSupplierError(w, http.StatusRequestEntityTooLarge, errors.New("webhook body exceeds 64 KiB"))
			return
		}
		writeSupplierError(w, http.StatusBadRequest, err)
		return
	}
	if event.Event == "" || event.EventID == "" {
		writeSupplierError(w, http.StatusBadRequest, errors.New("event and event_id are required"))
		return
	}
	if len(event.EventID) > 256 || len(event.Event) > 100 {
		writeSupplierError(w, http.StatusBadRequest, errors.New("event or event_id is too long"))
		return
	}
	if awsMyProtocol || kiroDropProtocol {
		if !isSupplier32HexID(event.EventID) {
			writeSupplierError(w, http.StatusBadRequest, errors.New("event_id must be a 32-character hexadecimal string"))
			return
		}
		// The merchant API key authenticates only outbound API calls. AWS My uses
		// the fixed provider URL plus strict validation and idempotency; Kiro Drop
		// additionally reaches this point only after HMAC verification above.
		if len(strings.TrimSpace(event.BatchID)) > 256 {
			writeSupplierError(w, http.StatusBadRequest, errors.New("batch_id is too long"))
			return
		}
	}

	var intent *supplierPurchaseIntent
	pendingEvent := false
	inventoryWake := false
	switch event.Event {
	case "new_keys_available":
		if kiroDropProtocol {
			if err := validateKiroDropNewKeysEvent(event); err != nil {
				writeSupplierError(w, http.StatusBadRequest, err)
				return
			}
			inventoryWake = true
			break
		}
		if awsMyProtocol && config.EffectiveSupplierPurchaseSource(provider.PurchaseSource) == config.SupplierPurchaseSourceOwn {
			value, intentErr := newSupplierWebhookPurchaseIntent(*provider, event)
			if intentErr != nil {
				writeSupplierError(w, http.StatusBadRequest, intentErr)
				return
			}
			intent = &value
		} else {
			// KiroApp inventory notifications and AWS public-pool notifications
			// wake the normal zero-live-key check. Public extraction must query
			// /api/public/stock and generate its own client_order_id; the callback's
			// purchase_order_id is valid only for the batch owner on /api/my.
			inventoryWake = true
		}
	case "all_keys_dead":
		if awsMyProtocol || kiroDropProtocol {
			if event.Dead < 1 {
				writeSupplierError(w, http.StatusBadRequest, errors.New("dead must be greater than zero"))
				return
			}
			if awsMyProtocol {
				pendingEvent = true
			} else {
				if err := validateKiroDropAllKeysDeadEvent(event); err != nil {
					writeSupplierError(w, http.StatusBadRequest, err)
					return
				}
				inventoryWake = true
			}
		}
	}

	added, workAdded, err := h.suppliers.store.recordEventAndIntent(provider.ID, event, intent, pendingEvent)
	if err != nil {
		writeSupplierError(w, http.StatusInternalServerError, fmt.Errorf("persist webhook event: %w", err))
		return
	}
	queued := workAdded || (added && inventoryWake)
	if queued {
		h.suppliers.signal(provider.ID)
	}
	json.NewEncoder(w).Encode(map[string]any{
		"ok":        true,
		"accepted":  true,
		"duplicate": !added,
		"queued":    queued,
	})
}

func verifyKiroDropWebhook(provider config.SupplierProvider, header http.Header, payload []byte) error {
	secret := strings.TrimSpace(provider.WebhookSecret)
	if secret == "" {
		return errors.New("webhook signing secret is not configured; configure the webhook from Kiro-Go first")
	}
	eventID := strings.TrimSpace(header.Get("X-Kiro-Event-Id"))
	if !isSupplier32HexID(eventID) {
		return errors.New("invalid X-Kiro-Event-Id header")
	}
	timestampText := strings.TrimSpace(header.Get("X-Kiro-Timestamp"))
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil || timestamp <= 0 {
		return errors.New("invalid X-Kiro-Timestamp header")
	}
	delta := supplierNow().Unix() - timestamp
	if delta < 0 {
		delta = -delta
	}
	if delta > int64((5*time.Minute)/time.Second) {
		return errors.New("webhook timestamp is outside the allowed five-minute window")
	}
	signatureText := strings.TrimSpace(header.Get("X-Kiro-Signature"))
	if !strings.HasPrefix(signatureText, "v1=") {
		return errors.New("invalid X-Kiro-Signature header")
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signatureText, "v1="))
	if err != nil || len(provided) != sha256.Size {
		return errors.New("invalid X-Kiro-Signature header")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestampText))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(payload)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return errors.New("webhook signature verification failed")
	}
	var envelope struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return errors.New("invalid webhook JSON")
	}
	if strings.TrimSpace(envelope.EventID) != eventID {
		return errors.New("X-Kiro-Event-Id does not match the request body")
	}
	return nil
}

func validateKiroDropNewKeysEvent(event supplierWebhookEvent) error {
	if event.NewKeys < 1 {
		return errors.New("new_keys must be greater than zero")
	}
	region := strings.ToLower(strings.TrimSpace(event.Region))
	if region == "dual" {
		if strings.TrimSpace(event.DispatchID) == "" || len(strings.TrimSpace(event.DispatchID)) > 256 {
			return errors.New("dispatch_id is required and must not exceed 256 characters for a dual-region event")
		}
		usCount := event.NewKeysByRegion["us-east-1"]
		euCount := event.NewKeysByRegion["eu-central-1"]
		if usCount < 0 || euCount < 0 || usCount+euCount != event.NewKeys || usCount+euCount == 0 {
			return errors.New("new_keys_by_region must contain non-negative US/EU counts that sum to new_keys")
		}
		for key, count := range map[string]int{"us-east-1": usCount, "eu-central-1": euCount} {
			if count > 0 && !isSupplier32HexID(strings.TrimSpace(event.PurchaseOrderIDsByRegion[key])) {
				return fmt.Errorf("purchase_order_ids_by_region.%s must be a 32-character hexadecimal string", key)
			}
			for _, batchID := range event.BatchIDsByRegion[key] {
				if strings.TrimSpace(batchID) == "" || len(strings.TrimSpace(batchID)) > 256 {
					return fmt.Errorf("batch_ids_by_region.%s contains an invalid batch ID", key)
				}
			}
		}
		if len(event.Regions) > 0 {
			seen := make(map[string]bool, len(event.Regions))
			for _, item := range event.Regions {
				normalized := strings.ToLower(strings.TrimSpace(item))
				if normalized != "us-east-1" && normalized != "eu-central-1" {
					return errors.New("regions contains an unsupported region")
				}
				seen[normalized] = true
			}
			if (usCount > 0) != seen["us-east-1"] || (euCount > 0) != seen["eu-central-1"] || len(seen) != len(event.Regions) {
				return errors.New("regions must exactly match the positive entries in new_keys_by_region")
			}
		}
		return nil
	}
	if region != "us-east-1" && region != "eu-central-1" {
		return errors.New("region must be us-east-1, eu-central-1, or dual")
	}
	if !isSupplier32HexID(strings.TrimSpace(event.PurchaseOrderID)) {
		return errors.New("purchase_order_id must be a 32-character hexadecimal string")
	}
	if strings.TrimSpace(event.OrderID) == "" || len(strings.TrimSpace(event.OrderID)) > 256 {
		return errors.New("order_id is required and must not exceed 256 characters")
	}
	return nil
}

func validateKiroDropAllKeysDeadEvent(event supplierWebhookEvent) error {
	region := strings.ToLower(strings.TrimSpace(event.Region))
	if region != "us-east-1" && region != "eu-central-1" {
		return errors.New("region must be us-east-1 or eu-central-1")
	}
	if strings.TrimSpace(event.OrderID) == "" || len(strings.TrimSpace(event.OrderID)) > 256 {
		return errors.New("order_id is required and must not exceed 256 characters")
	}
	return nil
}

func decodeSupplierJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

func writeSupplierError(w http.ResponseWriter, status int, err error) {
	w.WriteHeader(status)
	message := "unknown error"
	if err != nil {
		message = err.Error()
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
