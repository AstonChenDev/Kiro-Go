package proxy

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"os"
	"strings"
)

const (
	integrationTokenEnv      = "KIRO_INTEGRATION_TOKEN"
	integrationMaxBodyBytes  = 16 << 20
	integrationMaxBatchSize  = 100
	integrationMaxSourceID   = 160
	integrationMaxMetadataID = 200
)

type credentialIntegrationRequest struct {
	Source      string            `json:"source"`
	CustomerRef string            `json:"customerRef,omitempty"`
	JobID       string            `json:"jobId,omitempty"`
	Credentials []json.RawMessage `json:"credentials"`
}

type credentialIntegrationResult struct {
	Index               int                    `json:"index"`
	SourceID            string                 `json:"sourceId,omitempty"`
	Status              string                 `json:"status"`
	Account             map[string]interface{} `json:"account,omitempty"`
	Error               string                 `json:"error,omitempty"`
	Retryable           bool                   `json:"retryable,omitempty"`
	RotatedRefreshToken string                 `json:"rotatedRefreshToken,omitempty"`
}

type credentialIntegrationSummary struct {
	Total      int `json:"total"`
	Imported   int `json:"imported"`
	Duplicates int `json:"duplicates"`
	Failed     int `json:"failed"`
}

// integrationCaptureWriter adapts the existing, heavily validated single-item
// credential importer to batch ingestion without duplicating its auth-method,
// refresh, profile validation, and persistence rules.
type integrationCaptureWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func newIntegrationCaptureWriter() *integrationCaptureWriter {
	return &integrationCaptureWriter{header: make(http.Header), status: http.StatusOK}
}

func (w *integrationCaptureWriter) Header() http.Header { return w.header }

func (w *integrationCaptureWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *integrationCaptureWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(p)
}

func configuredIntegrationToken() string {
	return strings.TrimSpace(os.Getenv(integrationTokenEnv))
}

func integrationAuthorized(r *http.Request, expected string) bool {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authHeader) < len("Bearer ") || !strings.EqualFold(authHeader[:len("Bearer ")], "Bearer ") {
		return false
	}
	provided := strings.TrimSpace(authHeader[len("Bearer "):])
	if provided == "" || expected == "" {
		return false
	}
	// Compare fixed-size digests so token length is not observable through the
	// constant-time primitive.
	expectedDigest := sha256.Sum256([]byte(expected))
	providedDigest := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedDigest[:], providedDigest[:]) == 1
}

func writeIntegrationJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *Handler) handleCredentialIntegration(w http.ResponseWriter, r *http.Request) {
	expected := configuredIntegrationToken()
	if len(expected) < 32 {
		writeIntegrationJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": integrationTokenEnv + " must be configured with at least 32 characters",
		})
		return
	}
	if !integrationAuthorized(r, expected) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeIntegrationJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}

	if r.URL.Path == "/internal/v1/credentials/status" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeIntegrationJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
			return
		}
		writeIntegrationJSON(w, http.StatusOK, map[string]interface{}{
			"status":       "ok",
			"version":      config.Version,
			"maxBatchSize": integrationMaxBatchSize,
		})
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeIntegrationJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, integrationMaxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var batch credentialIntegrationRequest
	if err := decoder.Decode(&batch); err != nil {
		writeIntegrationJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON: " + err.Error()})
		return
	}
	if err := ensureIntegrationJSONEOF(decoder); err != nil {
		writeIntegrationJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	batch.Source = strings.TrimSpace(batch.Source)
	batch.CustomerRef = strings.TrimSpace(batch.CustomerRef)
	batch.JobID = strings.TrimSpace(batch.JobID)
	if batch.Source == "" || len(batch.Source) > integrationMaxMetadataID ||
		len(batch.CustomerRef) > integrationMaxMetadataID || len(batch.JobID) > integrationMaxMetadataID {
		writeIntegrationJSON(w, http.StatusBadRequest, map[string]string{"error": "source is required and integration metadata must not exceed 200 characters"})
		return
	}
	if len(batch.Credentials) == 0 || len(batch.Credentials) > integrationMaxBatchSize {
		writeIntegrationJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("credentials must contain between 1 and %d items", integrationMaxBatchSize),
		})
		return
	}

	results := make([]credentialIntegrationResult, 0, len(batch.Credentials))
	summary := credentialIntegrationSummary{Total: len(batch.Credentials)}
	for index, raw := range batch.Credentials {
		result := h.importIntegrationCredential(r, index, raw)
		results = append(results, result)
		switch result.Status {
		case "imported":
			summary.Imported++
		case "duplicate":
			summary.Duplicates++
		default:
			summary.Failed++
		}
	}

	writeIntegrationJSON(w, http.StatusOK, map[string]interface{}{
		"success": summary.Failed == 0,
		"source":  batch.Source,
		"jobId":   batch.JobID,
		"summary": summary,
		"results": results,
	})
	logger.Infof(
		"[CredentialIntegration] source=%q customer_ref=%q job=%q total=%d imported=%d duplicates=%d failed=%d",
		batch.Source, batch.CustomerRef, batch.JobID, summary.Total, summary.Imported, summary.Duplicates, summary.Failed,
	)
}

func ensureIntegrationJSONEOF(decoder *json.Decoder) error {
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain exactly one JSON object")
		}
		return fmt.Errorf("invalid trailing JSON: %v", err)
	}
	return nil
}

func (h *Handler) importIntegrationCredential(parent *http.Request, index int, raw json.RawMessage) credentialIntegrationResult {
	result := credentialIntegrationResult{Index: index, Status: "failed"}
	if len(raw) == 0 || len(raw) > 1<<20 {
		result.Error = "credential item is empty or exceeds 1 MiB"
		return result
	}

	var shape map[string]interface{}
	if err := json.Unmarshal(raw, &shape); err != nil || shape == nil {
		result.Error = "credential item must be a JSON object"
		return result
	}
	if sourceID, ok := shape["sourceId"].(string); ok {
		result.SourceID = strings.TrimSpace(sourceID)
	}
	if len(result.SourceID) > integrationMaxSourceID {
		result.Error = "sourceId must not exceed 160 characters"
		return result
	}
	// Login passwords and MFA seeds are never needed by Kiro-Go. Reject them
	// instead of silently accepting accidental secret over-sharing.
	for _, field := range []string{"password", "mfaSecret", "mfa_secret"} {
		if value, exists := shape[field]; exists && value != nil && strings.TrimSpace(fmt.Sprint(value)) != "" {
			result.Error = field + " must not be sent to Kiro-Go"
			return result
		}
	}
	delete(shape, "sourceId")
	sanitized, err := json.Marshal(shape)
	if err != nil {
		result.Error = "credential item cannot be encoded"
		return result
	}

	child, err := http.NewRequestWithContext(parent.Context(), http.MethodPost, "/admin/api/auth/credentials", bytes.NewReader(sanitized))
	if err != nil {
		result.Error = "credential import request could not be created"
		return result
	}
	capture := newIntegrationCaptureWriter()
	h.apiImportCredentials(capture, child)

	var response map[string]interface{}
	_ = json.Unmarshal(capture.body.Bytes(), &response)
	if capture.status >= 200 && capture.status < 300 {
		result.Status = "imported"
		if account, ok := response["account"].(map[string]interface{}); ok {
			result.Account = account
		}
		return result
	}

	if message, ok := response["error"].(string); ok {
		result.Error = message
	} else {
		result.Error = strings.TrimSpace(capture.body.String())
	}
	if result.Error == "" {
		result.Error = http.StatusText(capture.status)
	}
	if len(result.Error) > 1000 {
		result.Error = result.Error[:1000]
	}
	if rotated, ok := response["rotatedRefreshToken"].(string); ok {
		result.RotatedRefreshToken = strings.TrimSpace(rotated)
	}
	if capture.status == http.StatusConflict {
		result.Status = "duplicate"
	} else {
		lowerError := strings.ToLower(result.Error)
		result.Retryable = capture.status == http.StatusTooManyRequests || capture.status >= 500 ||
			strings.Contains(lowerError, "timeout") || strings.Contains(lowerError, "timed out") ||
			strings.Contains(lowerError, "connection") || strings.Contains(lowerError, "connection reset") ||
			strings.Contains(lowerError, "unexpected eof") || strings.Contains(lowerError, "temporarily unavailable") ||
			strings.Contains(lowerError, "http 429") || strings.Contains(lowerError, "http 502") ||
			strings.Contains(lowerError, "http 503") || strings.Contains(lowerError, "http 504")
	}
	return result
}
