package config

import (
	"path/filepath"
	"testing"
)

func initSupplierTestConfig(t *testing.T) {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

func TestSupplierProviderLifecycleKeepsPermanentIDAndSecret(t *testing.T) {
	initSupplierTestConfig(t)
	created, err := AddSupplierProvider(SupplierProvider{
		ID:                "vendor-a",
		Name:              "Vendor A",
		BaseURL:           "https://vendor.example/",
		APIToken:          "km_original_secret",
		Enabled:           true,
		Priority:          20,
		AutoPurchaseCount: 8,
	})
	if err != nil {
		t.Fatalf("AddSupplierProvider: %v", err)
	}
	if created.ID != "vendor-a" || created.BaseURL != "https://vendor.example" {
		t.Fatalf("unexpected normalized supplier: %+v", created)
	}
	if created.APIType != SupplierAPITypeKiroApp {
		t.Fatalf("legacy/default API type = %q", created.APIType)
	}

	updated, err := UpdateSupplierProvider("vendor-a", SupplierProvider{
		Name:              "Renamed Vendor",
		BaseURL:           "https://new.example/base/",
		Enabled:           true,
		Priority:          1,
		AutoPurchaseCount: 12,
	})
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	if updated.ID != "vendor-a" {
		t.Fatalf("immutable id changed: %q", updated.ID)
	}
	if updated.APIToken != "km_original_secret" {
		t.Fatalf("blank update replaced write-only token: %q", updated.APIToken)
	}
	if updated.BaseURL != "https://new.example/base" {
		t.Fatalf("base URL = %q", updated.BaseURL)
	}

	if err := UpdateSupplierFeature(true, true); err != nil {
		t.Fatalf("UpdateSupplierFeature: %v", err)
	}
	got := GetSupplierIntegration()
	if !got.Enabled || !got.AutoPurchaseEnabled || len(got.Providers) != 1 {
		t.Fatalf("unexpected supplier integration: %+v", got)
	}
}

func TestSupplierAPITypeIsValidatedAndPreservedForLegacyUpdates(t *testing.T) {
	initSupplierTestConfig(t)
	created, err := AddSupplierProvider(SupplierProvider{
		ID: "aws-vendor", Name: "AWS Vendor", BaseURL: "https://aws.example", APIToken: "secret",
		APIType: SupplierAPITypeAWSMy, Enabled: true, AutoPurchaseCount: 2,
	})
	if err != nil {
		t.Fatalf("AddSupplierProvider: %v", err)
	}
	if created.APIType != SupplierAPITypeAWSMy {
		t.Fatalf("created API type = %q", created.APIType)
	}

	updated, err := UpdateSupplierProvider(created.ID, SupplierProvider{
		Name: "AWS Vendor Updated", BaseURL: created.BaseURL, Enabled: true, AutoPurchaseCount: 3,
	})
	if err != nil {
		t.Fatalf("legacy UpdateSupplierProvider: %v", err)
	}
	if updated.APIType != SupplierAPITypeAWSMy || updated.APIToken != "secret" {
		t.Fatalf("legacy update lost protocol or secret: %+v", updated)
	}
	if got := GetSupplierProvider(created.ID); got == nil || got.APIType != SupplierAPITypeAWSMy {
		t.Fatalf("GetSupplierProvider = %+v", got)
	}

	if _, err := AddSupplierProvider(SupplierProvider{
		ID: "bad-type", Name: "Bad", BaseURL: "https://bad.example", APIToken: "secret",
		APIType: "unknown", AutoPurchaseCount: 1,
	}); err == nil {
		t.Fatal("unknown supplier API type was accepted")
	}
}

func TestSupplierPollIntervalDefaultsValidatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := GetSupplierIntegration().PollIntervalSeconds; got != DefaultSupplierPollIntervalSeconds {
		t.Fatalf("default poll interval = %d, want %d", got, DefaultSupplierPollIntervalSeconds)
	}

	for _, invalid := range []int{MinSupplierPollIntervalSeconds - 1, MaxSupplierPollIntervalSeconds + 1} {
		if err := UpdateSupplierSettings(true, true, invalid); err == nil {
			t.Fatalf("poll interval %d unexpectedly accepted", invalid)
		}
	}
	if err := UpdateSupplierSettings(true, true, MinSupplierPollIntervalSeconds); err != nil {
		t.Fatalf("UpdateSupplierSettings: %v", err)
	}
	if got := GetSupplierIntegration().PollIntervalSeconds; got != MinSupplierPollIntervalSeconds {
		t.Fatalf("poll interval = %d, want %d", got, MinSupplierPollIntervalSeconds)
	}

	if err := Init(path); err != nil {
		t.Fatalf("reload Init: %v", err)
	}
	got := GetSupplierIntegration()
	if !got.Enabled || !got.AutoPurchaseEnabled || got.PollIntervalSeconds != MinSupplierPollIntervalSeconds {
		t.Fatalf("poll settings did not persist: %+v", got)
	}
}

func TestSupplierValidationRejectsUnsafeOrUnstableValues(t *testing.T) {
	initSupplierTestConfig(t)
	tests := []SupplierProvider{
		{ID: "Upper Case", Name: "x", BaseURL: "https://example.com", APIToken: "km_x", AutoPurchaseCount: 1},
		{ID: "vendor", Name: "", BaseURL: "https://example.com", APIToken: "km_x", AutoPurchaseCount: 1},
		{ID: "vendor", Name: "x", BaseURL: "ftp://example.com", APIToken: "km_x", AutoPurchaseCount: 1},
		{ID: "vendor", Name: "x", BaseURL: "https://user:pass@example.com", APIToken: "km_x", AutoPurchaseCount: 1},
		{ID: "vendor", Name: "x", BaseURL: "https://example.com?q=1", APIToken: "km_x", AutoPurchaseCount: 1},
		{ID: "vendor", Name: "x", BaseURL: "https://example.com", APIToken: "", AutoPurchaseCount: 1},
		{ID: "vendor", Name: "x", BaseURL: "https://example.com", APIToken: "km_x", AutoPurchaseCount: MaxSupplierPurchaseCount + 1},
	}
	for i, provider := range tests {
		if _, err := AddSupplierProvider(provider); err == nil {
			t.Fatalf("case %d unexpectedly succeeded: %+v", i, provider)
		}
	}
}

func TestSortedEnabledSupplierProvidersHonorsWakeSourceThenPriority(t *testing.T) {
	initSupplierTestConfig(t)
	for _, provider := range []SupplierProvider{
		{ID: "slow", Name: "Slow", BaseURL: "https://slow.example", APIToken: "km_1", Enabled: true, Priority: 50, AutoPurchaseCount: 1},
		{ID: "fast", Name: "Fast", BaseURL: "https://fast.example", APIToken: "km_2", Enabled: true, Priority: 1, AutoPurchaseCount: 1},
		{ID: "off", Name: "Off", BaseURL: "https://off.example", APIToken: "km_3", Enabled: false, Priority: 0, AutoPurchaseCount: 1},
	} {
		if _, err := AddSupplierProvider(provider); err != nil {
			t.Fatalf("AddSupplierProvider(%s): %v", provider.ID, err)
		}
	}
	got := SortedEnabledSupplierProviders("slow")
	if len(got) != 2 || got[0].ID != "slow" || got[1].ID != "fast" {
		t.Fatalf("unexpected order: %+v", got)
	}
}

func TestDisableSupplierAPIKeyAccountsIsScopedAndIdempotent(t *testing.T) {
	initSupplierTestConfig(t)
	accounts := []Account{
		{ID: "a-dead", AuthMethod: "api_key", KiroApiKey: "ksk_dead_a", AccessToken: "ksk_dead_a", SupplierID: "vendor-a", Enabled: true},
		{ID: "a-live", AuthMethod: "api_key", KiroApiKey: "ksk_live_a", AccessToken: "ksk_live_a", SupplierID: "vendor-a", Enabled: true},
		{ID: "b-same", AuthMethod: "api_key", KiroApiKey: "ksk_dead_a", AccessToken: "ksk_dead_a", SupplierID: "vendor-b", Enabled: true},
	}
	// AddAccounts globally deduplicates API keys, so use a distinct key for the
	// provider-scope assertion while still checking that it remains enabled.
	accounts[2].KiroApiKey = "ksk_dead_b"
	accounts[2].AccessToken = "ksk_dead_b"
	if added, _, err := AddAccounts(accounts); err != nil || added != 3 {
		t.Fatalf("AddAccounts = %d, %v", added, err)
	}
	disabled, err := DisableSupplierAPIKeyAccounts("vendor-a", []string{"ksk_dead_a", "ksk_dead_b"}, "supplier reported all_keys_dead")
	if err != nil || disabled != 1 {
		t.Fatalf("DisableSupplierAPIKeyAccounts = %d, %v", disabled, err)
	}
	byID := make(map[string]Account)
	for _, account := range GetAccounts() {
		byID[account.ID] = account
	}
	if byID["a-dead"].Enabled || byID["a-dead"].BanStatus != "BANNED" {
		t.Fatalf("dead account was not banned: %+v", byID["a-dead"])
	}
	if !byID["a-live"].Enabled || !byID["b-same"].Enabled {
		t.Fatalf("unrelated accounts changed: live=%+v other=%+v", byID["a-live"], byID["b-same"])
	}
	if again, err := DisableSupplierAPIKeyAccounts("vendor-a", []string{"ksk_dead_a"}, "supplier reported all_keys_dead"); err != nil || again != 0 {
		t.Fatalf("idempotent disable = %d, %v", again, err)
	}
}
