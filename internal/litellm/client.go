package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to the LiteLLM proxy Admin API with the master key.
type Client struct {
	BaseURL   string
	MasterKey string
	HTTP      *http.Client
}

// New builds a client. baseURL is the proxy root, e.g. https://gw.example.com.
func New(baseURL, masterKey string, timeout time.Duration) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		MasterKey: masterKey,
		HTTP:      &http.Client{Timeout: timeout},
	}
}

// APIError is a non-2xx answer from the proxy.
type APIError struct {
	Status  int
	Path    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("litellm %s: HTTP %d: %s", e.Path, e.Status, e.Message)
}

// NotFound reports whether the proxy does not implement the endpoint. Used to
// degrade gracefully on LiteLLM versions without /config/update.
func (e *APIError) NotFound() bool {
	return e.Status == http.StatusNotFound || e.Status == http.StatusMethodNotAllowed
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request for %s: %w", path, err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.MasterKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read response of %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Path: path, Message: errorMessage(raw)}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response of %s: %w", path, err)
	}
	return nil
}

// errorMessage digs the human-readable part out of a LiteLLM error body,
// falling back to the raw payload when the shape is unfamiliar.
func errorMessage(raw []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Detail any `json:"detail"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		if envelope.Error.Message != "" {
			return envelope.Error.Message
		}
		switch detail := envelope.Detail.(type) {
		case string:
			if detail != "" {
				return detail
			}
		case map[string]any:
			if msg, ok := detail["error"].(string); ok && msg != "" {
				return msg
			}
			if inner, ok := detail["error"].(map[string]any); ok {
				if msg, ok := inner["message"].(string); ok && msg != "" {
					return msg
				}
			}
		}
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	if text == "" {
		return "(empty body)"
	}
	return text
}

// ListDeployments returns the current model_list of the proxy.
func (c *Client) ListDeployments(ctx context.Context) ([]Deployment, error) {
	var out struct {
		Data []Deployment `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/model/info", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// CreateDeployment adds a deployment to the model_list.
func (c *Client) CreateDeployment(ctx context.Context, d Deployment) error {
	return c.do(ctx, http.MethodPost, "/model/new", d, nil)
}

// UpdateDeployment rewrites an existing deployment in place.
func (c *Client) UpdateDeployment(ctx context.Context, d Deployment) error {
	return c.do(ctx, http.MethodPost, "/model/update", d, nil)
}

// DeleteDeployment removes a deployment by its control-plane id.
func (c *Client) DeleteDeployment(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/model/delete", map[string]string{"id": id}, nil)
}

// ListKeys returns every virtual key known to the proxy, managed or not.
func (c *Client) ListKeys(ctx context.Context) ([]Key, error) {
	var out struct {
		Keys []Key `json:"keys"`
	}
	path := "/key/list?return_full_object=true&include_team_keys=false&size=500"
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Keys, nil
}

// GenerateKey creates a virtual key and returns the secret. The secret is not
// retrievable afterwards — the proxy stores only its hash.
func (c *Client) GenerateKey(ctx context.Context, req GenerateKeyRequest) (GeneratedKey, error) {
	var out GeneratedKey
	if err := c.do(ctx, http.MethodPost, "/key/generate", req, &out); err != nil {
		return GeneratedKey{}, err
	}
	return out, nil
}

// UpdateKey changes attributes of an existing key.
func (c *Client) UpdateKey(ctx context.Context, req UpdateKeyRequest) error {
	return c.do(ctx, http.MethodPost, "/key/update", req, nil)
}

// DeleteKeys revokes keys immediately, identified by their token hashes.
func (c *Client) DeleteKeys(ctx context.Context, tokens []string) error {
	if len(tokens) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodPost, "/key/delete", map[string]any{"keys": tokens}, nil)
}

// Liveness reports whether the proxy process answers at all.
func (c *Client) Liveness(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health/liveness", nil, nil)
}

// Readiness reports whether the proxy considers its dependencies usable.
func (c *Client) Readiness(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	if err := c.do(ctx, http.MethodGet, "/health/readiness", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ProviderHealth asks the proxy to probe every configured deployment.
func (c *Client) ProviderHealth(ctx context.Context) (HealthReport, error) {
	var out HealthReport
	if err := c.do(ctx, http.MethodGet, "/health", nil, &out); err != nil {
		return HealthReport{}, err
	}
	return out, nil
}

// SpendLogs returns accounted requests in [start, end]. Dates are YYYY-MM-DD.
func (c *Client) SpendLogs(ctx context.Context, start, end time.Time) ([]SpendLog, error) {
	query := url.Values{}
	query.Set("start_date", start.Format("2006-01-02"))
	query.Set("end_date", end.Format("2006-01-02"))

	var out []SpendLog
	if err := c.do(ctx, http.MethodGet, "/spend/logs?"+query.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

var _ API = (*Client)(nil)
