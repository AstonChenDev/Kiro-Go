package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"strconv"
	"strings"
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
			"enabled":             provider.Enabled,
			"priority":            provider.Priority,
			"autoPurchaseCount":   provider.AutoPurchaseCount,
			"hasToken":            strings.TrimSpace(provider.APIToken) != "",
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
	keys, err := h.suppliers.apiFactory(*provider).GetKeys(history, page, pageSize)
	if err != nil {
		writeSupplierError(w, http.StatusBadGateway, err)
		return
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
		AutoImport bool   `json:"autoImport"`
	}
	if err := decodeSupplierJSON(r, &body); err != nil {
		writeSupplierError(w, http.StatusBadRequest, err)
		return
	}
	outcome, err := h.suppliers.startPurchase(*provider, body.Count, body.Region, body.AutoImport, "manual")
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
	if !h.suppliers.allowWebhook(provider.ID) {
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
		if event.Event == "webhook_test" {
			if event.EventID == "" {
				writeSupplierError(w, http.StatusBadRequest, errors.New("event_id is required"))
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
	added, err := h.suppliers.store.recordEvent(provider.ID, event)
	if err != nil {
		writeSupplierError(w, http.StatusInternalServerError, fmt.Errorf("persist webhook event: %w", err))
		return
	}
	if added && event.Event == "new_keys_available" {
		h.suppliers.signal(provider.ID)
	}
	json.NewEncoder(w).Encode(map[string]any{
		"accepted":  true,
		"duplicate": !added,
	})
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
