package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	DefaultSupplierPurchaseCount           = 1
	MaxSupplierPurchaseCount               = 500
	DefaultSupplierPollIntervalSeconds     = 5
	MinSupplierPollIntervalSeconds         = 1
	MaxSupplierPollIntervalSeconds         = 300
	MinSupplierProviderPollIntervalSeconds = 0.1
	MaxSupplierProviderPollIntervalSeconds = 300.0
	DefaultSupplierImportMaxSSE            = 500
	DefaultSupplierImportMaxRPM            = 300
	SupplierAPITypeKiroApp                 = "kiroapp"
	SupplierAPITypeAWSMy                   = "aws_my"
	SupplierAPITypeKiroDrop                = "kiro_drop"
	SupplierAPITypeKiroCEO                 = "kiro_ceo"
	SupplierPurchaseSourceOwn              = "own"
	SupplierPurchaseSourcePublic           = "public"
	legacySupplierImportMaxSSE             = 300
	legacySupplierImportMaxRPM             = 200
)

var supplierIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var ErrSupplierNotFound = errors.New("supplier not found")

// SupplierIntegrationConfig controls the entire supplier feature. Providers
// remain configured while Enabled is false, making the master switch fully
// reversible without changing their permanent webhook URLs.
type SupplierIntegrationConfig struct {
	Enabled             bool               `json:"enabled"`
	AutoPurchaseEnabled bool               `json:"autoPurchaseEnabled"`
	PollIntervalSeconds int                `json:"pollIntervalSeconds,omitempty"`
	Providers           []SupplierProvider `json:"providers,omitempty"`
}

// SupplierProvider describes one supplier. ID is immutable once created because
// it forms the permanent webhook path. APIType selects the provider-specific
// wire protocol; an empty value from older configurations means KiroApp.
type SupplierProvider struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	BaseURL           string `json:"baseUrl"`
	APIToken          string `json:"apiToken"`
	WebhookSecret     string `json:"webhookSecret,omitempty"`
	APIType           string `json:"apiType,omitempty"`
	PurchaseSource    string `json:"purchaseSource,omitempty"`
	Enabled           bool   `json:"enabled"`
	Priority          int    `json:"priority"`
	AutoPurchaseCount int    `json:"autoPurchaseCount"`
	// PollIntervalSeconds is a provider-level override. Zero keeps older
	// configurations following the integration-wide default.
	PollIntervalSeconds float64 `json:"pollIntervalSeconds,omitempty"`
	AllowEUFallback     bool    `json:"allowEUFallback,omitempty"`
	// ImportMaxSSE/ImportMaxRPM are retained as the legacy shared aliases for
	// older configuration files and API clients. New code uses the regional
	// fields below; the aliases mirror the US values when saved.
	ImportMaxSSE   int   `json:"importMaxSSE,omitempty"`
	ImportMaxRPM   int   `json:"importMaxRPM,omitempty"`
	ImportUSMaxSSE int   `json:"importUSMaxSSE,omitempty"`
	ImportUSMaxRPM int   `json:"importUSMaxRPM,omitempty"`
	ImportEUMaxSSE int   `json:"importEUMaxSSE,omitempty"`
	ImportEUMaxRPM int   `json:"importEUMaxRPM,omitempty"`
	CreatedAt      int64 `json:"createdAt"`
	UpdatedAt      int64 `json:"updatedAt"`
}

func EffectiveSupplierAPIType(apiType string) string {
	apiType = strings.ToLower(strings.TrimSpace(apiType))
	if apiType == "" {
		return SupplierAPITypeKiroApp
	}
	return apiType
}

// EffectiveSupplierPurchaseSource keeps providers saved before public pools
// existed on their original own-inventory behavior.
func EffectiveSupplierPurchaseSource(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		return SupplierPurchaseSourceOwn
	}
	return source
}

func validateSupplierPurchaseSource(apiType, source string) error {
	switch EffectiveSupplierPurchaseSource(source) {
	case SupplierPurchaseSourceOwn:
		return nil
	case SupplierPurchaseSourcePublic:
		if EffectiveSupplierAPIType(apiType) != SupplierAPITypeAWSMy {
			return errors.New("public purchaseSource requires aws_my apiType")
		}
		return nil
	default:
		return errors.New("purchaseSource must be own or public")
	}
}

func validateSupplierAPIType(apiType string) error {
	switch EffectiveSupplierAPIType(apiType) {
	case SupplierAPITypeKiroApp, SupplierAPITypeAWSMy, SupplierAPITypeKiroDrop, SupplierAPITypeKiroCEO:
		return nil
	default:
		return errors.New("apiType must be kiroapp, aws_my, kiro_drop, or kiro_ceo")
	}
}

// SupplierSupportsEUFallback reports whether a provider exposes distinct US
// and EU inventory that can be selected at purchase time. AWS My own/public
// pools are regionless from the client's perspective and must remain US-only.
func SupplierSupportsEUFallback(provider SupplierProvider) bool {
	if EffectiveSupplierPurchaseSource(provider.PurchaseSource) != SupplierPurchaseSourceOwn {
		return false
	}
	switch EffectiveSupplierAPIType(provider.APIType) {
	case SupplierAPITypeKiroApp, SupplierAPITypeKiroDrop, SupplierAPITypeKiroCEO:
		return true
	default:
		return false
	}
}

func NormalizeSupplierID(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !supplierIDPattern.MatchString(id) {
		return "", errors.New("supplier id must be 1-64 lowercase letters, numbers, underscores, or hyphens")
	}
	return id, nil
}

func NormalizeSupplierBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("baseUrl is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid baseUrl: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("baseUrl must use http or https")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("baseUrl must contain a host and cannot contain credentials, query, or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return strings.TrimRight(u.String(), "/"), nil
}

func normalizeSupplierProvider(provider *SupplierProvider, creating bool) error {
	if provider == nil {
		return errors.New("supplier is required")
	}
	id, err := NormalizeSupplierID(provider.ID)
	if err != nil {
		return err
	}
	provider.ID = id
	provider.Name = strings.TrimSpace(provider.Name)
	if provider.Name == "" {
		return errors.New("supplier name is required")
	}
	if len(provider.Name) > 100 {
		return errors.New("supplier name is too long")
	}
	provider.BaseURL, err = NormalizeSupplierBaseURL(provider.BaseURL)
	if err != nil {
		return err
	}
	provider.APIToken = strings.TrimSpace(provider.APIToken)
	provider.WebhookSecret = strings.TrimSpace(provider.WebhookSecret)
	if provider.WebhookSecret != "" {
		if len(provider.WebhookSecret) != 64 {
			return errors.New("webhookSecret must be a 64-character hexadecimal string")
		}
		if _, err := hex.DecodeString(provider.WebhookSecret); err != nil {
			return errors.New("webhookSecret must be a 64-character hexadecimal string")
		}
	}
	provider.APIType = EffectiveSupplierAPIType(provider.APIType)
	if err := validateSupplierAPIType(provider.APIType); err != nil {
		return err
	}
	provider.PurchaseSource = EffectiveSupplierPurchaseSource(provider.PurchaseSource)
	if err := validateSupplierPurchaseSource(provider.APIType, provider.PurchaseSource); err != nil {
		return err
	}
	if provider.AllowEUFallback && !SupplierSupportsEUFallback(*provider) {
		return errors.New("allowEUFallback requires a regional own-inventory supplier protocol")
	}
	if creating && provider.APIToken == "" {
		return errors.New("apiToken is required")
	}
	if provider.AutoPurchaseCount == 0 {
		provider.AutoPurchaseCount = DefaultSupplierPurchaseCount
	}
	if provider.AutoPurchaseCount < 1 || provider.AutoPurchaseCount > MaxSupplierPurchaseCount {
		return fmt.Errorf("autoPurchaseCount must be between 1 and %d", MaxSupplierPurchaseCount)
	}
	if err := validateSupplierProviderPollInterval(provider); err != nil {
		return err
	}
	if err := normalizeSupplierImportLimits(provider); err != nil {
		return err
	}
	if provider.Priority < 0 || provider.Priority > 10000 {
		return errors.New("priority must be between 0 and 10000")
	}
	return nil
}

func validateSupplierProviderPollInterval(provider *SupplierProvider) error {
	if provider == nil || provider.PollIntervalSeconds == 0 {
		return nil
	}
	interval := provider.PollIntervalSeconds
	if math.IsNaN(interval) || math.IsInf(interval, 0) || interval < MinSupplierProviderPollIntervalSeconds || interval > MaxSupplierProviderPollIntervalSeconds {
		return fmt.Errorf("pollIntervalSeconds must be between %.1f and %.0f", MinSupplierProviderPollIntervalSeconds, MaxSupplierProviderPollIntervalSeconds)
	}
	return nil
}

func cloneSupplierConfig(in SupplierIntegrationConfig) SupplierIntegrationConfig {
	out := in
	out.Providers = append([]SupplierProvider(nil), in.Providers...)
	for i := range out.Providers {
		out.Providers[i].APIType = EffectiveSupplierAPIType(out.Providers[i].APIType)
		out.Providers[i].PurchaseSource = EffectiveSupplierPurchaseSource(out.Providers[i].PurchaseSource)
		applySupplierImportLimitDefaults(&out.Providers[i])
	}
	return out
}

func normalizeSupplierImportLimits(provider *SupplierProvider) error {
	for name, value := range map[string]int{
		"importMaxSSE": provider.ImportMaxSSE, "importMaxRPM": provider.ImportMaxRPM,
		"importUSMaxSSE": provider.ImportUSMaxSSE, "importUSMaxRPM": provider.ImportUSMaxRPM,
		"importEUMaxSSE": provider.ImportEUMaxSSE, "importEUMaxRPM": provider.ImportEUMaxRPM,
	} {
		if value < 0 {
			return fmt.Errorf("%s must be greater than zero", name)
		}
	}
	applySupplierImportLimitDefaults(provider)
	return nil
}

func applySupplierImportLimitDefaults(provider *SupplierProvider) {
	legacySSE := EffectiveSupplierImportMaxSSE(provider.ImportMaxSSE)
	legacyRPM := EffectiveSupplierImportMaxRPM(provider.ImportMaxRPM)
	if provider.ImportUSMaxSSE <= 0 {
		provider.ImportUSMaxSSE = legacySSE
	}
	if provider.ImportUSMaxRPM <= 0 {
		provider.ImportUSMaxRPM = legacyRPM
	}
	if provider.ImportEUMaxSSE <= 0 {
		provider.ImportEUMaxSSE = legacySSE
	}
	if provider.ImportEUMaxRPM <= 0 {
		provider.ImportEUMaxRPM = legacyRPM
	}
	// Keep the old fields useful to older readers without collapsing the EU
	// values. An older writer that explicitly sends these aliases intentionally
	// applies its shared value to both regions in UpdateSupplierProvider.
	provider.ImportMaxSSE = provider.ImportUSMaxSSE
	provider.ImportMaxRPM = provider.ImportUSMaxRPM
}

func EffectiveSupplierImportMaxSSE(limit int) int {
	if limit <= 0 {
		return DefaultSupplierImportMaxSSE
	}
	return limit
}

func EffectiveSupplierImportMaxRPM(limit int) int {
	if limit <= 0 {
		return DefaultSupplierImportMaxRPM
	}
	return limit
}

// SupplierImportLimitsForRegion resolves the account limits that must be
// applied when a supplier key is imported. Older providers with only the
// shared fields transparently use those values for both regions.
func SupplierImportLimitsForRegion(provider SupplierProvider, region string) (maxSSE, maxRPM int) {
	applySupplierImportLimitDefaults(&provider)
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "eu" || strings.HasPrefix(region, "eu-") {
		return provider.ImportEUMaxSSE, provider.ImportEUMaxRPM
	}
	return provider.ImportUSMaxSSE, provider.ImportUSMaxRPM
}

// EffectiveSupplierPollIntervalSeconds keeps configurations written before
// this setting existed compatible, and safely recovers from a manually edited
// out-of-range value without allowing an aggressive supplier request loop.
func EffectiveSupplierPollIntervalSeconds(seconds int) int {
	if seconds < MinSupplierPollIntervalSeconds || seconds > MaxSupplierPollIntervalSeconds {
		return DefaultSupplierPollIntervalSeconds
	}
	return seconds
}

// EffectiveSupplierProviderPollIntervalSeconds resolves the independent stock
// polling cadence for one provider. A zero override inherits the legacy global
// setting. Every supported protocol uses the same provider-level range.
func EffectiveSupplierProviderPollIntervalSeconds(provider SupplierProvider, fallbackSeconds int) float64 {
	interval := provider.PollIntervalSeconds
	if math.IsNaN(interval) || math.IsInf(interval, 0) || interval < MinSupplierProviderPollIntervalSeconds || interval > MaxSupplierProviderPollIntervalSeconds {
		interval = float64(EffectiveSupplierPollIntervalSeconds(fallbackSeconds))
	}
	return interval
}

func GetSupplierIntegration() SupplierIntegrationConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return SupplierIntegrationConfig{}
	}
	integration := cloneSupplierConfig(cfg.SupplierIntegration)
	integration.PollIntervalSeconds = EffectiveSupplierPollIntervalSeconds(integration.PollIntervalSeconds)
	return integration
}

func GetSupplierProvider(id string) *SupplierProvider {
	id = strings.ToLower(strings.TrimSpace(id))
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.SupplierIntegration.Providers {
		if cfg.SupplierIntegration.Providers[i].ID == id {
			provider := cfg.SupplierIntegration.Providers[i]
			provider.APIType = EffectiveSupplierAPIType(provider.APIType)
			provider.PurchaseSource = EffectiveSupplierPurchaseSource(provider.PurchaseSource)
			applySupplierImportLimitDefaults(&provider)
			return &provider
		}
	}
	return nil
}

func UpdateSupplierFeature(enabled, autoPurchaseEnabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	previous := cloneSupplierConfig(cfg.SupplierIntegration)
	cfg.SupplierIntegration.Enabled = enabled
	cfg.SupplierIntegration.AutoPurchaseEnabled = autoPurchaseEnabled
	if err := saveLocked(); err != nil {
		cfg.SupplierIntegration = previous
		return err
	}
	return nil
}

// UpdateSupplierSettings persists the runtime polling interval together with
// the feature switches. The manager observes the new value through its wake
// channel, so no process or container restart is required.
func UpdateSupplierSettings(enabled, autoPurchaseEnabled bool, pollIntervalSeconds int) error {
	if pollIntervalSeconds < MinSupplierPollIntervalSeconds || pollIntervalSeconds > MaxSupplierPollIntervalSeconds {
		return fmt.Errorf("pollIntervalSeconds must be between %d and %d", MinSupplierPollIntervalSeconds, MaxSupplierPollIntervalSeconds)
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	previous := cloneSupplierConfig(cfg.SupplierIntegration)
	cfg.SupplierIntegration.Enabled = enabled
	cfg.SupplierIntegration.AutoPurchaseEnabled = autoPurchaseEnabled
	cfg.SupplierIntegration.PollIntervalSeconds = pollIntervalSeconds
	if err := saveLocked(); err != nil {
		cfg.SupplierIntegration = previous
		return err
	}
	return nil
}

func AddSupplierProvider(provider SupplierProvider) (SupplierProvider, error) {
	if err := normalizeSupplierProvider(&provider, true); err != nil {
		return SupplierProvider{}, err
	}
	now := time.Now().Unix()
	provider.CreatedAt = now
	provider.UpdatedAt = now

	cfgLock.Lock()
	defer cfgLock.Unlock()
	for _, existing := range cfg.SupplierIntegration.Providers {
		if existing.ID == provider.ID {
			return SupplierProvider{}, errors.New("supplier id already exists")
		}
	}
	previous := append([]SupplierProvider(nil), cfg.SupplierIntegration.Providers...)
	cfg.SupplierIntegration.Providers = append(cfg.SupplierIntegration.Providers, provider)
	if err := saveLocked(); err != nil {
		cfg.SupplierIntegration.Providers = previous
		return SupplierProvider{}, err
	}
	return provider, nil
}

// UpdateSupplierProvider preserves the immutable ID and the existing token when
// apiToken is blank. This gives the admin UI write-only secret semantics.
func UpdateSupplierProvider(id string, update SupplierProvider) (SupplierProvider, error) {
	normalizedID, err := NormalizeSupplierID(id)
	if err != nil {
		return SupplierProvider{}, err
	}
	update.ID = normalizedID
	apiTypeProvided := strings.TrimSpace(update.APIType) != ""
	purchaseSourceProvided := strings.TrimSpace(update.PurchaseSource) != ""
	apiTokenProvided := strings.TrimSpace(update.APIToken) != ""
	pollIntervalProvided := update.PollIntervalSeconds != 0
	importMaxSSEProvided := update.ImportMaxSSE != 0
	importMaxRPMProvided := update.ImportMaxRPM != 0
	importUSMaxSSEProvided := update.ImportUSMaxSSE != 0
	importUSMaxRPMProvided := update.ImportUSMaxRPM != 0
	importEUMaxSSEProvided := update.ImportEUMaxSSE != 0
	importEUMaxRPMProvided := update.ImportEUMaxRPM != 0
	if err := normalizeSupplierProvider(&update, false); err != nil {
		return SupplierProvider{}, err
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.SupplierIntegration.Providers {
		if cfg.SupplierIntegration.Providers[i].ID != normalizedID {
			continue
		}
		previous := cfg.SupplierIntegration.Providers[i]
		update.CreatedAt = previous.CreatedAt
		update.UpdatedAt = time.Now().Unix()
		if !apiTypeProvided {
			update.APIType = EffectiveSupplierAPIType(previous.APIType)
		}
		if !purchaseSourceProvided {
			if apiTypeProvided && update.APIType != EffectiveSupplierAPIType(previous.APIType) {
				update.PurchaseSource = SupplierPurchaseSourceOwn
			} else {
				update.PurchaseSource = EffectiveSupplierPurchaseSource(previous.PurchaseSource)
			}
		}
		if !pollIntervalProvided {
			update.PollIntervalSeconds = previous.PollIntervalSeconds
		}
		if err := validateSupplierPurchaseSource(update.APIType, update.PurchaseSource); err != nil {
			return SupplierProvider{}, err
		}
		if err := validateSupplierProviderPollInterval(&update); err != nil {
			return SupplierProvider{}, err
		}
		if update.AllowEUFallback && !SupplierSupportsEUFallback(update) {
			return SupplierProvider{}, errors.New("allowEUFallback requires a regional own-inventory supplier protocol")
		}
		connectionChanged := update.APIType != EffectiveSupplierAPIType(previous.APIType) ||
			update.BaseURL != previous.BaseURL || (apiTokenProvided && update.APIToken != previous.APIToken)
		if update.APIToken == "" {
			update.APIToken = previous.APIToken
		}
		previousLimits := previous
		applySupplierImportLimitDefaults(&previousLimits)
		if !importUSMaxSSEProvided && !importMaxSSEProvided {
			update.ImportUSMaxSSE = previousLimits.ImportUSMaxSSE
		}
		if !importUSMaxRPMProvided && !importMaxRPMProvided {
			update.ImportUSMaxRPM = previousLimits.ImportUSMaxRPM
		}
		if !importEUMaxSSEProvided && !importMaxSSEProvided {
			update.ImportEUMaxSSE = previousLimits.ImportEUMaxSSE
		}
		if !importEUMaxRPMProvided && !importMaxRPMProvided {
			update.ImportEUMaxRPM = previousLimits.ImportEUMaxRPM
		}
		update.ImportMaxSSE = update.ImportUSMaxSSE
		update.ImportMaxRPM = update.ImportUSMaxRPM
		if update.WebhookSecret == "" && !connectionChanged {
			update.WebhookSecret = previous.WebhookSecret
		}
		if update.APIToken == "" {
			return SupplierProvider{}, errors.New("apiToken is required")
		}
		previousAccounts := append([]Account(nil), cfg.Accounts...)
		cfg.SupplierIntegration.Providers[i] = update
		migrateSupplierAccountRegionLimitsLocked(previous.ID, false, previousLimits.ImportUSMaxSSE, previousLimits.ImportUSMaxRPM, update.ImportUSMaxSSE, update.ImportUSMaxRPM)
		migrateSupplierAccountRegionLimitsLocked(previous.ID, true, previousLimits.ImportEUMaxSSE, previousLimits.ImportEUMaxRPM, update.ImportEUMaxSSE, update.ImportEUMaxRPM)
		if err := saveLocked(); err != nil {
			cfg.SupplierIntegration.Providers[i] = previous
			cfg.Accounts = previousAccounts
			return SupplierProvider{}, err
		}
		return update, nil
	}
	return SupplierProvider{}, ErrSupplierNotFound
}

func migrateSupplierAccountRegionLimitsLocked(providerID string, eu bool, oldSSE, oldRPM, newSSE, newRPM int) int {
	if providerID == "" || oldSSE == newSSE && oldRPM == newRPM {
		return 0
	}
	migrated := 0
	for i := range cfg.Accounts {
		account := &cfg.Accounts[i]
		if account.SupplierID != providerID || !IsAPIKeyAccount(account) || supplierAccountUsesEU(account) != eu ||
			account.MaxSSE != oldSSE || account.MaxRPM != oldRPM {
			continue
		}
		account.MaxSSE = newSSE
		account.MaxRPM = newRPM
		migrated++
	}
	return migrated
}

func supplierAccountUsesEU(account *Account) bool {
	if account == nil {
		return false
	}
	region := strings.ToLower(strings.TrimSpace(account.ApiRegion))
	if region == "" {
		region = strings.ToLower(strings.TrimSpace(account.Region))
	}
	return region == "eu" || strings.HasPrefix(region, "eu-")
}

// MigrateLegacySupplierImportLimits upgrades only supplier-managed API-key
// accounts that still carry the former 300/200 defaults. Manual accounts and
// accounts whose limits were customized remain untouched. It is idempotent and
// can safely run at every startup.
func MigrateLegacySupplierImportLimits() (int, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return 0, nil
	}

	providers := make(map[string]SupplierProvider, len(cfg.SupplierIntegration.Providers))
	for _, provider := range cfg.SupplierIntegration.Providers {
		providers[provider.ID] = provider
	}
	previousAccounts := append([]Account(nil), cfg.Accounts...)
	migrated := 0
	for i := range cfg.Accounts {
		account := &cfg.Accounts[i]
		provider, ok := providers[account.SupplierID]
		if !ok || !IsAPIKeyAccount(account) || account.MaxSSE != legacySupplierImportMaxSSE || account.MaxRPM != legacySupplierImportMaxRPM {
			continue
		}
		region := "us"
		if supplierAccountUsesEU(account) {
			region = "eu"
		}
		newSSE, newRPM := SupplierImportLimitsForRegion(provider, region)
		if newSSE == legacySupplierImportMaxSSE && newRPM == legacySupplierImportMaxRPM {
			continue
		}
		account.MaxSSE = newSSE
		account.MaxRPM = newRPM
		migrated++
	}
	if migrated == 0 {
		return 0, nil
	}
	if err := saveLocked(); err != nil {
		cfg.Accounts = previousAccounts
		return 0, err
	}
	return migrated, nil
}

// UpdateSupplierWebhookSecret persists the signing secret returned by a
// supplier's webhook configuration endpoint without exposing it through the
// admin API or rewriting unrelated provider settings.
func UpdateSupplierWebhookSecret(id, secret string) error {
	normalizedID, err := NormalizeSupplierID(id)
	if err != nil {
		return err
	}
	secret = strings.TrimSpace(secret)
	if len(secret) != 64 {
		return errors.New("webhook secret must be a 64-character hexadecimal string")
	}
	if _, err := hex.DecodeString(secret); err != nil {
		return errors.New("webhook secret must be a 64-character hexadecimal string")
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.SupplierIntegration.Providers {
		if cfg.SupplierIntegration.Providers[i].ID != normalizedID {
			continue
		}
		previous := cfg.SupplierIntegration.Providers[i]
		cfg.SupplierIntegration.Providers[i].WebhookSecret = secret
		cfg.SupplierIntegration.Providers[i].UpdatedAt = time.Now().Unix()
		if err := saveLocked(); err != nil {
			cfg.SupplierIntegration.Providers[i] = previous
			return err
		}
		return nil
	}
	return ErrSupplierNotFound
}

func SortedEnabledSupplierProviders(preferredID string) []SupplierProvider {
	integration := GetSupplierIntegration()
	providers := make([]SupplierProvider, 0, len(integration.Providers))
	for _, provider := range integration.Providers {
		if provider.Enabled && provider.APIToken != "" {
			providers = append(providers, provider)
		}
	}
	sort.SliceStable(providers, func(i, j int) bool {
		if providers[i].ID == preferredID {
			return true
		}
		if providers[j].ID == preferredID {
			return false
		}
		if providers[i].Priority != providers[j].Priority {
			return providers[i].Priority < providers[j].Priority
		}
		return providers[i].ID < providers[j].ID
	})
	return providers
}
