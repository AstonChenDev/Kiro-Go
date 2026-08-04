package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const supplierMaxResponseBytes int64 = 4 << 20

type supplierAPIError struct {
	StatusCode int
	Message    string
}

func (e *supplierAPIError) Error() string {
	if e == nil {
		return "supplier API error"
	}
	if e.StatusCode == 0 {
		return e.Message
	}
	return fmt.Sprintf("supplier API returned HTTP %d: %s", e.StatusCode, e.Message)
}

func isRetryableSupplierError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *supplierAPIError
	if !errors.As(err, &apiErr) {
		return true // transport failures can be ambiguous after a POST
	}
	return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
}

type supplierStock struct {
	Stock    int     `json:"stock"`
	StockUS  int     `json:"stock_us"`
	StockEU  int     `json:"stock_eu"`
	Price    float64 `json:"price"`
	PriceMin float64 `json:"price_min"`
	PriceMax float64 `json:"price_max"`
	Balance  float64 `json:"balance"`
}

type supplierProfile struct {
	User struct {
		ID          string  `json:"id"`
		Name        string  `json:"name"`
		Email       string  `json:"email"`
		Balance     float64 `json:"balance"`
		MinPurchase int     `json:"min_purchase"`
		MaxPurchase int     `json:"max_purchase"`
	} `json:"user"`
}

type supplierKey struct {
	ID          string  `json:"id,omitempty"`
	Key         string  `json:"key,omitempty"`
	KeyValue    string  `json:"key_value,omitempty"`
	Account     string  `json:"account,omitempty"`
	Password    string  `json:"password,omitempty"`
	IssuerURL   string  `json:"issuer_url,omitempty"`
	Status      string  `json:"status,omitempty"`
	Price       float64 `json:"price,omitempty"`
	PurchasedAt string  `json:"purchased_at,omitempty"`
	CreatedAt   string  `json:"created_at,omitempty"`
}

func (k supplierKey) Value() string {
	if strings.TrimSpace(k.Key) != "" {
		return strings.TrimSpace(k.Key)
	}
	return strings.TrimSpace(k.KeyValue)
}

type supplierKeysPage struct {
	Items    []supplierKey `json:"items"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Pages    int           `json:"pages"`
}

type supplierPurchaseResponse struct {
	Purchased  int           `json:"purchased"`
	Requested  int           `json:"requested"`
	Remaining  int           `json:"remaining"`
	UnitPrice  float64       `json:"unit_price"`
	TotalDebit float64       `json:"total_debit"`
	OrderID    string        `json:"order_id"`
	Keys       []supplierKey `json:"keys"`
	Replayed   bool          `json:"replayed"`
}

type supplierAPI interface {
	GetStock() (supplierStock, error)
	GetProfile() (supplierProfile, error)
	GetKeys(history bool, page, pageSize int) (supplierKeysPage, error)
	Purchase(count int, region, clientOrderID string) (supplierPurchaseResponse, error)
}

type httpSupplierAPI struct {
	provider config.SupplierProvider
	client   *http.Client
}

func newHTTPSupplierAPI(provider config.SupplierProvider) supplierAPI {
	return &httpSupplierAPI{
		provider: provider,
		client:   GetRestClientForProxy(config.GetProxyURL()),
	}
}

func (c *httpSupplierAPI) endpoint(path string) string {
	return strings.TrimRight(c.provider.BaseURL, "/") + path
}

func (c *httpSupplierAPI) do(method, path string, body any, target any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode supplier request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.endpoint(path), reader)
	if err != nil {
		return fmt.Errorf("build supplier request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.provider.APIToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("call supplier API: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, supplierMaxResponseBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read supplier response: %w", err)
	}
	if int64(len(payload)) > supplierMaxResponseBytes {
		return errors.New("supplier response is too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(payload, &apiBody)
		message := strings.TrimSpace(apiBody.Error)
		if message == "" {
			message = strings.TrimSpace(string(payload))
		}
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		if len(message) > 500 {
			message = message[:500]
		}
		return &supplierAPIError{StatusCode: resp.StatusCode, Message: message}
	}
	if target == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("decode supplier response: %w", err)
	}
	return nil
}

func (c *httpSupplierAPI) GetStock() (supplierStock, error) {
	var out supplierStock
	err := c.do(http.MethodGet, "/api/me/stock", nil, &out)
	return out, err
}

func (c *httpSupplierAPI) GetProfile() (supplierProfile, error) {
	var out supplierProfile
	err := c.do(http.MethodGet, "/api/me/profile", nil, &out)
	return out, err
}

func (c *httpSupplierAPI) GetKeys(history bool, page, pageSize int) (supplierKeysPage, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 500 {
		pageSize = 500
	}
	query := url.Values{}
	query.Set("page", strconv.Itoa(page))
	query.Set("page_size", strconv.Itoa(pageSize))
	if history {
		query.Set("history", "1")
	}
	var out supplierKeysPage
	err := c.do(http.MethodGet, "/api/me/keys?"+query.Encode(), nil, &out)
	return out, err
}

func (c *httpSupplierAPI) Purchase(count int, region, clientOrderID string) (supplierPurchaseResponse, error) {
	var out supplierPurchaseResponse
	body := map[string]any{
		"count":           count,
		"region":          region,
		"client_order_id": clientOrderID,
	}
	err := c.do(http.MethodPost, "/api/me/purchase", body, &out)
	if err == nil && out.Requested == 0 {
		out.Requested = count
	}
	return out, err
}

// supplierNow is replaceable by tests that need deterministic state timestamps.
var supplierNow = time.Now
