package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountTypePolicyAdminRoundTrip(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:               "pro-account",
		Enabled:          true,
		AccessToken:      "token",
		SubscriptionType: config.AccountTypePro,
		Provider:         "GitHub",
		AuthMethod:       "social",
	}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.ResetTransientState()
	p.Reload()
	h := &Handler{pool: p}

	update := httptest.NewRecorder()
	h.apiUpdateAccountTypePolicy(update, httptest.NewRequest(
		http.MethodPut,
		"/admin/api/account-type-policies/PRO",
		strings.NewReader(`{"allowedModels":["Claude-Opus-4.8"],"maxSSE":6,"maxRPM":18}`),
	), config.AccountTypePro)
	if update.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", update.Code, update.Body.String())
	}

	get := httptest.NewRecorder()
	h.apiGetAccountTypePolicies(get, httptest.NewRequest(http.MethodGet, "/admin/api/account-type-policies", nil))
	var response struct {
		Types []accountTypePolicyResponse `json:"types"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Types) != len(config.SupportedAccountPolicyCategories) {
		t.Fatalf("policy categories=%d, want %d", len(response.Types), len(config.SupportedAccountPolicyCategories))
	}
	var pro, github *accountTypePolicyResponse
	for i := range response.Types {
		if response.Types[i].Type == config.AccountTypePro {
			pro = &response.Types[i]
		}
		if response.Types[i].Type == config.CredentialTypeGitHub {
			github = &response.Types[i]
		}
	}
	if pro == nil || pro.Group != config.PolicyGroupSubscription || pro.AccountCount != 1 || pro.MaxSSE != 6 || pro.MaxRPM != 18 || len(pro.AllowedModels) != 1 || pro.AllowedModels[0] != "claude-opus-4.8" {
		t.Fatalf("PRO policy = %+v", pro)
	}
	if github == nil || github.Group != config.PolicyGroupCredential || github.AccountCount != 1 {
		t.Fatalf("GitHub policy category = %+v", github)
	}
}

func TestAccountUpdateAndListExposeEffectivePolicy(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.SetAccountTypePolicy(config.AccountTypeFree, config.AccountTypePolicy{
		AllowedModels: []string{"claude-sonnet-4.6"}, MaxSSE: 4, MaxRPM: 14,
	}); err != nil {
		t.Fatalf("SetAccountTypePolicy: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "account-1", Enabled: true, AccessToken: "token"}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.ResetTransientState()
	p.Reload()
	h := &Handler{pool: p}

	update := httptest.NewRecorder()
	h.apiUpdateAccount(update, httptest.NewRequest(http.MethodPut, "/admin/api/accounts/account-1", strings.NewReader(
		`{"modelPolicyOverride":true,"allowedModels":["Claude-Opus-4.8"],"maxSSE":9}`,
	)), "account-1")
	if update.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", update.Code, update.Body.String())
	}

	list := httptest.NewRecorder()
	h.apiGetAccounts(list, httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil))
	var accounts []map[string]interface{}
	if err := json.Unmarshal(list.Body.Bytes(), &accounts); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts=%d", len(accounts))
	}
	a := accounts[0]
	if a["accountType"] != config.AccountTypeFree || a["modelPolicySource"] != "account" || a["maxSSESource"] != "account" || a["maxRPMSource"] != "subscription_type" {
		t.Fatalf("policy sources = %#v", a)
	}
	if a["effectiveMaxSSE"] != float64(9) || a["effectiveMaxRPM"] != float64(14) {
		t.Fatalf("effective limits = SSE:%v RPM:%v", a["effectiveMaxSSE"], a["effectiveMaxRPM"])
	}
	models, ok := a["effectiveAllowedModels"].([]interface{})
	if !ok || len(models) != 1 || models[0] != "claude-opus-4.8" {
		t.Fatalf("effective models = %#v", a["effectiveAllowedModels"])
	}
}

func TestAccountPolicyAdminValidation(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "account-1"}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	invalidBodies := []string{
		`{"modelPolicyOverride":true,"allowedModels":"not-an-array"}`,
		`{"modelPolicyOverride":"yes"}`,
		`{"maxSSE":1.5}`,
		`{"maxRPM":-1}`,
	}
	for _, body := range invalidBodies {
		recorder := httptest.NewRecorder()
		h.apiUpdateAccount(recorder, httptest.NewRequest(http.MethodPut, "/admin/api/accounts/account-1", strings.NewReader(body)), "account-1")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, recorder.Code, recorder.Body.String())
		}
	}
	account := config.GetAccounts()[0]
	if account.ModelPolicyOverride || len(account.AllowedModels) != 0 {
		t.Fatalf("invalid update mutated account: %+v", account)
	}
}
