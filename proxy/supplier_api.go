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

var errSupplierOperationUnsupported = errors.New("supplier API does not support this operation")

type supplierAPIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *supplierAPIError) Error() string {
	if e == nil {
		return "supplier API error"
	}
	if e.StatusCode == 0 {
		return e.Message
	}
	if e.Code != "" {
		return fmt.Sprintf("supplier API returned HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
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
	return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500 ||
		(apiErr.StatusCode == http.StatusConflict && apiErr.Code == "ORDER_PROCESSING")
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
	ClientOrderID string        `json:"client_order_id,omitempty"`
	Purchased     int           `json:"purchased"`
	Requested     int           `json:"requested"`
	Remaining     int           `json:"remaining"`
	UnitPrice     float64       `json:"unit_price"`
	TotalDebit    float64       `json:"total_debit"`
	OrderID       string        `json:"order_id"`
	Keys          []supplierKey `json:"keys"`
	Replayed      bool          `json:"replayed"`
}

type supplierPurchaseRequest struct {
	Count           int
	Region          string
	ClientOrderID   string
	SupplierOrderID string
}

type supplierAPI interface {
	GetStock() (supplierStock, error)
	GetProfile() (supplierProfile, error)
	GetKeys(history bool, page, pageSize int) (supplierKeysPage, error)
	Purchase(request supplierPurchaseRequest) (supplierPurchaseResponse, error)
	SetWebhook(webhookURL string) error
	TestWebhook() error
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

func (c *httpSupplierAPI) apiType() string {
	return config.EffectiveSupplierAPIType(c.provider.APIType)
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
	if c.apiType() == config.SupplierAPITypeAWSMy {
		req.Header.Set("X-API-Key", c.provider.APIToken)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.provider.APIToken)
	}
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
			Error   string `json:"error"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &apiBody)
		message := strings.TrimSpace(apiBody.Error)
		if message == "" {
			message = strings.TrimSpace(apiBody.Message)
		}
		if message == "" {
			message = strings.TrimSpace(string(payload))
		}
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		if len(message) > 500 {
			message = message[:500]
		}
		return &supplierAPIError{StatusCode: resp.StatusCode, Code: strings.TrimSpace(apiBody.Code), Message: message}
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
	if c.apiType() == config.SupplierAPITypeAWSMy {
		var response struct {
			Max int `json:"max"`
		}
		if err := c.do(http.MethodGet, "/api/my/stock", nil, &response); err != nil {
			return supplierStock{}, err
		}
		if response.Max < 0 {
			return supplierStock{}, errors.New("supplier returned a negative stock value")
		}
		// The AWS My protocol exposes one regionless pool. Kiro-Go imports these
		// Kiro keys with its preferred US runtime region, so the normalized US
		// field drives the existing zero-pool replenishment guard.
		return supplierStock{Stock: response.Max, StockUS: response.Max}, nil
	}
	var out supplierStock
	err := c.do(http.MethodGet, "/api/me/stock", nil, &out)
	return out, err
}

func (c *httpSupplierAPI) GetProfile() (supplierProfile, error) {
	if c.apiType() == config.SupplierAPITypeAWSMy {
		var response struct {
			Name string `json:"name"`
		}
		if err := c.do(http.MethodGet, "/api/my/profile", nil, &response); err != nil {
			return supplierProfile{}, err
		}
		var out supplierProfile
		out.User.Name = response.Name
		return out, nil
	}
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
	if c.apiType() == config.SupplierAPITypeKiroApp && pageSize > 500 {
		pageSize = 500
	} else if pageSize > 100000 {
		pageSize = 100000
	}
	if c.apiType() == config.SupplierAPITypeAWSMy {
		query := ""
		if history {
			query = "?history=1"
		}
		var response struct {
			Count  int           `json:"count"`
			Active int           `json:"active"`
			Keys   []supplierKey `json:"keys"`
		}
		if err := c.do(http.MethodGet, "/api/my/keys"+query, nil, &response); err != nil {
			return supplierKeysPage{}, err
		}
		return paginateSupplierKeys(response.Keys, page, pageSize), nil
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

func paginateSupplierKeys(items []supplierKey, page, pageSize int) supplierKeysPage {
	total := len(items)
	pages := 0
	if total > 0 {
		pages = (total + pageSize - 1) / pageSize
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pageItems := append([]supplierKey(nil), items[start:end]...)
	return supplierKeysPage{Items: pageItems, Total: total, Page: page, PageSize: pageSize, Pages: pages}
}

func (c *httpSupplierAPI) Purchase(request supplierPurchaseRequest) (supplierPurchaseResponse, error) {
	var out supplierPurchaseResponse
	body := map[string]any{
		"count":           request.Count,
		"client_order_id": request.ClientOrderID,
	}
	path := "/api/me/purchase"
	if c.apiType() == config.SupplierAPITypeAWSMy {
		path = "/api/my/purchase"
	} else {
		if request.Region != "" {
			body["region"] = request.Region
		}
		if request.SupplierOrderID != "" {
			body["order_id"] = request.SupplierOrderID
		}
	}
	err := c.do(http.MethodPost, path, body, &out)
	if err != nil {
		return out, err
	}
	if c.apiType() == config.SupplierAPITypeAWSMy {
		if !strings.EqualFold(out.ClientOrderID, request.ClientOrderID) {
			return out, errors.New("supplier purchase response returned a different client_order_id")
		}
		if out.Purchased != request.Count || len(out.Keys) != out.Purchased {
			return out, fmt.Errorf("supplier purchase response is incomplete: requested %d, purchased %d, returned %d keys", request.Count, out.Purchased, len(out.Keys))
		}
		seen := make(map[string]struct{}, len(out.Keys))
		for _, item := range out.Keys {
			key := item.Value()
			if key == "" {
				return out, errors.New("supplier purchase response contains an empty key")
			}
			if _, duplicate := seen[key]; duplicate {
				return out, errors.New("supplier purchase response contains duplicate keys")
			}
			seen[key] = struct{}{}
		}
	}
	if out.Requested == 0 {
		out.Requested = request.Count
	}
	if c.apiType() == config.SupplierAPITypeAWSMy && out.OrderID == "" {
		out.OrderID = request.ClientOrderID
	}
	return out, nil
}

func (c *httpSupplierAPI) SetWebhook(webhookURL string) error {
	if c.apiType() != config.SupplierAPITypeAWSMy {
		return errSupplierOperationUnsupported
	}
	var response struct {
		WebhookURL string `json:"webhook_url"`
	}
	if err := c.do(http.MethodPost, "/api/my/webhook", map[string]string{"webhook_url": webhookURL}, &response); err != nil {
		return err
	}
	if response.WebhookURL != webhookURL {
		return errors.New("supplier did not confirm the requested webhook URL")
	}
	return nil
}

func (c *httpSupplierAPI) TestWebhook() error {
	if c.apiType() != config.SupplierAPITypeAWSMy {
		return errSupplierOperationUnsupported
	}
	var response struct {
		OK any `json:"ok"`
	}
	if err := c.do(http.MethodPost, "/api/my/webhook/test", nil, &response); err != nil {
		return err
	}
	switch value := response.OK.(type) {
	case bool:
		if value {
			return nil
		}
	case string:
		if strings.EqualFold(strings.TrimSpace(value), "true") {
			return nil
		}
	}
	return errors.New("supplier webhook test did not return ok=true")
}

// supplierNow is replaceable by tests that need deterministic state timestamps.
var supplierNow = time.Now
