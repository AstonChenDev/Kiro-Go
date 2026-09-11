package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newCredentialIntegrationRequest(method, path, token, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestCredentialIntegrationStatusAuthentication(t *testing.T) {
	h := &Handler{}
	const secret = "integration-secret-at-least-32-chars"

	t.Setenv(integrationTokenEnv, "")
	missingConfig := httptest.NewRecorder()
	h.ServeHTTP(missingConfig, newCredentialIntegrationRequest(http.MethodGet, "/internal/v1/credentials/status", "token", ""))
	if missingConfig.Code != http.StatusServiceUnavailable {
		t.Fatalf("unset token status=%d body=%s", missingConfig.Code, missingConfig.Body.String())
	}

	t.Setenv(integrationTokenEnv, secret)
	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, newCredentialIntegrationRequest(http.MethodGet, "/internal/v1/credentials/status", "wrong", ""))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	authorized := httptest.NewRecorder()
	h.ServeHTTP(authorized, newCredentialIntegrationRequest(http.MethodGet, "/internal/v1/credentials/status", secret, ""))
	if authorized.Code != http.StatusOK {
		t.Fatalf("valid token status=%d body=%s", authorized.Code, authorized.Body.String())
	}
	if authorized.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", authorized.Header().Get("Cache-Control"))
	}
}

func TestCredentialIntegrationBatchImportsAndDeduplicatesAPIKeys(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const secret = "integration-secret-at-least-32-chars"
	t.Setenv(integrationTokenEnv, secret)
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	h := &Handler{pool: accountpool.GetPool()}
	body := `{
		"source":"kiro-login-web",
		"customerRef":"customer-hash",
		"jobId":"job-1",
		"credentials":[
			{"sourceId":"job-1:1","authMethod":"api_key","kiroApiKey":"ksk_integration_test|eu-central-1","email":"one@example.com"},
			{"sourceId":"job-1:2","authMethod":"api_key","kiroApiKey":"ksk_integration_test|eu-central-1","email":"one@example.com"}
		]
	}`
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, newCredentialIntegrationRequest(http.MethodPost, "/internal/v1/credentials/import", secret, body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool                          `json:"success"`
		Summary credentialIntegrationSummary  `json:"summary"`
		Results []credentialIntegrationResult `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Success || response.Summary.Imported != 1 || response.Summary.Duplicates != 1 || response.Summary.Failed != 0 {
		t.Fatalf("unexpected response: %+v body=%s", response, recorder.Body.String())
	}
	if len(response.Results) != 2 || response.Results[0].SourceID != "job-1:1" || response.Results[1].Status != "duplicate" {
		t.Fatalf("unexpected results: %+v", response.Results)
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 || accounts[0].AuthMethod != "api_key" || accounts[0].Region != "eu-central-1" {
		t.Fatalf("unexpected accounts: %+v", accounts)
	}
}

func TestCredentialIntegrationRejectsLoginSecretsPerItem(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const secret = "integration-secret-at-least-32-chars"
	t.Setenv(integrationTokenEnv, secret)
	h := &Handler{pool: accountpool.GetPool()}
	body := `{"source":"kiro-login-web","credentials":[{"sourceId":"one","authMethod":"api_key","kiroApiKey":"ksk_secret_test","password":"must-not-travel"}]}`
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, newCredentialIntegrationRequest(http.MethodPost, "/internal/v1/credentials/import", secret, body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "password must not be sent") || !strings.Contains(recorder.Body.String(), `"failed":1`) {
		t.Fatalf("body=%s", recorder.Body.String())
	}
	if got := len(config.GetAccounts()); got != 0 {
		t.Fatalf("persisted %d accounts", got)
	}
}

func TestCredentialIntegrationRejectsInvalidEnvelope(t *testing.T) {
	const secret = "integration-secret-at-least-32-chars"
	t.Setenv(integrationTokenEnv, secret)
	h := &Handler{}
	for name, body := range map[string]string{
		"empty batch":    `{"source":"kiro-login-web","credentials":[]}`,
		"missing source": `{"credentials":[{}]}`,
		"unknown field":  `{"source":"kiro-login-web","credentials":[{}],"surprise":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, newCredentialIntegrationRequest(http.MethodPost, "/internal/v1/credentials/import", secret, body))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestCredentialIntegrationMethodRestrictions(t *testing.T) {
	const secret = "integration-secret-at-least-32-chars"
	t.Setenv(integrationTokenEnv, secret)
	h := &Handler{}
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/internal/v1/credentials/status"},
		{http.MethodGet, "/internal/v1/credentials/import"},
	} {
		t.Run(fmt.Sprintf("%s %s", tc.method, tc.path), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, newCredentialIntegrationRequest(tc.method, tc.path, secret, ""))
			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
