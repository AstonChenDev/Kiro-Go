package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	AccountTypeFree    = "FREE"
	AccountTypePro     = "PRO"
	AccountTypeProPlus = "PRO_PLUS"
	AccountTypePower   = "POWER"

	CredentialTypeBuilderID  = "BUILDER_ID"
	CredentialTypeEnterprise = "ENTERPRISE"
	CredentialTypeGoogle     = "GOOGLE"
	CredentialTypeGitHub     = "GITHUB"
	CredentialTypeAPIKey     = "API_KEY"

	PolicyGroupSubscription = "subscription"
	PolicyGroupCredential   = "credential"

	maxAccountPolicyLimit = 1_000_000
	maxAllowedModels      = 512
	maxModelIDLength      = 256
)

// AccountPolicyCategory describes one selectable shared-policy scope.
type AccountPolicyCategory struct {
	Type  string
	Group string
}

// SupportedAccountPolicyCategories is the stable display and API order. An
// account belongs to one subscription category and, when identifiable, one
// credential/provider category.
var SupportedAccountPolicyCategories = []AccountPolicyCategory{
	{Type: AccountTypeFree, Group: PolicyGroupSubscription},
	{Type: AccountTypePro, Group: PolicyGroupSubscription},
	{Type: AccountTypeProPlus, Group: PolicyGroupSubscription},
	{Type: AccountTypePower, Group: PolicyGroupSubscription},
	{Type: CredentialTypeBuilderID, Group: PolicyGroupCredential},
	{Type: CredentialTypeEnterprise, Group: PolicyGroupCredential},
	{Type: CredentialTypeGoogle, Group: PolicyGroupCredential},
	{Type: CredentialTypeGitHub, Group: PolicyGroupCredential},
	{Type: CredentialTypeAPIKey, Group: PolicyGroupCredential},
}

// AccountTypePolicy defines defaults inherited from a subscription tier or a
// credential/provider family. Zero concurrency values inherit the next policy
// layer. An empty model list is unrestricted at this layer; actual upstream
// model availability is always enforced independently by the account pool.
type AccountTypePolicy struct {
	AllowedModels []string `json:"allowedModels,omitempty"`
	MaxSSE        int      `json:"maxSSE,omitempty"`
	MaxRPM        int      `json:"maxRPM,omitempty"`
}

// AccountLimits is the resolved concurrency/rate-limit view used by both the
// scheduler and the admin API. Sources are "account", "credential_type",
// "subscription_type", or "system".
type AccountLimits struct {
	MaxSSE    int
	MaxRPM    int
	SSESource string
	RPMSource string
}

// NormalizeAccountType maps upstream subscription labels to the four stable
// policy categories already used by the product UI. Unknown or empty plans are
// treated as FREE, matching the existing subscription parsing behaviour.
func NormalizeAccountType(raw string) string {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	switch {
	case strings.Contains(upper, "PRO_PLUS"), strings.Contains(upper, "PROPLUS"), strings.Contains(upper, "PRO+"):
		return AccountTypeProPlus
	case strings.Contains(upper, "POWER"):
		return AccountTypePower
	case strings.Contains(upper, "PRO"):
		return AccountTypePro
	default:
		return AccountTypeFree
	}
}

func IsSupportedAccountPolicyType(raw string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(raw))
	for _, category := range SupportedAccountPolicyCategories {
		if normalized == category.Type {
			return true
		}
	}
	return false
}

func AccountTypeFor(account Account) string {
	return NormalizeAccountType(account.SubscriptionType)
}

// CredentialTypeFor classifies the credential/provider families shown on
// account cards. The bool is false when legacy social credentials do not retain
// enough provider metadata to distinguish Google from GitHub.
func CredentialTypeFor(account Account) (string, bool) {
	if IsAPIKeyAccount(&account) {
		return CredentialTypeAPIKey, true
	}
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	method := strings.ToLower(strings.TrimSpace(account.AuthMethod))
	switch {
	case strings.Contains(provider, "builder"), method == "builderid", method == "builder_id":
		return CredentialTypeBuilderID, true
	case strings.Contains(provider, "google"):
		return CredentialTypeGoogle, true
	case strings.Contains(provider, "github"):
		return CredentialTypeGitHub, true
	case strings.Contains(provider, "enterprise"), strings.Contains(provider, "azure"), strings.Contains(provider, "microsoft"):
		return CredentialTypeEnterprise, true
	case method == "external_idp", method == "idc":
		return CredentialTypeEnterprise, true
	default:
		return "", false
	}
}

// NormalizeAllowedModels canonicalizes model IDs for case-insensitive routing.
// Duplicates and blank entries are removed, and output is sorted for stable JSON.
func NormalizeAllowedModels(models []string) ([]string, error) {
	if len(models) > maxAllowedModels {
		return nil, fmt.Errorf("allowedModels supports at most %d entries", maxAllowedModels)
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			continue
		}
		if len(model) > maxModelIDLength {
			return nil, fmt.Errorf("model ID %q exceeds %d characters", model[:32], maxModelIDLength)
		}
		if strings.ContainsAny(model, "\x00\r\n\t") {
			return nil, fmt.Errorf("model ID %q contains control characters", model)
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	sort.Strings(out)
	return out, nil
}

func validatePolicyLimit(name string, value int) error {
	if value < 0 || value > maxAccountPolicyLimit {
		return fmt.Errorf("%s must be between 0 and %d", name, maxAccountPolicyLimit)
	}
	return nil
}

func normalizeAccountRoutingPolicy(account *Account) error {
	if account == nil {
		return errors.New("account is nil")
	}
	if err := validatePolicyLimit("maxSSE", account.MaxSSE); err != nil {
		return err
	}
	if err := validatePolicyLimit("maxRPM", account.MaxRPM); err != nil {
		return err
	}
	models, err := NormalizeAllowedModels(account.AllowedModels)
	if err != nil {
		return err
	}
	account.AllowedModels = models
	if !account.ModelPolicyOverride {
		// Avoid persisting stale values that are not active and could become an
		// unexpected override if a client later toggles only the boolean.
		account.AllowedModels = nil
	}
	return nil
}

// ValidateAccountRoutingPolicy validates and canonicalizes an administrative
// account update before it is persisted.
func ValidateAccountRoutingPolicy(account *Account) error {
	return normalizeAccountRoutingPolicy(account)
}

func normalizeAccountTypePolicy(policy AccountTypePolicy) (AccountTypePolicy, error) {
	if err := validatePolicyLimit("maxSSE", policy.MaxSSE); err != nil {
		return AccountTypePolicy{}, err
	}
	if err := validatePolicyLimit("maxRPM", policy.MaxRPM); err != nil {
		return AccountTypePolicy{}, err
	}
	models, err := NormalizeAllowedModels(policy.AllowedModels)
	if err != nil {
		return AccountTypePolicy{}, err
	}
	policy.AllowedModels = models
	return policy, nil
}

// normalizeAccountPolicies validates and canonicalizes persisted policy data.
// It is called before a loaded config becomes globally visible.
func normalizeAccountPolicies(c *Config) (bool, error) {
	if c == nil {
		return false, errors.New("config is nil")
	}
	changed := false
	for i := range c.Accounts {
		beforeModels := strings.Join(c.Accounts[i].AllowedModels, "\x00")
		beforeOverride := c.Accounts[i].ModelPolicyOverride
		if err := normalizeAccountRoutingPolicy(&c.Accounts[i]); err != nil {
			return false, fmt.Errorf("account %q routing policy: %w", c.Accounts[i].ID, err)
		}
		if beforeModels != strings.Join(c.Accounts[i].AllowedModels, "\x00") || beforeOverride != c.Accounts[i].ModelPolicyOverride {
			changed = true
		}
	}

	normalized := make(map[string]AccountTypePolicy, len(c.AccountTypePolicies))
	seenTypes := make(map[string]struct{}, len(c.AccountTypePolicies))
	for rawType, rawPolicy := range c.AccountTypePolicies {
		accountType := strings.ToUpper(strings.TrimSpace(rawType))
		if !IsSupportedAccountPolicyType(accountType) {
			return false, fmt.Errorf("unsupported account type policy %q", rawType)
		}
		policy, err := normalizeAccountTypePolicy(rawPolicy)
		if err != nil {
			return false, fmt.Errorf("account type %s policy: %w", accountType, err)
		}
		if _, duplicate := seenTypes[accountType]; duplicate {
			return false, fmt.Errorf("duplicate account type policy %q", accountType)
		}
		seenTypes[accountType] = struct{}{}
		if len(policy.AllowedModels) == 0 && policy.MaxSSE == 0 && policy.MaxRPM == 0 {
			changed = true
			continue
		}
		normalized[accountType] = policy
		if accountType != rawType || strings.Join(policy.AllowedModels, "\x00") != strings.Join(rawPolicy.AllowedModels, "\x00") {
			changed = true
		}
	}
	if len(normalized) == 0 {
		changed = changed || c.AccountTypePolicies != nil
		c.AccountTypePolicies = nil
	} else {
		c.AccountTypePolicies = normalized
	}
	return changed, nil
}

func cloneAccountTypePolicy(policy AccountTypePolicy) AccountTypePolicy {
	policy.AllowedModels = append([]string(nil), policy.AllowedModels...)
	return policy
}

func GetAccountTypePolicies() map[string]AccountTypePolicy {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	out := make(map[string]AccountTypePolicy, len(SupportedAccountPolicyCategories))
	if cfg == nil {
		return out
	}
	for _, category := range SupportedAccountPolicyCategories {
		out[category.Type] = cloneAccountTypePolicy(cfg.AccountTypePolicies[category.Type])
	}
	return out
}

func SetAccountTypePolicy(accountType string, policy AccountTypePolicy) error {
	accountType = strings.ToUpper(strings.TrimSpace(accountType))
	if !IsSupportedAccountPolicyType(accountType) {
		return fmt.Errorf("unsupported account type %q", accountType)
	}
	normalized, err := normalizeAccountTypePolicy(policy)
	if err != nil {
		return err
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if cfg.AccountTypePolicies == nil {
		cfg.AccountTypePolicies = make(map[string]AccountTypePolicy)
	}
	previous, existed := cfg.AccountTypePolicies[accountType]
	if len(normalized.AllowedModels) == 0 && normalized.MaxSSE == 0 && normalized.MaxRPM == 0 {
		delete(cfg.AccountTypePolicies, accountType)
	} else {
		cfg.AccountTypePolicies[accountType] = normalized
	}
	if err := saveLocked(); err != nil {
		if existed {
			cfg.AccountTypePolicies[accountType] = previous
		} else {
			delete(cfg.AccountTypePolicies, accountType)
		}
		return err
	}
	return nil
}

func subscriptionPolicyLocked(account Account) AccountTypePolicy {
	if cfg == nil || cfg.AccountTypePolicies == nil {
		return AccountTypePolicy{}
	}
	return cfg.AccountTypePolicies[AccountTypeFor(account)]
}

func credentialPolicyLocked(account Account) (AccountTypePolicy, bool) {
	if cfg == nil || cfg.AccountTypePolicies == nil {
		return AccountTypePolicy{}, false
	}
	credentialType, ok := CredentialTypeFor(account)
	if !ok {
		return AccountTypePolicy{}, false
	}
	return cfg.AccountTypePolicies[credentialType], true
}

// EffectiveAllowedModels returns the active manual allow-list and its source.
// restricted=false means no manual restriction; the runtime upstream capability
// cache still applies in all cases.
func EffectiveAllowedModels(account Account) (models []string, source string, restricted bool) {
	if account.ModelPolicyOverride {
		return append([]string(nil), account.AllowedModels...), "account", true
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if policy, ok := credentialPolicyLocked(account); ok && len(policy.AllowedModels) > 0 {
		return append([]string(nil), policy.AllowedModels...), "credential_type", true
	}
	policy := subscriptionPolicyLocked(account)
	if len(policy.AllowedModels) > 0 {
		return append([]string(nil), policy.AllowedModels...), "subscription_type", true
	}
	return nil, "system", false
}

func isEnterpriseAccount(account Account) bool {
	return strings.EqualFold(account.Provider, "Enterprise") ||
		strings.EqualFold(account.Provider, "AzureAD") ||
		strings.EqualFold(account.AuthMethod, "idc") ||
		strings.EqualFold(account.AuthMethod, "external_idp")
}

func systemAccountLimits(account Account) (maxSSE, maxRPM int) {
	if isEnterpriseAccount(account) {
		return 30, 15
	}
	return 3, 10
}

// EffectiveAccountLimits resolves account override > type default > system
// default independently for SSE and RPM.
func EffectiveAccountLimits(account Account) AccountLimits {
	maxSSE, maxRPM := systemAccountLimits(account)
	result := AccountLimits{MaxSSE: maxSSE, MaxRPM: maxRPM, SSESource: "system", RPMSource: "system"}

	cfgLock.RLock()
	subscriptionPolicy := subscriptionPolicyLocked(account)
	credentialPolicy, hasCredentialPolicy := credentialPolicyLocked(account)
	cfgLock.RUnlock()
	if subscriptionPolicy.MaxSSE > 0 {
		result.MaxSSE = subscriptionPolicy.MaxSSE
		result.SSESource = "subscription_type"
	}
	if subscriptionPolicy.MaxRPM > 0 {
		result.MaxRPM = subscriptionPolicy.MaxRPM
		result.RPMSource = "subscription_type"
	}
	if hasCredentialPolicy && credentialPolicy.MaxSSE > 0 {
		result.MaxSSE = credentialPolicy.MaxSSE
		result.SSESource = "credential_type"
	}
	if hasCredentialPolicy && credentialPolicy.MaxRPM > 0 {
		result.MaxRPM = credentialPolicy.MaxRPM
		result.RPMSource = "credential_type"
	}
	if account.MaxSSE > 0 {
		result.MaxSSE = account.MaxSSE
		result.SSESource = "account"
	}
	if account.MaxRPM > 0 {
		result.MaxRPM = account.MaxRPM
		result.RPMSource = "account"
	}
	return result
}
