// Package aihub implements the narrow, security-conscious AIHub HTTP surface
// used by the CPA plugin.
package aihub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const DefaultTimeout = 30 * time.Second

// Transport permits deterministic HTTP fixtures without bypassing client policy.
type Transport interface {
	RoundTrip(*http.Request) (*http.Response, error)
}

// Client communicates with the observed AIHub public and account endpoints.
type Client struct {
	baseURL    string
	httpClient pluginapi.HostHTTPClient
}

// NewClient creates a client that bounds every request and rejects redirects.
func NewClient(baseURL string, transport Transport) *Client {
	return NewClientWithTimeout(baseURL, transport, DefaultTimeout)
}

// NewClientWithTimeout is intended for deterministic tests and callers with a
// stricter request budget. A non-positive value falls back to DefaultTimeout.
func NewClientWithTimeout(baseURL string, transport Transport, timeout time.Duration) *Client {
	if transport == nil {
		transport = http.DefaultTransport
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &standardHTTPClient{client: &http.Client{Timeout: timeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}},
	}
}

// NewClientWithHostHTTPClient sends every operation through CPA's HTTP bridge.
// The stdlib constructor is retained only for independent test fixtures.
func NewClientWithHostHTTPClient(baseURL string, httpClient pluginapi.HostHTTPClient) *Client {
	if httpClient == nil {
		return NewClient(baseURL, nil)
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}
}

// Error is safe for host logs: it intentionally excludes remote response bodies.
type Error struct {
	StatusCode int
	Operation  string
	Err        error
}

func (errorValue *Error) Error() string {
	if errorValue.StatusCode > 0 {
		return fmt.Sprintf("aihub %s failed (HTTP %d)", errorValue.Operation, errorValue.StatusCode)
	}
	return fmt.Sprintf("aihub %s failed", errorValue.Operation)
}

type Session struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}
type Account struct {
	ID    string
	Label string
}
type Model struct {
	ID          string
	DisplayName string
}
type Key struct {
	ID      string
	Material string
	GroupID int
}

// ProviderStat is a best-effort parse of one AIHub public provider entry. The
// model_health and model_prices semantics are not yet confirmed, so every field
// is optional and missing values fall back to conservative routing defaults.
type ProviderStat struct {
	GroupID       int
	Available     bool
	CacheHitRate  float64
	ModelHealth   map[string]bool
	ModelPrices   map[string]float64
}

// ProviderStats fetches the public provider catalog without authentication.
func (client *Client) ProviderStats(ctx context.Context) ([]ProviderStat, error) {
	var data []map[string]any
	if err := client.json(ctx, http.MethodGet, "/api/v1/public/providers", "", nil, &data); err != nil {
		return nil, err
	}
	stats := make([]ProviderStat, 0, len(data))
	for _, record := range data {
		groupID := intValue(record["group_id"])
		if groupID <= 0 {
			continue
		}
		stat := ProviderStat{GroupID: groupID, Available: true, ModelHealth: map[string]bool{}, ModelPrices: map[string]float64{}}
		if available, ok := record["available"].(bool); ok {
			stat.Available = available
		}
		if hitRate, ok := parsePercent(record["cache_hit_rate"]); ok {
			stat.CacheHitRate = hitRate
		}
		if health, ok := record["model_health"].(map[string]any); ok {
			for model, value := range health {
				if healthy, ok := value.(bool); ok {
					stat.ModelHealth[strings.ToLower(strings.TrimSpace(model))] = healthy
				}
			}
		}
		if prices, ok := record["model_prices"].(map[string]any); ok {
			for model, value := range prices {
				if price, ok := floatValue(value); ok {
					stat.ModelPrices[strings.ToLower(strings.TrimSpace(model))] = price
				}
			}
		}
		stats = append(stats, stat)
	}
	return stats, nil
}

func (client *Client) Login(ctx context.Context, email, password string) (Session, error) {
	var data map[string]any
	err := client.json(ctx, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": strings.TrimSpace(email), "password": password}, &data)
	if err != nil {
		return Session{}, err
	}
	return parseSession(data)
}
func (client *Client) Refresh(ctx context.Context, refreshToken string) (Session, error) {
	var data map[string]any
	err := client.json(ctx, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refreshToken}, &data)
	if err != nil {
		return Session{}, err
	}
	session, err := parseSession(data)
	if err == nil && session.RefreshToken == "" {
		session.RefreshToken = refreshToken
	}
	return session, err
}
func (client *Client) Me(ctx context.Context, token string) (Account, error) {
	var data map[string]any
	if err := client.json(ctx, http.MethodGet, "/api/v1/auth/me", token, nil, &data); err != nil {
		return Account{}, err
	}
	return Account{ID: stringValue(data["id"]), Label: firstNonEmpty(stringValue(data["email"]), stringValue(data["username"]), stringValue(data["name"]))}, nil
}
func (client *Client) Models(ctx context.Context, token string) ([]Model, error) {
	var data any
	if err := client.json(ctx, http.MethodGet, "/v1/models", token, nil, &data); err != nil {
		return nil, err
	}
	modelItems, ok := data.(map[string]any)["data"].([]any)
	if !ok {
		return nil, &Error{StatusCode: http.StatusOK, Operation: "model discovery", Err: errors.New("invalid model catalog")}
	}
	models := make([]Model, 0, len(modelItems))
	for _, item := range modelItems {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := stringValue(record["id"])
		if id != "" {
			models = append(models, Model{ID: id, DisplayName: firstNonEmpty(stringValue(record["display_name"]), id)})
		}
	}
	return models, nil
}

// CreateKey creates a managed API key bound to a group and returns its plaintext
// material. The material is the only secret and is never included in errors.
func (client *Client) CreateKey(ctx context.Context, token, name string, groupID int) (Key, error) {
	var data map[string]any
	if err := client.json(ctx, http.MethodPost, "/api/v1/keys", token, map[string]any{"name": name, "group_id": groupID}, &data); err != nil {
		return Key{}, err
	}
	key := Key{ID: stringValue(data["id"]), Material: stringValue(data["key"]), GroupID: intValue(data["group_id"])}
	if key.ID == "" || key.Material == "" {
		return Key{}, &Error{StatusCode: http.StatusOK, Operation: "key creation", Err: errors.New("invalid key response")}
	}
	return key, nil
}

// DeleteKey removes a managed API key by its remote identifier.
func (client *Client) DeleteKey(ctx context.Context, token, keyID string) error {
	var data map[string]any
	return client.json(ctx, http.MethodDelete, "/api/v1/keys/"+keyID, token, nil, &data)
}

// KeyExists reports whether a managed API key still exists on the account.
func (client *Client) KeyExists(ctx context.Context, token, keyID string) (bool, error) {
	var data map[string]any
	if err := client.json(ctx, http.MethodGet, "/api/v1/keys?page=1&page_size=1", token, nil, &data); err != nil {
		return false, err
	}
	items, ok := data["items"].([]any)
	if !ok {
		return false, &Error{StatusCode: http.StatusOK, Operation: "key listing", Err: errors.New("invalid key list")}
	}
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if stringValue(record["id"]) == keyID {
			return true, nil
		}
	}
	return false, nil
}
func (client *Client) Execute(ctx context.Context, path, token string, headers http.Header, body []byte) (pluginapi.HTTPResponse, error) {
	response, errRequest := client.do(ctx, http.MethodPost, path, token, headers, body, false)
	if errRequest != nil {
		return pluginapi.HTTPResponse{}, errRequest
	}
	return response.(pluginapi.HTTPResponse), nil
}

// ExecuteBounded uses the host streaming transport even for buffered executor
// responses. This prevents a host HTTP implementation from materializing an
// unbounded upstream body before the plugin can enforce its contract.
func (client *Client) ExecuteBounded(ctx context.Context, path, token string, headers http.Header, body []byte, maximumBytes int64) (pluginapi.HTTPResponse, error) {
	response, errExecute := client.ExecuteStream(ctx, path, token, headers, body)
	if errExecute != nil {
		return pluginapi.HTTPResponse{}, errExecute
	}
	payload, errRead := CollectBoundedStream(response.Chunks, maximumBytes)
	if errRead != nil {
		return pluginapi.HTTPResponse{}, errRead
	}
	return pluginapi.HTTPResponse{StatusCode: response.StatusCode, Headers: response.Headers, Body: payload}, nil
}

func (client *Client) ExecuteStream(ctx context.Context, path, token string, headers http.Header, body []byte) (pluginapi.HTTPStreamResponse, error) {
	response, errRequest := client.do(ctx, http.MethodPost, path, token, headers, body, true)
	if errRequest != nil {
		return pluginapi.HTTPStreamResponse{}, errRequest
	}
	return response.(pluginapi.HTTPStreamResponse), nil
}

// CollectBoundedStream copies at most maximumBytes of a host-provided stream.
// It checks each chunk before appending, so no oversized payload is retained.
func CollectBoundedStream(chunks <-chan pluginapi.HTTPStreamChunk, maximumBytes int64) ([]byte, error) {
	if maximumBytes <= 0 {
		return nil, errors.New("maximum response size must be positive")
	}
	payload := make([]byte, 0)
	for chunk := range chunks {
		if chunk.Err != nil {
			return nil, chunk.Err
		}
		if int64(len(chunk.Payload)) > maximumBytes-int64(len(payload)) {
			return nil, errors.New("response exceeds configured byte limit")
		}
		payload = append(payload, chunk.Payload...)
	}
	return payload, nil
}

func (client *Client) json(ctx context.Context, method, path, token string, payload any, destination any) error {
	var body []byte
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = raw
	}
	headers := http.Header{"Accept": []string{"application/json"}}
	if payload != nil {
		headers.Set("Content-Type", "application/json")
	}
	responseValue, errRequest := client.do(ctx, method, path, token, headers, body, false)
	if errRequest != nil {
		return errRequest
	}
	response := responseValue.(pluginapi.HTTPResponse)
	var rawResponse json.RawMessage
	if err := json.Unmarshal(response.Body, &rawResponse); err != nil {
		return &Error{StatusCode: response.StatusCode, Operation: path, Err: errors.New("invalid JSON response")}
	}
	var envelope struct {
		Code any             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rawResponse, &envelope); err != nil {
		return &Error{StatusCode: response.StatusCode, Operation: path, Err: errors.New("invalid JSON response")}
	}
	if envelope.Code == nil {
		return json.Unmarshal(rawResponse, destination)
	}
	if envelope.Code != nil && fmt.Sprint(envelope.Code) != "0" {
		return &Error{StatusCode: response.StatusCode, Operation: path, Err: errors.New("API error")}
	}
	if envelope.Data == nil {
		return &Error{StatusCode: response.StatusCode, Operation: path, Err: errors.New("response data missing")}
	}
	return json.Unmarshal(envelope.Data, destination)
}

func (client *Client) do(ctx context.Context, method, path, token string, headers http.Header, body []byte, stream bool) (any, error) {
	requestURL, errURL := client.requestURL(path)
	if errURL != nil {
		return nil, &Error{Operation: path, Err: errURL}
	}
	requestHeaders := safeRequestHeaders(headers)
	if token != "" {
		requestHeaders.Set("Authorization", "Bearer "+token)
	}
	requestHeaders.Set("Accept-Encoding", "identity")
	request := pluginapi.HTTPRequest{Method: method, URL: requestURL, Headers: requestHeaders, Body: append([]byte(nil), body...)}
	if stream {
		response, errDo := client.httpClient.DoStream(ctx, request)
		if errDo != nil {
			return nil, &Error{Operation: path, Err: errDo}
		}
		if errStatus := responseError(path, response.StatusCode); errStatus != nil {
			return nil, errStatus
		}
		return response, nil
	}
	response, errDo := client.httpClient.Do(ctx, request)
	if errDo != nil {
		return nil, &Error{Operation: path, Err: errDo}
	}
	if errStatus := responseError(path, response.StatusCode); errStatus != nil {
		return nil, errStatus
	}
	return response, nil
}

func responseError(operation string, statusCode int) error {
	if statusCode >= 300 && statusCode < 400 {
		return &Error{StatusCode: statusCode, Operation: operation, Err: errors.New("redirect rejected")}
	}
	if statusCode < 200 || statusCode >= 300 {
		return &Error{StatusCode: statusCode, Operation: operation, Err: errors.New("upstream error")}
	}
	return nil
}

type standardHTTPClient struct{ client *http.Client }

func (client *standardHTTPClient) Do(ctx context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	response, errDo := client.request(ctx, request)
	if errDo != nil {
		return pluginapi.HTTPResponse{}, errDo
	}
	defer response.Body.Close()
	body, errRead := io.ReadAll(response.Body)
	if errRead != nil {
		return pluginapi.HTTPResponse{}, errRead
	}
	return pluginapi.HTTPResponse{StatusCode: response.StatusCode, Headers: response.Header.Clone(), Body: body}, nil
}

func (client *standardHTTPClient) DoStream(ctx context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	response, errDo := client.request(ctx, request)
	if errDo != nil {
		return pluginapi.HTTPStreamResponse{}, errDo
	}
	chunks := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(chunks)
		defer response.Body.Close()
		buffer := make([]byte, 32*1024)
		for {
			count, errRead := response.Body.Read(buffer)
			if count > 0 {
				chunks <- pluginapi.HTTPStreamChunk{Payload: append([]byte(nil), buffer[:count]...)}
			}
			if errRead != nil {
				if !errors.Is(errRead, io.EOF) {
					chunks <- pluginapi.HTTPStreamChunk{Err: errRead}
				}
				return
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{StatusCode: response.StatusCode, Headers: response.Header.Clone(), Chunks: chunks}, nil
}

func (client *standardHTTPClient) request(ctx context.Context, request pluginapi.HTTPRequest) (*http.Response, error) {
	httpRequest, errNewRequest := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(request.Body))
	if errNewRequest != nil {
		return nil, errNewRequest
	}
	httpRequest.Header = request.Headers.Clone()
	return client.client.Do(httpRequest)
}

func (client *Client) requestURL(path string) (string, error) {
	baseURL, errParse := url.Parse(client.baseURL)
	if errParse != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return "", errors.New("invalid configured base URL")
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return "", errors.New("unsafe configured base URL")
	}
	endpointURL, errParse := url.Parse(path)
	if errParse != nil || !strings.HasPrefix(path, "/") || endpointURL.IsAbs() || endpointURL.Host != "" {
		return "", errors.New("invalid endpoint path")
	}
	return client.baseURL + path, nil
}

func parseSession(data map[string]any) (Session, error) {
	token := firstNonEmpty(stringValue(data["access_token"]), stringValue(data["accessToken"]), stringValue(data["token"]))
	if token == "" {
		return Session{}, &Error{StatusCode: http.StatusOK, Operation: "authentication", Err: errors.New("access token missing")}
	}
	expiresIn, _ := data["expires_in"].(float64)
	return Session{AccessToken: token, RefreshToken: firstNonEmpty(stringValue(data["refresh_token"]), stringValue(data["refreshToken"])), ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second)}, nil
}
func stringValue(value any) string {
	stringResult, _ := value.(string)
	return strings.TrimSpace(stringResult)
}
func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(typed))
		return parsed
	}
	return 0
}
func floatValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, errParse == nil
	}
	return 0, false
}
func parsePercent(value any) (float64, bool) {
	if raw, ok := value.(string); ok {
		raw = strings.TrimSpace(raw)
		if strings.HasSuffix(raw, "%") {
			parsed, errParse := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64)
			if errParse == nil {
				return parsed / 100, true
			}
		}
	}
	parsed, ok := floatValue(value)
	return parsed, ok && parsed >= 0 && parsed <= 1
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func safeRequestHeaders(source http.Header) http.Header {
	destination := make(http.Header)
	for name, values := range source {
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization", "host", "content-length", "connection", "transfer-encoding":
			continue
		}
		destination[name] = append([]string(nil), values...)
	}
	return destination
}
