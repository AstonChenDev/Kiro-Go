package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeAPIKeyAccountPipeRegionAndMachineId(t *testing.T) {
	account := Account{
		KiroApiKey: " ksk_test_key|eu-central-1 ",
		AuthMethod: "API KEY",
	}
	if err := NormalizeAPIKeyAccount(&account); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if account.KiroApiKey != "ksk_test_key" {
		t.Fatalf("key = %q", account.KiroApiKey)
	}
	if account.AccessToken != "ksk_test_key" {
		t.Fatalf("accessToken should mirror api key, got %q", account.AccessToken)
	}
	if account.AuthMethod != "api_key" {
		t.Fatalf("authMethod = %q", account.AuthMethod)
	}
	if account.Region != "eu-central-1" {
		t.Fatalf("region = %q", account.Region)
	}
	if account.RefreshToken != "" || account.ProfileArn != "" || account.ExpiresAt != 0 {
		t.Fatalf("oauth fields should be cleared: %+v", account)
	}
	wantMachine := MachineIdFromAPIKey("ksk_test_key")
	if account.MachineId != wantMachine {
		t.Fatalf("machineId = %q, want %q", account.MachineId, wantMachine)
	}
	if !IsAPIKeyAccount(&account) {
		t.Fatal("expected IsAPIKeyAccount true")
	}
}

func TestAddAccountRejectsDuplicateAPIKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	first := Account{ID: "api-1", KiroApiKey: "ksk_dup", AuthMethod: "api_key", Enabled: true}
	if err := AddAccount(first); err != nil {
		t.Fatalf("add first: %v", err)
	}
	second := Account{ID: "api-2", KiroApiKey: "ksk_dup", AuthMethod: "api_key", Enabled: true}
	if err := AddAccount(second); err != ErrDuplicateAPIKey {
		t.Fatalf("expected ErrDuplicateAPIKey, got %v", err)
	}
}

func TestAddAccountsNormalizesAPIKeyRegions(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	added, skipped, err := AddAccounts([]Account{{
		ID:         "api-batch-1",
		KiroApiKey: "ksk_batch",
		AuthMethod: " API_KEY ",
		Region:     " US-EAST-1 ",
		AuthRegion: " US-WEST-2 ",
		ApiRegion:  " EU-CENTRAL-1 ",
	}})
	if err != nil {
		t.Fatalf("AddAccounts: %v", err)
	}
	if added != 1 || skipped != 0 {
		t.Fatalf("added=%d skipped=%d", added, skipped)
	}
	got := GetAccounts()[0]
	if got.Region != "us-east-1" || got.AuthRegion != "us-west-2" || got.ApiRegion != "eu-central-1" {
		t.Fatalf("regions were not normalized: %+v", got)
	}
	if got.AccessToken != "ksk_batch" || got.AuthMethod != "api_key" {
		t.Fatalf("API-key account was not normalized: %+v", got)
	}
}

func TestAddAccountsRejectsUnsafeRegionWithoutMutation(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_, _, err := AddAccounts([]Account{
		{ID: "valid", RefreshToken: "refresh-valid", Region: "us-east-1"},
		{ID: "unsafe", RefreshToken: "refresh-unsafe", AuthRegion: unsafeConfigRegion},
	})
	if err == nil {
		t.Fatal("expected unsafe batch region to be rejected")
	}
	if got := len(GetAccounts()); got != 0 {
		t.Fatalf("batch mutated config before validation completed: %d accounts", got)
	}
}

const unsafeConfigRegion = "us-east-1.amazonaws.com@attacker.test/x"

func TestSplitKiroAPIKeyAndRegionValidation(t *testing.T) {
	key, region, err := SplitKiroAPIKeyAndRegion("ksk_abc|us-east-1")
	if err != nil || key != "ksk_abc" || region != "us-east-1" {
		t.Fatalf("got key=%q region=%q err=%v", key, region, err)
	}
	if _, _, err := SplitKiroAPIKeyAndRegion("ksk_abc|us-east-1|extra"); err == nil {
		t.Fatal("expected multi-pipe error")
	}
	if _, _, err := SplitKiroAPIKeyAndRegion("|us-east-1"); err == nil {
		t.Fatal("expected empty key error")
	}
	if _, _, err := SplitKiroAPIKeyAndRegion("ksk_abc|" + unsafeConfigRegion); err == nil {
		t.Fatal("expected host-injection region to be rejected")
	}
}

func TestNormalizeAWSRegion(t *testing.T) {
	for raw, want := range map[string]string{
		" us-east-1 ":     "us-east-1",
		"US-GOV-WEST-1":   "us-gov-west-1",
		"eusc-de-east-1":  "eusc-de-east-1",
		"ap-southeast-12": "ap-southeast-12",
	} {
		got, err := NormalizeAWSRegion(raw)
		if err != nil {
			t.Fatalf("NormalizeAWSRegion(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("NormalizeAWSRegion(%q) = %q, want %q", raw, got, want)
		}
	}

	for _, raw := range []string{
		"us-east-1.amazonaws.com",
		unsafeConfigRegion,
		"us_east_1",
		"-us-east-1",
		"us-east",
		"us--east-1",
	} {
		if _, err := NormalizeAWSRegion(raw); err == nil {
			t.Fatalf("NormalizeAWSRegion(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestNormalizeAPIKeyAccountValidatesAllRegionFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Account)
	}{
		{"region", func(a *Account) { a.Region = "us-east-1/unsafe" }},
		{"authRegion", func(a *Account) { a.AuthRegion = unsafeConfigRegion }},
		{"apiRegion", func(a *Account) { a.ApiRegion = "runtime.attacker.test/x" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := Account{KiroApiKey: "ksk_test"}
			tc.mutate(&account)
			if err := NormalizeAPIKeyAccount(&account); err == nil {
				t.Fatal("expected unsafe region to be rejected")
			}
		})
	}
}

func TestUpdateSettingsPatchPreservesOmittedAPIKeyFields(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := UpdateSettingsPatch(nil, nil, "new-admin-password"); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "proxy-api-key" {
		t.Fatalf("expected API key to be preserved, got %q", got)
	}
	if !IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to stay enabled")
	}
	if got := GetPassword(); got != "new-admin-password" {
		t.Fatalf("expected password to update, got %q", got)
	}
}

func TestUpdateSettingsPatchCanExplicitlyDisableAPIKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	emptyKey := ""
	requireAPIKey := false
	if err := UpdateSettingsPatch(&emptyKey, &requireAPIKey, ""); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "" {
		t.Fatalf("expected API key to be cleared, got %q", got)
	}
	if IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to be disabled")
	}
	if got := GetPassword(); got != "admin-password" {
		t.Fatalf("expected password to be preserved, got %q", got)
	}
}

func TestUpdateAccountStaleSnapshotPreservesCredentialRotation(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := Account{
		ID:            "rotation-account",
		AccessToken:   "access-1",
		RefreshToken:  "refresh-1",
		ClientID:      "client",
		AuthMethod:    "external_idp",
		Region:        "us-east-1",
		AuthRegion:    "us-west-2",
		ApiRegion:     "eu-central-1",
		ExpiresAt:     100,
		ProfileArn:    "arn:aws:codewhisperer:us-east-1:123456789012:profile/one",
		TokenEndpoint: "https://login.microsoftonline.com/tenant/oauth2/v2.0/token",
		IssuerURL:     "https://login.microsoftonline.com/tenant/v2.0",
		Scopes:        "scope-one",
		Enabled:       true,
	}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	stale := GetAccounts()[0]

	const rotatedProfile = "arn:aws:codewhisperer:eu-central-1:123456789012:profile/two"
	if err := UpdateAccountCredentialState(
		account.ID,
		"access-2",
		"refresh-2",
		200,
		rotatedProfile,
	); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}

	stale.Enabled = false
	stale.BanStatus = "BANNED"
	stale.BanReason = "stale status update"
	stale.AuthRegion = "ap-south-1"
	stale.ApiRegion = "ap-southeast-2"
	if err := UpdateAccount(account.ID, stale); err != nil {
		t.Fatalf("apply stale status snapshot: %v", err)
	}

	got := GetAccounts()[0]
	if got.AccessToken != "access-2" ||
		got.RefreshToken != "refresh-2" ||
		got.ExpiresAt != 200 ||
		got.ProfileArn != rotatedProfile {
		t.Fatalf("stale status update reverted credential state: %+v", got)
	}
	if got.RefreshTokenFingerprint != RefreshTokenFingerprint("refresh-1") {
		t.Fatalf("original refresh token fingerprint = %q", got.RefreshTokenFingerprint)
	}
	if got.AuthRegion != "us-west-2" || got.ApiRegion != "eu-central-1" {
		t.Fatalf("stale status update overwrote specialized regions: %+v", got)
	}
	if got.Enabled || got.BanStatus != "BANNED" || got.BanReason != "stale status update" {
		t.Fatalf("status fields were not applied: %+v", got)
	}
}

// TestAccountAllowOverageMigration verifies that a config.json from before the
// upstream-Overages-switch refactor (which carried `allowOverage: true` per
// account) is migrated into OverageStatus="ENABLED" on first load, and that
// the legacy field is cleared so future saves don't re-emit it.
func TestAccountAllowOverageMigration(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"requireApiKey": false,
		"accounts": []map[string]interface{}{
			{"id": "acc-allow", "enabled": true, "allowOverage": true},
			{"id": "acc-deny", "enabled": true, "allowOverage": false},
			{"id": "acc-already-set", "enabled": true, "allowOverage": true, "overageStatus": "DISABLED"},
		},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	accounts := GetAccounts()
	byID := map[string]Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}

	if got := byID["acc-allow"].OverageStatus; got != "ENABLED" {
		t.Fatalf("expected acc-allow to migrate to OverageStatus=ENABLED, got %q", got)
	}
	if byID["acc-allow"].LegacyAllowOverage {
		t.Fatalf("expected legacy allowOverage to be cleared after migration")
	}
	if got := byID["acc-deny"].OverageStatus; got != "" {
		t.Fatalf("expected acc-deny to keep empty OverageStatus, got %q", got)
	}
	// Pre-set OverageStatus must win over the legacy field.
	if got := byID["acc-already-set"].OverageStatus; got != "DISABLED" {
		t.Fatalf("expected acc-already-set OverageStatus to be preserved, got %q", got)
	}
	if byID["acc-already-set"].LegacyAllowOverage {
		t.Fatalf("expected legacy field to still be cleared on acc-already-set")
	}

	// Re-read the file and confirm legacy field is gone (so it doesn't drift
	// back in on later saves).
	on_disk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var reloaded struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(on_disk, &reloaded); err != nil {
		t.Fatalf("decode reload: %v", err)
	}
	for _, a := range reloaded.Accounts {
		if _, ok := a["allowOverage"]; ok {
			t.Fatalf("expected allowOverage to be omitted from persisted file, got %+v", a)
		}
	}
}

func TestIsApiKeyCredential(t *testing.T) {
	cases := []struct {
		name string
		a    Account
		want bool
	}{
		{"key present", Account{KiroApiKey: "k"}, true},
		{"api_key lower", Account{AuthMethod: "api_key"}, true},
		{"apikey lower", Account{AuthMethod: "apikey"}, true},
		{"API_KEY upper", Account{AuthMethod: "API_KEY"}, true},
		{"api_key padded", Account{AuthMethod: " api_key "}, true},
		{"key whitespace only", Account{KiroApiKey: "   "}, false},
		{"key wins over idc", Account{KiroApiKey: "k", AuthMethod: "idc"}, true},
		{"idc not api key", Account{AuthMethod: "idc"}, false},
		{"external_idp not api key", Account{AuthMethod: "external_idp"}, false},
		{"empty", Account{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.IsApiKeyCredential(); got != tc.want {
				t.Fatalf("IsApiKeyCredential() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEffectiveRegionsFallbackChain(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}

	// account-level wins
	a := &Account{AuthRegion: "eu-west-1", ApiRegion: "ap-southeast-1", Region: "us-west-2"}
	if got := a.EffectiveAuthRegion(); got != "eu-west-1" {
		t.Fatalf("auth: want eu-west-1, got %q", got)
	}
	if got := a.EffectiveApiRegion(); got != "ap-southeast-1" {
		t.Fatalf("api: want ap-southeast-1, got %q", got)
	}

	// falls back to Region
	b := &Account{Region: "us-west-2"}
	if got := b.EffectiveAuthRegion(); got != "us-west-2" {
		t.Fatalf("auth fallback: want us-west-2, got %q", got)
	}
	if got := b.EffectiveApiRegion(); got != "us-west-2" {
		t.Fatalf("api fallback: want us-west-2, got %q", got)
	}

	// empty account → default us-east-1
	c := &Account{}
	if got := c.EffectiveAuthRegion(); got != "us-east-1" {
		t.Fatalf("auth default: want us-east-1, got %q", got)
	}
	if got := c.EffectiveApiRegion(); got != "us-east-1" {
		t.Fatalf("api default: want us-east-1, got %q", got)
	}
}

func TestEffectiveRegionsHonorExplicitGlobalUSEast1(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{
		"password":"p",
		"port":8080,
		"host":"127.0.0.1",
		"region":"eu-west-1",
		"authRegion":"us-east-1",
		"apiRegion":"us-east-1",
		"accounts":[]
	}`)
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	account := &Account{}
	if got := account.EffectiveAuthRegion(); got != "us-east-1" {
		t.Fatalf("auth specialized region lost to global Region: %q", got)
	}
	if got := account.EffectiveApiRegion(); got != "us-east-1" {
		t.Fatalf("api specialized region lost to global Region: %q", got)
	}
}

func TestUpdateRegionSettingsValidatesBeforePersist(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := UpdateRegionSettings(" EU-WEST-1 ", " US-EAST-1 ", " AP-SOUTH-1 "); err != nil {
		t.Fatalf("UpdateRegionSettings: %v", err)
	}
	if got := Get().Region; got != "eu-west-1" {
		t.Fatalf("region = %q", got)
	}
	if got := Get().AuthRegion; got != "us-east-1" {
		t.Fatalf("authRegion = %q", got)
	}
	if got := Get().ApiRegion; got != "ap-south-1" {
		t.Fatalf("apiRegion = %q", got)
	}

	if err := UpdateRegionSettings("us-west-2", unsafeConfigRegion, "eu-central-1"); err == nil {
		t.Fatal("expected unsafe global region update to fail")
	}
	if Get().Region != "eu-west-1" || Get().AuthRegion != "us-east-1" || Get().ApiRegion != "ap-south-1" {
		t.Fatalf("failed update mutated global regions: %+v", Get())
	}
}

func TestLoadNormalizesRegionsAndRejectsUnsafeValues(t *testing.T) {
	t.Run("normalizes", func(t *testing.T) {
		cfgFile := filepath.Join(t.TempDir(), "config.json")
		raw := []byte(`{
			"region":" EU-WEST-1 ",
			"authRegion":" US-EAST-1 ",
			"apiRegion":" AP-SOUTHEAST-1 ",
			"accounts":[{
				"id":"account-1",
				"authMethod":"idc",
				"region":" US-WEST-2 ",
				"authRegion":" EU-CENTRAL-1 ",
				"apiRegion":" AP-SOUTH-1 "
			}]
		}`)
		if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if err := Init(cfgFile); err != nil {
			t.Fatalf("init: %v", err)
		}
		got := GetAccounts()[0]
		if got.Region != "us-west-2" || got.AuthRegion != "eu-central-1" || got.ApiRegion != "ap-south-1" {
			t.Fatalf("account regions were not canonicalized: %+v", got)
		}
		if Get().Region != "eu-west-1" || Get().AuthRegion != "us-east-1" || Get().ApiRegion != "ap-southeast-1" {
			t.Fatalf("global regions were not canonicalized: %+v", Get())
		}
	})

	t.Run("rejects unsafe account authRegion", func(t *testing.T) {
		cfgFile := filepath.Join(t.TempDir(), "config.json")
		raw := []byte(`{
			"accounts":[{
				"id":"account-1",
				"authMethod":"idc",
				"authRegion":"us-east-1.amazonaws.com@attacker.test/x"
			}]
		}`)
		if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if err := Init(cfgFile); err == nil {
			t.Fatal("expected unsafe persisted authRegion to be rejected")
		}
	})
}

func TestMaxPayloadBytesDefaultFallbackAndPersist(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := GetMaxPayloadBytes(); got != DefaultMaxPayloadBytes {
		t.Fatalf("default: want %d, got %d", DefaultMaxPayloadBytes, got)
	}
	if err := UpdateMaxPayloadBytes(2100000); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := GetMaxPayloadBytes(); got != 2100000 {
		t.Fatalf("after set: want 2100000, got %d", got)
	}
}
