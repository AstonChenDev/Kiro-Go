package proxy

import (
	"fmt"
	"kiro-go/config"
	"net/http"
	"strings"
)

const (
	kiroStreamingSDKVersion = "1.0.34"
	kiroRuntimeSDKVersion   = "1.0.0"
)

type kiroHeaderValues struct {
	UserAgent    string
	AmzUserAgent string
	Host         string
}

func buildStreamingHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererstreaming", kiroStreamingSDKVersion, "m/E")
}

func buildRuntimeHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererruntime", kiroRuntimeSDKVersion, "m/N,E")
}

func buildKiroHeaderValues(account *config.Account, host, apiName, sdkVersion, mode string) kiroHeaderValues {
	accountID := ""
	if account != nil {
		accountID = account.ID
	}
	// Derive the desktop fingerprint (platform/version) from the same accountID
	// used for the device id below, so an account's platform and device stay
	// mutually consistent — one stable mac/win install, never a Linux server.
	clientCfg := config.GetKiroClientConfig(accountID)
	machineID := ""
	if account != nil {
		machineID = account.MachineId
		// Never let machineId be empty: an empty value drops the UA suffix and
		// makes this account's User-Agent identical to every other empty-id
		// account, which is the strongest cross-account association signal
		// upstream can correlate on. Fall back to a stable, account-derived
		// 64-hex id (sha256) so the UA always carries a unique, fixed device
		// fingerprint — same value per account across requests and restarts.
		// Ported from kiro-tutu (zero-dep: config.DeriveMachineId is stdlib).
		if machineID == "" && account.ID != "" {
			machineID = config.DeriveMachineId(account.ID)
		}
	}

	userAgent := fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s lang/js md/nodejs#%s api/%s#%s %s KiroIDE-%s",
		sdkVersion,
		clientCfg.SystemVersion,
		clientCfg.NodeVersion,
		apiName,
		sdkVersion,
		mode,
		clientCfg.KiroVersion,
	)
	amzUserAgent := fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s", sdkVersion, clientCfg.KiroVersion)
	if machineID != "" {
		userAgent += "-" + machineID
		amzUserAgent += "-" + machineID
	}

	return kiroHeaderValues{
		UserAgent:    userAgent,
		AmzUserAgent: amzUserAgent,
		Host:         host,
	}
}

func applyKiroBaseHeaders(req *http.Request, account *config.Account, values kiroHeaderValues) {
	token := accountBearerToken(account)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// Kiro requires external identity-provider access tokens and API keys to be
	// identified explicitly. Keep this in the shared header path so streaming
	// and REST requests cannot drift apart.
	req.Header.Del("TokenType")
	req.Header.Del("tokentype")
	if account != nil {
		if config.IsAPIKeyAccount(account) {
			// Upstream accepts either casing; CLI captures use lowercase "tokentype".
			req.Header.Set("tokentype", "API_KEY")
		} else if strings.EqualFold(strings.TrimSpace(account.AuthMethod), "external_idp") {
			req.Header.Set("TokenType", "EXTERNAL_IDP")
		}
	}
	req.Header.Set("User-Agent", values.UserAgent)
	req.Header.Set("x-amz-user-agent", values.AmzUserAgent)
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	if values.Host != "" {
		req.Host = values.Host
	}
}

// accountBearerToken returns the token used for Authorization: Bearer.
// API Key accounts prefer KiroApiKey; OAuth accounts use AccessToken.
func accountBearerToken(account *config.Account) string {
	if account == nil {
		return ""
	}
	if config.IsAPIKeyAccount(account) {
		if key := strings.TrimSpace(account.KiroApiKey); key != "" {
			return key
		}
	}
	return strings.TrimSpace(account.AccessToken)
}
