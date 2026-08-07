package proxy

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"math"
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

func isSupplierPublicInventoryRaceError(err error) bool {
	var apiErr *supplierAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case "STOCK_CHANGED", "BATCH_NOT_AVAILABLE", "INSUFFICIENT_BATCH_STOCK", "NO_STOCK", "BATCH_NOT_FOUND":
		return true
	default:
		return false
	}
}

type supplierStock struct {
	Stock    int                   `json:"stock"`
	StockUS  int                   `json:"stock_us"`
	StockEU  int                   `json:"stock_eu"`
	Price    float64               `json:"price"`
	PriceMin float64               `json:"price_min"`
	PriceMax float64               `json:"price_max"`
	Balance  float64               `json:"balance"`
	Batches  []supplierPublicBatch `json:"-"`
}

type supplierPublicBatch struct {
	BatchID           string  `json:"batch_id"`
	Available         int     `json:"available"`
	PublishedAt       string  `json:"published_at,omitempty"`
	LastHealthCheckAt *string `json:"last_health_check_at,omitempty"`
}

type supplierPublicStock struct {
	Total   int                   `json:"total"`
	Batches []supplierPublicBatch `json:"batches"`
}

type supplierPublicPurchaseOrder struct {
	ClientOrderID string `json:"client_order_id"`
	Requested     int    `json:"requested"`
	Purchased     int    `json:"purchased"`
	SourceIP      string `json:"source_ip,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
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
	Region      string  `json:"region,omitempty"`
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
	BatchID       string        `json:"batch_id,omitempty"`
	ClientOrderID string        `json:"client_order_id,omitempty"`
	Purchased     int           `json:"purchased"`
	Requested     int           `json:"requested"`
	Remaining     any           `json:"remaining,omitempty"`
	UnitPrice     float64       `json:"unit_price"`
	TotalDebit    float64       `json:"total_debit"`
	OrderID       string        `json:"order_id"`
	Region        string        `json:"region,omitempty"`
	Status        string        `json:"status,omitempty"`
	RefundedCNY   any           `json:"refunded_amount_cny,omitempty"`
	Keys          []supplierKey `json:"keys"`
	Replayed      bool          `json:"replayed"`
}

type supplierPurchaseRequest struct {
	Count           int
	Region          string
	PurchaseSource  string
	BatchID         string
	ClientOrderID   string
	SupplierOrderID string
}

type supplierAPI interface {
	GetStock() (supplierStock, error)
	GetPublicStock() (supplierPublicStock, error)
	GetPublicPurchaseOrders() ([]supplierPublicPurchaseOrder, error)
	GetProfile() (supplierProfile, error)
	GetKeys(history bool, page, pageSize int) (supplierKeysPage, error)
	Purchase(request supplierPurchaseRequest) (supplierPurchaseResponse, error)
	SetWebhook(webhookURL string) (string, error)
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
	if c.apiType() == config.SupplierAPITypeAWSMy || c.apiType() == config.SupplierAPITypeKiroDrop {
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
			Error   string          `json:"error"`
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
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
		code := strings.TrimSpace(string(apiBody.Code))
		code = strings.Trim(code, `"`)
		if code == "null" {
			code = ""
		}
		return &supplierAPIError{StatusCode: resp.StatusCode, Code: code, Message: message}
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
	if c.apiType() == config.SupplierAPITypeKiroDrop {
		us, err := c.getKiroDropRegionStock("us")
		if err != nil {
			return supplierStock{}, err
		}
		eu, err := c.getKiroDropRegionStock("eu")
		if err != nil {
			return supplierStock{}, err
		}
		priceMin, priceMax := us.Price, eu.Price
		if priceMin > priceMax {
			priceMin, priceMax = priceMax, priceMin
		}
		return supplierStock{
			Stock: us.Stock + eu.Stock, StockUS: us.Stock, StockEU: eu.Stock,
			PriceMin: priceMin, PriceMax: priceMax, Balance: us.Balance,
		}, nil
	}
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

type supplierDecimal float64

func (d *supplierDecimal) UnmarshalJSON(data []byte) error {
	value := strings.TrimSpace(string(data))
	if value == "" || value == "null" {
		*d = 0
		return nil
	}
	value = strings.Trim(value, `"`)
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return fmt.Errorf("invalid decimal value %q", value)
	}
	*d = supplierDecimal(parsed)
	return nil
}

type kiroDropRegionStock struct {
	Region  string          `json:"region"`
	Stock   int             `json:"stock"`
	Price   supplierDecimal `json:"price"`
	Balance supplierDecimal `json:"balance"`
}

func (c *httpSupplierAPI) getKiroDropRegionStock(region string) (supplierStock, error) {
	query := url.Values{}
	query.Set("region", region)
	var response kiroDropRegionStock
	if err := c.do(http.MethodGet, "/api/me/stock?"+query.Encode(), nil, &response); err != nil {
		return supplierStock{}, err
	}
	expectedRegion := "us-east-1"
	if region == "eu" {
		expectedRegion = "eu-central-1"
	}
	if response.Region != expectedRegion {
		return supplierStock{}, fmt.Errorf("supplier returned region %q for %s stock", response.Region, strings.ToUpper(region))
	}
	if response.Stock < 0 || response.Price < 0 || response.Balance < 0 {
		return supplierStock{}, errors.New("supplier returned a negative stock, price, or balance value")
	}
	return supplierStock{Stock: response.Stock, Price: float64(response.Price), Balance: float64(response.Balance)}, nil
}

func (c *httpSupplierAPI) GetPublicStock() (supplierPublicStock, error) {
	if c.apiType() != config.SupplierAPITypeAWSMy {
		return supplierPublicStock{}, errSupplierOperationUnsupported
	}
	var out supplierPublicStock
	if err := c.do(http.MethodGet, "/api/public/stock", nil, &out); err != nil {
		return supplierPublicStock{}, err
	}
	if out.Total < 0 {
		return supplierPublicStock{}, errors.New("supplier returned a negative public stock total")
	}
	sum := 0
	seen := make(map[string]struct{}, len(out.Batches))
	for i := range out.Batches {
		out.Batches[i].BatchID = strings.TrimSpace(out.Batches[i].BatchID)
		if out.Batches[i].BatchID == "" {
			return supplierPublicStock{}, errors.New("supplier public stock contains an empty batch_id")
		}
		if out.Batches[i].Available < 0 {
			return supplierPublicStock{}, errors.New("supplier public stock contains a negative batch quantity")
		}
		if _, duplicate := seen[out.Batches[i].BatchID]; duplicate {
			return supplierPublicStock{}, errors.New("supplier public stock contains duplicate batch_id values")
		}
		seen[out.Batches[i].BatchID] = struct{}{}
		if out.Batches[i].Available > out.Total || sum > out.Total-out.Batches[i].Available {
			return supplierPublicStock{}, fmt.Errorf("supplier public stock total mismatch: total=%d", out.Total)
		}
		sum += out.Batches[i].Available
	}
	if sum != out.Total {
		return supplierPublicStock{}, fmt.Errorf("supplier public stock total mismatch: total=%d batches=%d", out.Total, sum)
	}
	return out, nil
}

func (c *httpSupplierAPI) GetPublicPurchaseOrders() ([]supplierPublicPurchaseOrder, error) {
	if c.apiType() != config.SupplierAPITypeAWSMy {
		return nil, errSupplierOperationUnsupported
	}
	var out []supplierPublicPurchaseOrder
	if err := c.do(http.MethodGet, "/api/public/purchase-orders", nil, &out); err != nil {
		return nil, err
	}
	if len(out) > 50 {
		return nil, errors.New("supplier returned more than 50 public purchase orders")
	}
	seen := make(map[string]struct{}, len(out))
	for i := range out {
		out[i].ClientOrderID = strings.TrimSpace(out[i].ClientOrderID)
		if !isSupplier32HexID(out[i].ClientOrderID) {
			return nil, errors.New("supplier public order contains an invalid client_order_id")
		}
		if out[i].Requested < 1 || out[i].Purchased != out[i].Requested {
			return nil, errors.New("supplier public order contains an invalid quantity")
		}
		if _, duplicate := seen[out[i].ClientOrderID]; duplicate {
			return nil, errors.New("supplier returned duplicate public purchase orders")
		}
		seen[out[i].ClientOrderID] = struct{}{}
	}
	return out, nil
}

func (c *httpSupplierAPI) GetProfile() (supplierProfile, error) {
	if c.apiType() == config.SupplierAPITypeKiroDrop {
		var response struct {
			Name      string          `json:"name"`
			Remaining supplierDecimal `json:"remaining"`
		}
		if err := c.do(http.MethodGet, "/api/my/profile", nil, &response); err != nil {
			return supplierProfile{}, err
		}
		if response.Remaining < 0 {
			return supplierProfile{}, errors.New("supplier returned a negative balance")
		}
		var out supplierProfile
		out.User.Name = strings.TrimSpace(response.Name)
		out.User.Balance = float64(response.Remaining)
		return out, nil
	}
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
	if c.apiType() == config.SupplierAPITypeKiroDrop {
		return supplierKeysPage{}, errSupplierOperationUnsupported
	}
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
		if config.EffectiveSupplierPurchaseSource(request.PurchaseSource) == config.SupplierPurchaseSourcePublic {
			request.BatchID = strings.TrimSpace(request.BatchID)
			if request.BatchID == "" {
				return out, errors.New("batch_id is required for a public supplier purchase")
			}
			path = "/api/public/purchase"
			body["batch_id"] = request.BatchID
		} else {
			path = "/api/my/purchase"
		}
	} else if c.apiType() == config.SupplierAPITypeKiroDrop {
		path = "/api/my/purchase"
		body["region"] = request.Region
		if request.SupplierOrderID != "" {
			body["order_id"] = request.SupplierOrderID
		}
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
	if c.apiType() == config.SupplierAPITypeKiroDrop {
		status := strings.ToLower(strings.TrimSpace(out.Status))
		if status == "refunded" || status == "partially_refunded" {
			return out, &supplierAPIError{StatusCode: http.StatusConflict, Code: "PURCHASE_REFUNDED", Message: "the idempotent supplier order was refunded and its keys will not be imported"}
		}
	}
	if c.apiType() == config.SupplierAPITypeAWSMy || c.apiType() == config.SupplierAPITypeKiroDrop {
		if !strings.EqualFold(out.ClientOrderID, request.ClientOrderID) {
			return out, errors.New("supplier purchase response returned a different client_order_id")
		}
		if out.Purchased != request.Count || len(out.Keys) != out.Purchased {
			return out, fmt.Errorf("supplier purchase response is incomplete: requested %d, purchased %d, returned %d keys", request.Count, out.Purchased, len(out.Keys))
		}
		if config.EffectiveSupplierPurchaseSource(request.PurchaseSource) == config.SupplierPurchaseSourcePublic && out.BatchID != request.BatchID {
			return out, errors.New("supplier public purchase response returned a different batch_id")
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
	if c.apiType() == config.SupplierAPITypeKiroDrop {
		status := strings.ToLower(strings.TrimSpace(out.Status))
		if status != "completed" {
			return out, fmt.Errorf("supplier purchase returned unsupported status %q", out.Status)
		}
		if strings.TrimSpace(out.OrderID) == "" {
			return out, errors.New("supplier purchase response is missing order_id")
		}
		expectedRegion := "us-east-1"
		if request.Region == "eu" {
			expectedRegion = "eu-central-1"
		}
		if out.Region != expectedRegion {
			return out, fmt.Errorf("supplier purchase returned region %q, want %q", out.Region, expectedRegion)
		}
		for _, item := range out.Keys {
			if item.Region != expectedRegion {
				return out, fmt.Errorf("supplier purchase returned a key for region %q, want %q", item.Region, expectedRegion)
			}
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

func (c *httpSupplierAPI) SetWebhook(webhookURL string) (string, error) {
	if c.apiType() != config.SupplierAPITypeAWSMy && c.apiType() != config.SupplierAPITypeKiroDrop {
		return "", errSupplierOperationUnsupported
	}
	var response struct {
		OK            any    `json:"ok"`
		WebhookURL    string `json:"webhook_url"`
		WebhookSecret string `json:"webhook_secret"`
	}
	method := http.MethodPost
	if c.apiType() == config.SupplierAPITypeKiroDrop {
		method = http.MethodPut
	}
	if err := c.do(method, "/api/my/webhook", map[string]string{"webhook_url": webhookURL}, &response); err != nil {
		return "", err
	}
	if response.WebhookURL != webhookURL {
		return "", errors.New("supplier did not confirm the requested webhook URL")
	}
	if c.apiType() == config.SupplierAPITypeKiroDrop {
		if !supplierOK(response.OK) {
			return "", errors.New("supplier webhook configuration did not return ok=true")
		}
		secret := strings.TrimSpace(response.WebhookSecret)
		if len(secret) != 64 {
			return "", errors.New("supplier returned an invalid webhook signing secret")
		}
		if _, err := hex.DecodeString(secret); err != nil {
			return "", errors.New("supplier returned an invalid webhook signing secret")
		}
		return secret, nil
	}
	return "", nil
}

func (c *httpSupplierAPI) TestWebhook() error {
	if c.apiType() != config.SupplierAPITypeAWSMy && c.apiType() != config.SupplierAPITypeKiroDrop {
		return errSupplierOperationUnsupported
	}
	if c.apiType() == config.SupplierAPITypeKiroDrop {
		return c.do(http.MethodPost, "/api/my/webhook/test", nil, nil)
	}
	var response struct {
		OK any `json:"ok"`
	}
	if err := c.do(http.MethodPost, "/api/my/webhook/test", nil, &response); err != nil {
		return err
	}
	if supplierOK(response.OK) {
		return nil
	}
	return errors.New("supplier webhook test did not return ok=true")
}

func supplierOK(value any) bool {
	switch value := value.(type) {
	case bool:
		if value {
			return true
		}
	case string:
		if strings.EqualFold(strings.TrimSpace(value), "true") {
			return true
		}
	}
	return false
}

// supplierNow is replaceable by tests that need deterministic state timestamps.
var supplierNow = time.Now
