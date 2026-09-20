package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"math"
	"net/http"
	"sort"
	"strings"
)

func parseIntegerPatch(name string, value interface{}) (int, error) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return int(number), nil
}

func parseAllowedModelsPatch(value interface{}) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]interface{})
	if !ok {
		return nil, fmt.Errorf("allowedModels must be an array of strings")
	}
	models := make([]string, 0, len(items))
	for i, item := range items {
		model, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("allowedModels[%d] must be a string", i)
		}
		models = append(models, model)
	}
	return models, nil
}

type accountTypePolicyResponse struct {
	Type          string   `json:"type"`
	Group         string   `json:"group"`
	AccountCount  int      `json:"accountCount"`
	AllowedModels []string `json:"allowedModels"`
	MaxSSE        int      `json:"maxSSE"`
	MaxRPM        int      `json:"maxRPM"`
}

func (h *Handler) apiGetAccountTypePolicies(w http.ResponseWriter, _ *http.Request) {
	accounts := config.GetAccounts()
	counts := make(map[string]int, len(config.SupportedAccountPolicyCategories))
	knownModels := make(map[string]struct{})
	for _, model := range h.pool.GetKnownModels() {
		knownModels[model] = struct{}{}
	}
	for _, account := range accounts {
		counts[config.AccountTypeFor(account)]++
		if credentialType, ok := config.CredentialTypeFor(account); ok {
			counts[credentialType]++
		}
		for _, model := range account.AllowedModels {
			knownModels[model] = struct{}{}
		}
	}

	h.modelsCacheMu.RLock()
	for _, model := range h.cachedModels {
		normalized := strings.ToLower(strings.TrimSpace(model.ModelId))
		if normalized != "" {
			knownModels[normalized] = struct{}{}
		}
	}
	h.modelsCacheMu.RUnlock()

	policies := config.GetAccountTypePolicies()
	result := make([]accountTypePolicyResponse, 0, len(config.SupportedAccountPolicyCategories))
	for _, category := range config.SupportedAccountPolicyCategories {
		policy := policies[category.Type]
		for _, model := range policy.AllowedModels {
			knownModels[model] = struct{}{}
		}
		result = append(result, accountTypePolicyResponse{
			Type:          category.Type,
			Group:         category.Group,
			AccountCount:  counts[category.Type],
			AllowedModels: append([]string{}, policy.AllowedModels...),
			MaxSSE:        policy.MaxSSE,
			MaxRPM:        policy.MaxRPM,
		})
	}

	availableModels := make([]string, 0, len(knownModels))
	for model := range knownModels {
		availableModels = append(availableModels, model)
	}
	sort.Strings(availableModels)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"types":           result,
		"availableModels": availableModels,
	})
}

func (h *Handler) apiUpdateAccountTypePolicy(w http.ResponseWriter, r *http.Request, accountType string) {
	var request struct {
		AllowedModels []string `json:"allowedModels"`
		MaxSSE        int      `json:"maxSSE"`
		MaxRPM        int      `json:"maxRPM"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid policy: " + err.Error()})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid policy: request body must contain one JSON object"})
		return
	}
	if err := config.SetAccountTypePolicy(accountType, config.AccountTypePolicy{
		AllowedModels: request.AllowedModels,
		MaxSSE:        request.MaxSSE,
		MaxRPM:        request.MaxRPM,
	}); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
