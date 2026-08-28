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
	if created.PurchaseSource != SupplierPurchaseSourceOwn {
		t.Fatalf("legacy/default purchase source = %q", created.PurchaseSource)
	}
	if created.AllowEUFallback {
		t.Fatal("EU fallback must be disabled by default")
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

func TestSupplierImportLimitsDefaultAndFollowProviderUpdates(t *testing.T) {
	initSupplierTestConfig(t)
	provider, err := AddSupplierProvider(SupplierProvider{
		ID: "limits", Name: "Limits", BaseURL: "https://limits.example", APIToken: "secret",
		Enabled: true, AutoPurchaseCount: 1,
	})
	if err != nil {
		t.Fatalf("AddSupplierProvider: %v", err)
	}
	if provider.ImportUSMaxSSE != DefaultSupplierImportMaxSSE || provider.ImportUSMaxRPM != DefaultSupplierImportMaxRPM ||
		provider.ImportEUMaxSSE != DefaultSupplierImportMaxSSE || provider.ImportEUMaxRPM != DefaultSupplierImportMaxRPM {
		t.Fatalf("default regional import limits = %+v", provider)
	}
	accounts := []Account{
		{ID: "us-managed", AuthMethod: "api_key", KiroApiKey: "ksk_us_managed", AccessToken: "ksk_us_managed", SupplierID: provider.ID, Region: "us-east-1", Enabled: true, MaxSSE: provider.ImportUSMaxSSE, MaxRPM: provider.ImportUSMaxRPM},
		{ID: "eu-managed", AuthMethod: "api_key", KiroApiKey: "ksk_eu_managed", AccessToken: "ksk_eu_managed", SupplierID: provider.ID, Region: "eu-central-1", Enabled: true, MaxSSE: provider.ImportEUMaxSSE, MaxRPM: provider.ImportEUMaxRPM},
		{ID: "us-custom", AuthMethod: "api_key", KiroApiKey: "ksk_us_custom", AccessToken: "ksk_us_custom", SupplierID: provider.ID, Region: "us-east-1", Enabled: true, MaxSSE: provider.ImportUSMaxSSE + 1, MaxRPM: provider.ImportUSMaxRPM},
		{ID: "eu-custom", AuthMethod: "api_key", KiroApiKey: "ksk_eu_custom", AccessToken: "ksk_eu_custom", SupplierID: provider.ID, Region: "eu-central-1", Enabled: true, MaxSSE: provider.ImportEUMaxSSE, MaxRPM: provider.ImportEUMaxRPM + 1},
	}
	if added, _, addErr := AddAccounts(accounts); addErr != nil || added != len(accounts) {
		t.Fatalf("AddAccounts = %d, %v", added, addErr)
	}

	provider.ImportUSMaxSSE = 700
	provider.ImportUSMaxRPM = 400
	provider.ImportEUMaxSSE = 600
	provider.ImportEUMaxRPM = 350
	updated, err := UpdateSupplierProvider(provider.ID, provider)
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	if updated.ImportUSMaxSSE != 700 || updated.ImportUSMaxRPM != 400 || updated.ImportEUMaxSSE != 600 || updated.ImportEUMaxRPM != 350 {
		t.Fatalf("updated regional limits = %+v", updated)
	}
	byID := make(map[string]Account)
	for _, account := range GetAccounts() {
		byID[account.ID] = account
	}
	if got := byID["us-managed"]; got.MaxSSE != 700 || got.MaxRPM != 400 {
		t.Fatalf("US managed account did not follow provider: %+v", got)
	}
	if got := byID["eu-managed"]; got.MaxSSE != 600 || got.MaxRPM != 350 {
		t.Fatalf("EU managed account did not follow provider: %+v", got)
	}
	if got := byID["us-custom"]; got.MaxSSE != DefaultSupplierImportMaxSSE+1 || got.MaxRPM != DefaultSupplierImportMaxRPM {
		t.Fatalf("US custom account was overwritten: %+v", got)
	}
	if got := byID["eu-custom"]; got.MaxSSE != DefaultSupplierImportMaxSSE || got.MaxRPM != DefaultSupplierImportMaxRPM+1 {
		t.Fatalf("EU custom account was overwritten: %+v", got)
	}

	// A US-only edit must not alter EU defaults or EU accounts.
	updated, err = UpdateSupplierProvider(provider.ID, SupplierProvider{
		Name: updated.Name, BaseURL: updated.BaseURL, Enabled: true, Priority: 2, AutoPurchaseCount: 1,
		ImportUSMaxSSE: 710, ImportUSMaxRPM: 410,
	})
	if err != nil || updated.ImportUSMaxSSE != 710 || updated.ImportUSMaxRPM != 410 || updated.ImportEUMaxSSE != 600 || updated.ImportEUMaxRPM != 350 {
		t.Fatalf("US-only update changed the wrong limits: %+v, %v", updated, err)
	}
	byID = make(map[string]Account)
	for _, account := range GetAccounts() {
		byID[account.ID] = account
	}
	if got := byID["us-managed"]; got.MaxSSE != 710 || got.MaxRPM != 410 {
		t.Fatalf("US-only migration failed: %+v", got)
	}
	if got := byID["eu-managed"]; got.MaxSSE != 600 || got.MaxRPM != 350 {
		t.Fatalf("US-only update changed EU account: %+v", got)
	}

	// An older client that explicitly sends the former shared fields applies
	// them to both regions, preserving the pre-regional API contract.
	updated, err = UpdateSupplierProvider(provider.ID, SupplierProvider{
		Name: updated.Name, BaseURL: updated.BaseURL, Enabled: true, Priority: 3, AutoPurchaseCount: 1,
		ImportMaxSSE: 800, ImportMaxRPM: 450,
	})
	if err != nil || updated.ImportUSMaxSSE != 800 || updated.ImportUSMaxRPM != 450 || updated.ImportEUMaxSSE != 800 || updated.ImportEUMaxRPM != 450 {
		t.Fatalf("legacy shared update was not applied to both regions: %+v, %v", updated, err)
	}
	byID = make(map[string]Account)
	for _, account := range GetAccounts() {
		byID[account.ID] = account
	}
	if got := byID["us-managed"]; got.MaxSSE != 800 || got.MaxRPM != 450 {
		t.Fatalf("legacy shared update did not migrate US managed account: %+v", got)
	}
	if got := byID["eu-managed"]; got.MaxSSE != 800 || got.MaxRPM != 450 {
		t.Fatalf("legacy shared update did not migrate EU managed account: %+v", got)
	}
	if got := byID["us-custom"]; got.MaxSSE != DefaultSupplierImportMaxSSE+1 || got.MaxRPM != DefaultSupplierImportMaxRPM {
		t.Fatalf("legacy shared update overwrote US custom account: %+v", got)
	}
	if got := byID["eu-custom"]; got.MaxSSE != DefaultSupplierImportMaxSSE || got.MaxRPM != DefaultSupplierImportMaxRPM+1 {
		t.Fatalf("legacy shared update overwrote EU custom account: %+v", got)
	}

	// Omitting every import field on an unrelated edit preserves both regions.
	preserved, err := UpdateSupplierProvider(provider.ID, SupplierProvider{
		Name: updated.Name, BaseURL: updated.BaseURL, Enabled: true, Priority: 4, AutoPurchaseCount: 1,
	})
	if err != nil || preserved.ImportUSMaxSSE != 800 || preserved.ImportUSMaxRPM != 450 || preserved.ImportEUMaxSSE != 800 || preserved.ImportEUMaxRPM != 450 {
		t.Fatalf("unrelated update lost regional limits: %+v, %v", preserved, err)
	}
}

func TestSupplierRegionalImportLimitsPersistAndLegacyValuesLoadIntoBothRegions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := AddSupplierProvider(SupplierProvider{
		ID: "regional", Name: "Regional", BaseURL: "https://regional.example", APIToken: "secret", AutoPurchaseCount: 1,
		ImportUSMaxSSE: 720, ImportUSMaxRPM: 410, ImportEUMaxSSE: 610, ImportEUMaxRPM: 340,
	}); err != nil {
		t.Fatalf("add regional provider: %v", err)
	}
	if _, err := AddSupplierProvider(SupplierProvider{
		ID: "legacy-shared", Name: "Legacy shared", BaseURL: "https://legacy-shared.example", APIToken: "secret", AutoPurchaseCount: 1,
		ImportMaxSSE: 660, ImportMaxRPM: 370,
	}); err != nil {
		t.Fatalf("add legacy provider: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("reload Init: %v", err)
	}
	regional := GetSupplierProvider("regional")
	if regional == nil || regional.ImportUSMaxSSE != 720 || regional.ImportUSMaxRPM != 410 || regional.ImportEUMaxSSE != 610 || regional.ImportEUMaxRPM != 340 {
		t.Fatalf("regional limits after reload = %+v", regional)
	}
	legacy := GetSupplierProvider("legacy-shared")
	if legacy == nil || legacy.ImportUSMaxSSE != 660 || legacy.ImportUSMaxRPM != 370 || legacy.ImportEUMaxSSE != 660 || legacy.ImportEUMaxRPM != 370 {
		t.Fatalf("legacy limits after reload = %+v", legacy)
	}
}

func TestMigrateLegacySupplierImportLimitsIsScopedAndIdempotent(t *testing.T) {
	initSupplierTestConfig(t)
	defaultProvider, err := AddSupplierProvider(SupplierProvider{
		ID: "default", Name: "Default", BaseURL: "https://default.example", APIToken: "default-secret", AutoPurchaseCount: 1,
	})
	if err != nil {
		t.Fatalf("add default provider: %v", err)
	}
	customProvider, err := AddSupplierProvider(SupplierProvider{
		ID: "custom", Name: "Custom", BaseURL: "https://custom.example", APIToken: "custom-secret", AutoPurchaseCount: 1,
		ImportUSMaxSSE: 800, ImportUSMaxRPM: 450, ImportEUMaxSSE: 600, ImportEUMaxRPM: 350,
	})
	if err != nil {
		t.Fatalf("add custom provider: %v", err)
	}
	legacyProvider, err := AddSupplierProvider(SupplierProvider{
		ID: "legacy", Name: "Legacy", BaseURL: "https://legacy.example", APIToken: "legacy-secret", AutoPurchaseCount: 1,
		ImportMaxSSE: 300, ImportMaxRPM: 200,
	})
	if err != nil {
		t.Fatalf("add legacy provider: %v", err)
	}
	accounts := []Account{
		{ID: "legacy-default", AuthMethod: "api_key", KiroApiKey: "ksk_legacy_default", AccessToken: "ksk_legacy_default", SupplierID: defaultProvider.ID, MaxSSE: 300, MaxRPM: 200},
		{ID: "legacy-custom-us", AuthMethod: "api_key", KiroApiKey: "ksk_legacy_custom_us", AccessToken: "ksk_legacy_custom_us", SupplierID: customProvider.ID, Region: "us-east-1", MaxSSE: 300, MaxRPM: 200},
		{ID: "legacy-custom-eu", AuthMethod: "api_key", KiroApiKey: "ksk_legacy_custom_eu", AccessToken: "ksk_legacy_custom_eu", SupplierID: customProvider.ID, ApiRegion: "eu-central-1", Region: "us-east-1", MaxSSE: 300, MaxRPM: 200},
		{ID: "manual", AuthMethod: "api_key", KiroApiKey: "ksk_manual", AccessToken: "ksk_manual", MaxSSE: 300, MaxRPM: 200},
		{ID: "overridden", AuthMethod: "api_key", KiroApiKey: "ksk_overridden", AccessToken: "ksk_overridden", SupplierID: defaultProvider.ID, MaxSSE: 301, MaxRPM: 200},
		{ID: "oauth", AuthMethod: "social", AccessToken: "oauth-token", RefreshToken: "oauth-refresh", SupplierID: defaultProvider.ID, MaxSSE: 300, MaxRPM: 200},
		{ID: "intentional-legacy", AuthMethod: "api_key", KiroApiKey: "ksk_intentional_legacy", AccessToken: "ksk_intentional_legacy", SupplierID: legacyProvider.ID, MaxSSE: 300, MaxRPM: 200},
	}
	if added, _, addErr := AddAccounts(accounts); addErr != nil || added != len(accounts) {
		t.Fatalf("AddAccounts = %d, %v", added, addErr)
	}
	migrated, err := MigrateLegacySupplierImportLimits()
	if err != nil || migrated != 3 {
		t.Fatalf("migration = %d, %v", migrated, err)
	}
	byID := make(map[string]Account)
	for _, account := range GetAccounts() {
		byID[account.ID] = account
	}
	if got := byID["legacy-default"]; got.MaxSSE != DefaultSupplierImportMaxSSE || got.MaxRPM != DefaultSupplierImportMaxRPM {
		t.Fatalf("default-provider migration = %+v", got)
	}
	if got := byID["legacy-custom-us"]; got.MaxSSE != 800 || got.MaxRPM != 450 {
		t.Fatalf("custom-provider US migration = %+v", got)
	}
	if got := byID["legacy-custom-eu"]; got.MaxSSE != 600 || got.MaxRPM != 350 {
		t.Fatalf("custom-provider EU migration = %+v", got)
	}
	for _, id := range []string{"manual", "overridden", "oauth", "intentional-legacy"} {
		if got := byID[id]; (id != "overridden" && (got.MaxSSE != 300 || got.MaxRPM != 200)) || (id == "overridden" && (got.MaxSSE != 301 || got.MaxRPM != 200)) {
			t.Fatalf("account %s should be untouched: %+v", id, got)
		}
	}
	if again, againErr := MigrateLegacySupplierImportLimits(); againErr != nil || again != 0 {
		t.Fatalf("idempotent migration = %d, %v", again, againErr)
	}
}

func TestSupplierPublicPurchaseSourceIsValidatedAndPreserved(t *testing.T) {
	initSupplierTestConfig(t)
	created, err := AddSupplierProvider(SupplierProvider{
		ID: "public-vendor", Name: "Public Vendor", BaseURL: "https://public.example", APIToken: "secret",
		APIType: SupplierAPITypeAWSMy, PurchaseSource: SupplierPurchaseSourcePublic, Enabled: true, AutoPurchaseCount: 2,
	})
	if err != nil {
		t.Fatalf("AddSupplierProvider: %v", err)
	}
	if created.PurchaseSource != SupplierPurchaseSourcePublic {
		t.Fatalf("created purchase source = %q", created.PurchaseSource)
	}

	updated, err := UpdateSupplierProvider(created.ID, SupplierProvider{
		Name: "Public Vendor Updated", BaseURL: created.BaseURL, Enabled: true, AutoPurchaseCount: 3,
	})
	if err != nil {
		t.Fatalf("legacy UpdateSupplierProvider: %v", err)
	}
	if updated.APIType != SupplierAPITypeAWSMy || updated.PurchaseSource != SupplierPurchaseSourcePublic {
		t.Fatalf("legacy update lost public source: %+v", updated)
	}

	changed, err := UpdateSupplierProvider(created.ID, SupplierProvider{
		Name: "Kiro Vendor", BaseURL: created.BaseURL, APIType: SupplierAPITypeKiroApp,
		Enabled: true, AutoPurchaseCount: 3,
	})
	if err != nil {
		t.Fatalf("change API protocol: %v", err)
	}
	if changed.PurchaseSource != SupplierPurchaseSourceOwn {
		t.Fatalf("protocol change did not reset purchase source: %+v", changed)
	}

	if _, err := AddSupplierProvider(SupplierProvider{
		ID: "invalid-public", Name: "Invalid Public", BaseURL: "https://invalid.example", APIToken: "secret",
		APIType: SupplierAPITypeKiroApp, PurchaseSource: SupplierPurchaseSourcePublic, AutoPurchaseCount: 1,
	}); err == nil {
		t.Fatal("public source was accepted for KiroApp protocol")
	}
	if _, err := AddSupplierProvider(SupplierProvider{
		ID: "invalid-source", Name: "Invalid Source", BaseURL: "https://invalid.example", APIToken: "secret",
		APIType: SupplierAPITypeAWSMy, PurchaseSource: "shared-ish", AutoPurchaseCount: 1,
	}); err == nil {
		t.Fatal("unknown purchase source was accepted")
	}
}

func TestSupplierEUFallbackIsRegionalOwnInventoryOnly(t *testing.T) {
	initSupplierTestConfig(t)
	regional, err := AddSupplierProvider(SupplierProvider{
		ID: "regional", Name: "Regional", BaseURL: "https://regional.example", APIToken: "secret",
		APIType: SupplierAPITypeKiroDrop, PurchaseSource: SupplierPurchaseSourceOwn,
		Enabled: true, AutoPurchaseCount: 2, AllowEUFallback: true,
	})
	if err != nil || !regional.AllowEUFallback || !SupplierSupportsEUFallback(regional) {
		t.Fatalf("regional EU fallback = %+v, %v", regional, err)
	}
	for _, provider := range []SupplierProvider{
		{ID: "aws-own", Name: "AWS Own", BaseURL: "https://aws-own.example", APIToken: "secret", APIType: SupplierAPITypeAWSMy, PurchaseSource: SupplierPurchaseSourceOwn, AutoPurchaseCount: 1, AllowEUFallback: true},
		{ID: "aws-public", Name: "AWS Public", BaseURL: "https://aws-public.example", APIToken: "secret", APIType: SupplierAPITypeAWSMy, PurchaseSource: SupplierPurchaseSourcePublic, AutoPurchaseCount: 1, AllowEUFallback: true},
	} {
		if _, err := AddSupplierProvider(provider); err == nil {
			t.Fatalf("unsupported EU fallback was accepted: %+v", provider)
		}
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

func TestKiroDropProviderPreservesWriteOnlyWebhookSecret(t *testing.T) {
	initSupplierTestConfig(t)
	created, err := AddSupplierProvider(SupplierProvider{
		ID: "kiro-drop", Name: "Kiro Drop", BaseURL: "https://drop.kiro.ss", APIToken: "usr-secret",
		APIType: SupplierAPITypeKiroDrop, PurchaseSource: SupplierPurchaseSourceOwn, Enabled: true, AutoPurchaseCount: 5,
	})
	if err != nil {
		t.Fatalf("AddSupplierProvider: %v", err)
	}
	if created.APIType != SupplierAPITypeKiroDrop {
		t.Fatalf("created API type = %q", created.APIType)
	}
	secret := "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
	if err := UpdateSupplierWebhookSecret(created.ID, secret); err != nil {
		t.Fatalf("UpdateSupplierWebhookSecret: %v", err)
	}
	updated, err := UpdateSupplierProvider(created.ID, SupplierProvider{
		Name: "Kiro Drop Renamed", BaseURL: created.BaseURL, Enabled: true, Priority: 1, AutoPurchaseCount: 6,
	})
	if err != nil {
		t.Fatalf("UpdateSupplierProvider: %v", err)
	}
	if updated.APIType != SupplierAPITypeKiroDrop || updated.WebhookSecret != secret || updated.APIToken != "usr-secret" {
		t.Fatalf("write-only Kiro Drop settings were not preserved: %+v", updated)
	}
	changedConnection, err := UpdateSupplierProvider(created.ID, SupplierProvider{
		Name: updated.Name, BaseURL: "https://new-drop.example", APIType: SupplierAPITypeKiroDrop,
		Enabled: true, Priority: 1, AutoPurchaseCount: 6,
	})
	if err != nil {
		t.Fatalf("change Kiro Drop connection: %v", err)
	}
	if changedConnection.WebhookSecret != "" {
		t.Fatalf("connection change retained a stale webhook secret: %+v", changedConnection)
	}
	if _, err := AddSupplierProvider(SupplierProvider{
		ID: "drop-public", Name: "Invalid", BaseURL: "https://drop.example", APIToken: "usr-secret",
		APIType: SupplierAPITypeKiroDrop, PurchaseSource: SupplierPurchaseSourcePublic, AutoPurchaseCount: 1,
	}); err == nil {
		t.Fatal("public source was accepted for Kiro Drop")
	}
	if err := UpdateSupplierWebhookSecret(created.ID, "not-hex"); err == nil {
		t.Fatal("invalid webhook secret was accepted")
	}
}

func TestKiroCEOProviderUsesDedicatedOwnInventoryProtocol(t *testing.T) {
	initSupplierTestConfig(t)
	created, err := AddSupplierProvider(SupplierProvider{
		ID: "kiro-ceo", Name: "Kiro CEO", BaseURL: "https://kiro.ceo", APIToken: "ceo-secret",
		APIType: SupplierAPITypeKiroCEO, PurchaseSource: SupplierPurchaseSourceOwn, Enabled: true, AutoPurchaseCount: 5,
	})
	if err != nil || created.APIType != SupplierAPITypeKiroCEO || created.PurchaseSource != SupplierPurchaseSourceOwn {
		t.Fatalf("AddSupplierProvider = %+v, %v", created, err)
	}
	if _, err := AddSupplierProvider(SupplierProvider{
		ID: "kiro-ceo-public", Name: "Invalid", BaseURL: "https://kiro.ceo", APIToken: "ceo-secret",
		APIType: SupplierAPITypeKiroCEO, PurchaseSource: SupplierPurchaseSourcePublic, AutoPurchaseCount: 1,
	}); err == nil {
		t.Fatal("public source was accepted for Kiro CEO")
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

func TestSupplierProviderPollIntervalIsIndependentPreciseAndCompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	fast, err := AddSupplierProvider(SupplierProvider{
		ID: "fast", Name: "Fast", BaseURL: "https://fast.example", APIToken: "secret", AutoPurchaseCount: 1,
		PollIntervalSeconds: 0.1,
	})
	if err != nil || fast.PollIntervalSeconds != 0.1 {
		t.Fatalf("fast provider = %+v, %v", fast, err)
	}
	legacy, err := AddSupplierProvider(SupplierProvider{
		ID: "legacy", Name: "Legacy", BaseURL: "https://legacy.example", APIToken: "secret", AutoPurchaseCount: 1,
	})
	if err != nil {
		t.Fatalf("legacy provider: %v", err)
	}
	if got := EffectiveSupplierProviderPollIntervalSeconds(legacy, 7); got != 7 {
		t.Fatalf("legacy effective interval = %v, want 7", got)
	}
	ceo, err := AddSupplierProvider(SupplierProvider{
		ID: "ceo", Name: "CEO", BaseURL: "https://ceo.example", APIToken: "secret", APIType: SupplierAPITypeKiroCEO, AutoPurchaseCount: 1,
		PollIntervalSeconds: 0.1,
	})
	if err != nil || EffectiveSupplierProviderPollIntervalSeconds(ceo, 1) != 0.1 {
		t.Fatalf("CEO provider = %+v, %v", ceo, err)
	}
	for _, provider := range []SupplierProvider{
		{ID: "too-fast", Name: "x", BaseURL: "https://x.example", APIToken: "secret", AutoPurchaseCount: 1, PollIntervalSeconds: 0.09},
		{ID: "too-slow", Name: "x", BaseURL: "https://x.example", APIToken: "secret", AutoPurchaseCount: 1, PollIntervalSeconds: 301},
	} {
		if _, err := AddSupplierProvider(provider); err == nil {
			t.Fatalf("invalid provider interval succeeded: %+v", provider)
		}
	}
	if err := Init(path); err != nil {
		t.Fatalf("reload Init: %v", err)
	}
	if got := GetSupplierProvider("fast"); got == nil || got.PollIntervalSeconds != 0.1 {
		t.Fatalf("fast interval after reload = %+v", got)
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
		{ID: "vendor", Name: "x", BaseURL: "https://example.com", APIToken: "km_x", AutoPurchaseCount: 1, ImportMaxSSE: -1},
		{ID: "vendor", Name: "x", BaseURL: "https://example.com", APIToken: "km_x", AutoPurchaseCount: 1, ImportMaxRPM: -1},
		{ID: "vendor", Name: "x", BaseURL: "https://example.com", APIToken: "km_x", AutoPurchaseCount: 1, ImportUSMaxSSE: -1},
		{ID: "vendor", Name: "x", BaseURL: "https://example.com", APIToken: "km_x", AutoPurchaseCount: 1, ImportUSMaxRPM: -1},
		{ID: "vendor", Name: "x", BaseURL: "https://example.com", APIToken: "km_x", AutoPurchaseCount: 1, ImportEUMaxSSE: -1},
		{ID: "vendor", Name: "x", BaseURL: "https://example.com", APIToken: "km_x", AutoPurchaseCount: 1, ImportEUMaxRPM: -1},
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
