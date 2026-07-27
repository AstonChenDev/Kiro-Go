package auth

import (
	"testing"
	"time"
)

const unsafeAWSRegion = "us-east-1.amazonaws.com@attacker.test/x"

func TestAuthEntryPointsRejectUnsafeRegionBeforeNetwork(t *testing.T) {
	if _, err := StartBuilderIdLogin(unsafeAWSRegion); err == nil {
		t.Fatal("StartBuilderIdLogin accepted an unsafe region")
	}
	if _, _, _, err := StartIamSsoLogin("https://view.awsapps.com/start", unsafeAWSRegion); err == nil {
		t.Fatal("StartIamSsoLogin accepted an unsafe region")
	}
	if _, _, err := StartKiroSsoLogin(unsafeAWSRegion); err == nil {
		t.Fatal("StartKiroSsoLogin accepted an unsafe region")
	}
	if _, _, _, _, _, err := ImportFromSsoToken("token", unsafeAWSRegion); err == nil {
		t.Fatal("ImportFromSsoToken accepted an unsafe region")
	}
}

func TestRefreshOIDCTokenRejectsUnsafeRegionBeforeURLBuild(t *testing.T) {
	previous := oidcTokenURL
	called := false
	oidcTokenURL = func(string) string {
		called = true
		return "https://attacker.test/token"
	}
	t.Cleanup(func() { oidcTokenURL = previous })

	if _, _, _, _, err := refreshOIDCToken("refresh", "client", "secret", unsafeAWSRegion, nil); err == nil {
		t.Fatal("refreshOIDCToken accepted an unsafe region")
	}
	if called {
		t.Fatal("OIDC URL builder was called before region validation")
	}
}

func TestStoredAuthSessionsRevalidateRegion(t *testing.T) {
	builderSession := &BuilderIdSession{
		ID:        "unsafe-builder-session",
		ExpiresAt: time.Now().Add(time.Minute),
		Region:    unsafeAWSRegion,
	}
	builderIdMu.Lock()
	builderIdSessions[builderSession.ID] = builderSession
	builderIdMu.Unlock()
	t.Cleanup(func() {
		builderIdMu.Lock()
		delete(builderIdSessions, builderSession.ID)
		builderIdMu.Unlock()
	})
	if _, _, _, _, _, _, _, err := PollBuilderIdAuth(builderSession.ID); err == nil {
		t.Fatal("PollBuilderIdAuth accepted an unsafe stored region")
	}

	iamSessionID := "unsafe-iam-session"
	iamSession := &IamSsoSession{
		State:     "expected-state",
		ExpiresAt: time.Now().Add(time.Minute),
		Region:    unsafeAWSRegion,
	}
	sessionsMu.Lock()
	sessions[iamSessionID] = iamSession
	sessionsMu.Unlock()
	t.Cleanup(func() {
		sessionsMu.Lock()
		delete(sessions, iamSessionID)
		sessionsMu.Unlock()
	})
	callback := "http://127.0.0.1/oauth/callback?code=test&state=expected-state"
	if _, _, _, _, _, _, err := CompleteIamSsoLogin(iamSessionID, callback); err == nil {
		t.Fatal("CompleteIamSsoLogin accepted an unsafe stored region")
	}
}
