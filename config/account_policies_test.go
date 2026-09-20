package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestAccountTypePolicyResolutionAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	policy := AccountTypePolicy{
		AllowedModels: []string{" Claude-Opus-4.8 ", "claude-sonnet-4.6", "claude-opus-4.8"},
		MaxSSE:        7,
		MaxRPM:        23,
	}
	if err := SetAccountTypePolicy(AccountTypePro, policy); err != nil {
		t.Fatalf("SetAccountTypePolicy: %v", err)
	}

	account := Account{ID: "pro-account", SubscriptionType: "Kiro Pro"}
	models, source, restricted := EffectiveAllowedModels(account)
	if !restricted || source != "subscription_type" {
		t.Fatalf("model policy = restricted:%v source:%q", restricted, source)
	}
	wantModels := []string{"claude-opus-4.8", "claude-sonnet-4.6"}
	if !reflect.DeepEqual(models, wantModels) {
		t.Fatalf("models = %#v, want %#v", models, wantModels)
	}
	limits := EffectiveAccountLimits(account)
	if limits.MaxSSE != 7 || limits.MaxRPM != 23 || limits.SSESource != "subscription_type" || limits.RPMSource != "subscription_type" {
		t.Fatalf("limits = %+v", limits)
	}

	if err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := GetAccountTypePolicies()[AccountTypePro]
	if !reflect.DeepEqual(got.AllowedModels, wantModels) || got.MaxSSE != 7 || got.MaxRPM != 23 {
		t.Fatalf("persisted policy = %+v", got)
	}
	if err := SetAccountTypePolicy(AccountTypePro, AccountTypePolicy{}); err != nil {
		t.Fatalf("reset policy: %v", err)
	}
	models, source, restricted = EffectiveAllowedModels(account)
	if restricted || source != "system" || len(models) != 0 {
		t.Fatalf("reset effective models = %#v source=%q restricted=%v", models, source, restricted)
	}
}

func TestPerAccountPolicyOverridesTypePolicyIndependently(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := SetAccountTypePolicy(AccountTypeFree, AccountTypePolicy{
		AllowedModels: []string{"claude-sonnet-4.6"},
		MaxSSE:        4,
		MaxRPM:        12,
	}); err != nil {
		t.Fatalf("SetAccountTypePolicy: %v", err)
	}
	account := Account{
		SubscriptionType:    AccountTypeFree,
		ModelPolicyOverride: true,
		AllowedModels:       []string{"CLAUDE-OPUS-4.8"},
		MaxSSE:              9,
	}
	if err := ValidateAccountRoutingPolicy(&account); err != nil {
		t.Fatalf("ValidateAccountRoutingPolicy: %v", err)
	}
	models, source, restricted := EffectiveAllowedModels(account)
	if !restricted || source != "account" || !reflect.DeepEqual(models, []string{"claude-opus-4.8"}) {
		t.Fatalf("effective models = %#v source=%q restricted=%v", models, source, restricted)
	}
	limits := EffectiveAccountLimits(account)
	if limits.MaxSSE != 9 || limits.SSESource != "account" || limits.MaxRPM != 12 || limits.RPMSource != "subscription_type" {
		t.Fatalf("limits = %+v", limits)
	}
}

func TestAccountPolicyValidationRejectsInvalidLimits(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := SetAccountTypePolicy(AccountTypeFree, AccountTypePolicy{MaxSSE: -1}); err == nil {
		t.Fatal("expected negative type limit to fail")
	}
	account := Account{MaxRPM: -1}
	if err := ValidateAccountRoutingPolicy(&account); err == nil {
		t.Fatal("expected negative account limit to fail")
	}
	if err := SetAccountTypePolicy("UNKNOWN_TYPE", AccountTypePolicy{}); err == nil {
		t.Fatal("expected unsupported account type to fail")
	}
}

func TestCredentialTypeClassification(t *testing.T) {
	tests := []struct {
		name    string
		account Account
		want    string
		ok      bool
	}{
		{"builder provider", Account{Provider: "BuilderId", AuthMethod: "idc"}, CredentialTypeBuilderID, true},
		{"enterprise provider", Account{Provider: "Enterprise"}, CredentialTypeEnterprise, true},
		{"idc fallback", Account{AuthMethod: "idc"}, CredentialTypeEnterprise, true},
		{"microsoft", Account{Provider: "AzureAD", AuthMethod: "external_idp"}, CredentialTypeEnterprise, true},
		{"google", Account{Provider: "Google", AuthMethod: "social"}, CredentialTypeGoogle, true},
		{"github", Account{Provider: "GitHub", AuthMethod: "social"}, CredentialTypeGitHub, true},
		{"api key", Account{AuthMethod: "api_key", KiroApiKey: "ksk_test"}, CredentialTypeAPIKey, true},
		{"ambiguous social", Account{Provider: "Kiro SSO", AuthMethod: "social"}, "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := CredentialTypeFor(test.account)
			if got != test.want || ok != test.ok {
				t.Fatalf("CredentialTypeFor() = %q, %v; want %q, %v", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestCredentialPolicyOverridesSubscriptionPerField(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := SetAccountTypePolicy(AccountTypePro, AccountTypePolicy{
		AllowedModels: []string{"claude-sonnet-4.6"}, MaxSSE: 4, MaxRPM: 12,
	}); err != nil {
		t.Fatalf("set subscription policy: %v", err)
	}
	if err := SetAccountTypePolicy(CredentialTypeGitHub, AccountTypePolicy{
		AllowedModels: []string{"gpt-5.6-sol"}, MaxSSE: 8,
	}); err != nil {
		t.Fatalf("set credential policy: %v", err)
	}
	account := Account{SubscriptionType: AccountTypePro, Provider: "GitHub", AuthMethod: "social"}
	models, source, restricted := EffectiveAllowedModels(account)
	if !restricted || source != "credential_type" || !reflect.DeepEqual(models, []string{"gpt-5.6-sol"}) {
		t.Fatalf("models=%#v source=%q restricted=%v", models, source, restricted)
	}
	limits := EffectiveAccountLimits(account)
	if limits.MaxSSE != 8 || limits.SSESource != "credential_type" || limits.MaxRPM != 12 || limits.RPMSource != "subscription_type" {
		t.Fatalf("limits = %+v", limits)
	}
}

func TestExplicitEmptyAccountModelOverrideBlocksAllModels(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	account := Account{ModelPolicyOverride: true}
	models, source, restricted := EffectiveAllowedModels(account)
	if !restricted || source != "account" || len(models) != 0 {
		t.Fatalf("models=%#v source=%q restricted=%v", models, source, restricted)
	}
}
