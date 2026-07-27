package proxy

import (
	"kiro-go/config"
	"testing"
)

// TestRegionalizeURLForRegion asserts that a non-us-east-1 region collapses BOTH
// hardcoded us-east-1 hosts (q.* and codewhisperer.*) onto q.{region} — there is no
// codewhisperer.{region} host — and that us-east-1/empty are no-ops.
func TestRegionalizeURLForRegion(t *testing.T) {
	cases := []struct {
		name   string
		rawURL string
		region string
		want   string
	}{
		{
			name:   "codewhisperer host to q.eu-central-1",
			rawURL: "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles",
			region: "eu-central-1",
			want:   "https://q.eu-central-1.amazonaws.com/ListAvailableProfiles",
		},
		{
			name:   "q host to q.eu-central-1",
			rawURL: "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
			region: "eu-central-1",
			want:   "https://q.eu-central-1.amazonaws.com/generateAssistantResponse",
		},
		{
			name:   "us-east-1 is a no-op (codewhisperer host kept)",
			rawURL: "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles",
			region: "us-east-1",
			want:   "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles",
		},
		{
			name:   "empty region is a no-op",
			rawURL: "https://q.us-east-1.amazonaws.com/x",
			region: "",
			want:   "https://q.us-east-1.amazonaws.com/x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := regionalizeURLForRegion(tc.rawURL, tc.region)
			if got != tc.want {
				t.Fatalf("regionalizeURLForRegion(%q, %q) = %q, want %q", tc.rawURL, tc.region, got, tc.want)
			}
		})
	}
}

// TestRegionalizeURLForRegionNoCodewhispererRegionalHost guards the user-stated
// invariant directly: a regionalized URL must never produce codewhisperer.{region}.
func TestRegionalizeURLForRegionNoCodewhispererRegionalHost(t *testing.T) {
	got := regionalizeURLForRegion("https://codewhisperer.us-east-1.amazonaws.com/GetUserInfo", "eu-central-1")
	if want := "https://q.eu-central-1.amazonaws.com/GetUserInfo"; got != want {
		t.Fatalf("got %q, want %q (must not be codewhisperer.eu-central-1)", got, want)
	}
}

// TestKiroProfileRegionCandidatesExternalIdp checks that Account.Region remains
// the OAuth/OIDC authentication region. Profile discovery uses the cached profile
// ARN when present, then an explicit ApiRegion, followed by the known Kiro
// data-plane defaults.
func TestKiroProfileRegionCandidatesExternalIdp(t *testing.T) {
	// The authentication region is not a profile-region candidate.
	got := kiroProfileRegionCandidates(&config.Account{AuthMethod: "external_idp", Region: "us-east-1"})
	assertOrder(t, got, []string{"us-east-1", "eu-central-1"})

	// A different authentication region does not reorder the data-plane defaults.
	got = kiroProfileRegionCandidates(&config.Account{AuthMethod: "external_idp", Region: "eu-central-1"})
	assertOrder(t, got, []string{"us-east-1", "eu-central-1"})

	// A cached profile ARN is authoritative and leads the fallback list.
	got = kiroProfileRegionCandidates(&config.Account{
		AuthMethod: "external_idp",
		Region:     "eu-central-1",
		ProfileArn: "arn:aws:codewhisperer:ap-southeast-2:123456789012:profile/test",
	})
	assertOrder(t, got, []string{"ap-southeast-2", "us-east-1", "eu-central-1"})

	// An explicit data-plane region follows the cached profile region and is
	// de-duplicated against the built-in fallback list.
	got = kiroProfileRegionCandidates(&config.Account{
		AuthMethod: "external_idp",
		Region:     "eu-west-1",
		ApiRegion:  "ap-south-1",
		ProfileArn: "arn:aws:codewhisperer:ap-southeast-2:123456789012:profile/test",
	})
	assertOrder(t, got, []string{"ap-southeast-2", "ap-south-1", "us-east-1", "eu-central-1"})

	got = kiroProfileRegionCandidates(&config.Account{
		AuthMethod: "external_idp",
		ApiRegion:  "eu-central-1",
	})
	assertOrder(t, got, []string{"eu-central-1", "us-east-1"})
}

// TestKiroProfileRegionCandidatesNoRegion checks an account with no region set falls
// back across the defaults regardless of auth method.
func TestKiroProfileRegionCandidatesNoRegion(t *testing.T) {
	got := kiroProfileRegionCandidates(&config.Account{})
	assertOrder(t, got, []string{"us-east-1", "eu-central-1"})
}

// TestKiroProfileRegionCandidatesIgnoresOAuthAuthRegion checks that OAuth account
// authentication regions never become profile-discovery regions. API-key accounts
// use EffectiveApiRegion for data-plane calls instead.
func TestKiroProfileRegionCandidatesIgnoresOAuthAuthRegion(t *testing.T) {
	for _, method := range []string{"social", "builderId", ""} {
		got := kiroProfileRegionCandidates(&config.Account{AuthMethod: method, Region: "eu-central-1"})
		assertOrder(t, got, []string{"us-east-1", "eu-central-1"})
	}

	got := kiroProfileRegionCandidates(&config.Account{
		AuthMethod: "external_idp",
		Region:     "ap-south-1",
		ApiRegion:  "evil.amazonaws.com",
	})
	assertOrder(t, got, []string{"us-east-1", "eu-central-1"})

	apiKey := &config.Account{
		AuthMethod: "api_key",
		KiroApiKey: "ksk_test",
		Region:     "us-east-1",
		ApiRegion:  "eu-central-1",
	}
	if got := kiroRegionForProfile(apiKey, ""); got != "eu-central-1" {
		t.Fatalf("API-key data-plane region = %q, want EffectiveApiRegion eu-central-1", got)
	}
	assertOrder(t, kiroProfileRegionCandidates(apiKey), []string{"us-east-1", "eu-central-1"})
}

// TestKiroProfileRegionCandidatesIdcFallback checks that idc (IAM Identity Center /
// enterprise SSO) Account.Region is authentication-only, so profile discovery
// probes the same known defaults as other OAuth methods.
func TestKiroProfileRegionCandidatesIdcFallback(t *testing.T) {
	got := kiroProfileRegionCandidates(&config.Account{AuthMethod: "idc", Region: "us-east-1"})
	assertOrder(t, got, []string{"us-east-1", "eu-central-1"})

	// Authentication region does not reorder profile data planes.
	got = kiroProfileRegionCandidates(&config.Account{AuthMethod: "idc", Region: "eu-central-1"})
	assertOrder(t, got, []string{"us-east-1", "eu-central-1"})
}

// TestKiroProfileRegionCandidatesEnvOverride checks KIRO_PROFILE_REGIONS replaces
// the built-in fallback list for OAuth methods without promoting Account.Region.
// An explicit ApiRegion still precedes the environment-provided fallback list.
func TestKiroProfileRegionCandidatesEnvOverride(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "eu-west-1, ap-south-1 ,eu-west-1")
	got := kiroProfileRegionCandidates(&config.Account{
		AuthMethod: "external_idp",
		Region:     "us-east-1",
		ApiRegion:  "ap-northeast-1",
	})
	// Environment values are de-duplicated and trimmed; built-in defaults are replaced.
	assertOrder(t, got, []string{"ap-northeast-1", "eu-west-1", "ap-south-1"})

	// All OAuth methods use the same profile-region extension list.
	got = kiroProfileRegionCandidates(&config.Account{AuthMethod: "idc", Region: "us-east-1"})
	assertOrder(t, got, []string{"eu-west-1", "ap-south-1"})

	got = kiroProfileRegionCandidates(&config.Account{AuthMethod: "social", Region: "us-east-1"})
	assertOrder(t, got, []string{"eu-west-1", "ap-south-1"})
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("candidate regions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate regions = %v, want %v", got, want)
		}
	}
}
