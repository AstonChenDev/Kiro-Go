package pool

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
)

func newPolicyTestPool(t *testing.T, accounts ...config.Account) *AccountPool {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, account := range accounts {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("config.AddAccount(%s): %v", account.ID, err)
		}
	}
	p := &AccountPool{}
	p.Reload()
	return p
}

func TestTypeModelPolicyFiltersDuringColdStart(t *testing.T) {
	p := newPolicyTestPool(t, config.Account{
		ID:               "free-account",
		Enabled:          true,
		AccessToken:      "token",
		SubscriptionType: config.AccountTypeFree,
	})
	if err := config.SetAccountTypePolicy(config.AccountTypeFree, config.AccountTypePolicy{
		AllowedModels: []string{"claude-sonnet-4.6"},
	}); err != nil {
		t.Fatalf("SetAccountTypePolicy: %v", err)
	}
	p.Reload()

	if got := p.GetNextForModel("claude-opus-4.8"); got != nil {
		t.Fatalf("disallowed model selected account: %+v", got)
	}
	if got := p.GetNextForModel("CLAUDE-SONNET-4.6"); got == nil || got.ID != "free-account" {
		t.Fatalf("allowed model did not select account: %+v", got)
	}
}

func TestAccountModelOverrideWinsAndIntersectsCapabilityCache(t *testing.T) {
	p := newPolicyTestPool(t, config.Account{
		ID:                  "override-account",
		Enabled:             true,
		AccessToken:         "token",
		SubscriptionType:    config.AccountTypeFree,
		ModelPolicyOverride: true,
		AllowedModels:       []string{"claude-opus-4.8", "claude-haiku-4.5"},
	})
	if err := config.SetAccountTypePolicy(config.AccountTypeFree, config.AccountTypePolicy{
		AllowedModels: []string{"claude-sonnet-4.6"},
	}); err != nil {
		t.Fatalf("SetAccountTypePolicy: %v", err)
	}
	p.Reload()
	p.SetModelList("override-account", []string{"claude-opus-4.8"})

	if got := p.GetNextForModel("claude-sonnet-4.6"); got != nil {
		t.Fatalf("type model must not bypass account override: %+v", got)
	}
	if got := p.GetNextForModel("claude-haiku-4.5"); got != nil {
		t.Fatalf("manual allow-list must not bypass upstream capability: %+v", got)
	}
	if got := p.GetNextForModel("claude-opus-4.8"); got == nil || got.ID != "override-account" {
		t.Fatalf("intersection model was not routed: %+v", got)
	}
}

func TestTypeConcurrencyDefaultsAndPerAccountOverride(t *testing.T) {
	p := newPolicyTestPool(t,
		config.Account{ID: "inherits", Enabled: true, AccessToken: "token-1", SubscriptionType: config.AccountTypePro},
		config.Account{ID: "overrides", Enabled: true, AccessToken: "token-2", SubscriptionType: config.AccountTypePro, MaxSSE: 2, MaxRPM: 3},
	)
	if err := config.SetAccountTypePolicy(config.AccountTypePro, config.AccountTypePolicy{MaxSSE: 1, MaxRPM: 2}); err != nil {
		t.Fatalf("SetAccountTypePolicy: %v", err)
	}
	p.Reload()

	if !p.Acquire("inherits", true) {
		t.Fatal("first inherited stream should succeed")
	}
	if p.Acquire("inherits", true) {
		t.Fatal("second inherited stream should hit type maxSSE=1")
	}
	p.Release("inherits", true)
	p.reqTimestamps["inherits"] = nil
	if !p.Acquire("inherits", false) || !p.Acquire("inherits", false) {
		t.Fatal("two inherited RPM slots should succeed")
	}
	if p.Acquire("inherits", false) {
		t.Fatal("third inherited request should hit type maxRPM=2")
	}

	p.reqTimestamps["overrides"] = nil
	if !p.Acquire("overrides", true) || !p.Acquire("overrides", true) {
		t.Fatal("account maxSSE=2 should override type maxSSE=1")
	}
	if p.Acquire("overrides", true) {
		t.Fatal("third override stream should fail")
	}
	p.Release("overrides", true)
	p.Release("overrides", true)
	p.reqTimestamps["overrides"] = nil
	if !p.Acquire("overrides", false) || !p.Acquire("overrides", false) || !p.Acquire("overrides", false) {
		t.Fatal("account maxRPM=3 should override type maxRPM=2")
	}
	if p.Acquire("overrides", false) {
		t.Fatal("fourth override request should fail")
	}
}
