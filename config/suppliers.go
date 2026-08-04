package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	DefaultSupplierPurchaseCount       = 1
	MaxSupplierPurchaseCount           = 500
	DefaultSupplierPollIntervalSeconds = 5
	MinSupplierPollIntervalSeconds     = 5
	MaxSupplierPollIntervalSeconds     = 300
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

// SupplierProvider describes one API-compatible supplier. ID is immutable once
// created because it forms the permanent webhook path.
type SupplierProvider struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	BaseURL           string `json:"baseUrl"`
	APIToken          string `json:"apiToken"`
	Enabled           bool   `json:"enabled"`
	Priority          int    `json:"priority"`
	AutoPurchaseCount int    `json:"autoPurchaseCount"`
	CreatedAt         int64  `json:"createdAt"`
	UpdatedAt         int64  `json:"updatedAt"`
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
	if creating && provider.APIToken == "" {
		return errors.New("apiToken is required")
	}
	if provider.AutoPurchaseCount == 0 {
		provider.AutoPurchaseCount = DefaultSupplierPurchaseCount
	}
	if provider.AutoPurchaseCount < 1 || provider.AutoPurchaseCount > MaxSupplierPurchaseCount {
		return fmt.Errorf("autoPurchaseCount must be between 1 and %d", MaxSupplierPurchaseCount)
	}
	if provider.Priority < 0 || provider.Priority > 10000 {
		return errors.New("priority must be between 0 and 10000")
	}
	return nil
}

func cloneSupplierConfig(in SupplierIntegrationConfig) SupplierIntegrationConfig {
	out := in
	out.Providers = append([]SupplierProvider(nil), in.Providers...)
	return out
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
		if update.APIToken == "" {
			update.APIToken = previous.APIToken
		}
		if update.APIToken == "" {
			return SupplierProvider{}, errors.New("apiToken is required")
		}
		cfg.SupplierIntegration.Providers[i] = update
		if err := saveLocked(); err != nil {
			cfg.SupplierIntegration.Providers[i] = previous
			return SupplierProvider{}, err
		}
		return update, nil
	}
	return SupplierProvider{}, ErrSupplierNotFound
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
