package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SessionsSnapshot represents the remote session and call ledger payload returned by GET /api/gateway/sessions.
type SessionsSnapshot struct {
	Sessions []TaskSession `json:"sessions"`
	Calls    []CallRecord  `json:"calls"`
}

// Client queries a remote Model Gateway instance for configuration, health, and live AX Task session telemetry.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient constructs a remote Model Gateway query client.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		httpClient: httpClient,
	}
}

// BaseURL returns the normalized base URL of the remote Model Gateway.
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

// HealthCheck probes GET /healthz on the remote Model Gateway.
func (c *Client) HealthCheck(ctx context.Context) error {
	if c == nil || c.baseURL == "" {
		return fmt.Errorf("model gateway baseURL is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s/healthz: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s/healthz status %d: %s", c.baseURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// GetConfig fetches GET /api/gateway/config from the remote Model Gateway.
func (c *Client) GetConfig(ctx context.Context) (Config, error) {
	if c == nil || c.baseURL == "" {
		return Config{}, fmt.Errorf("model gateway baseURL is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/gateway/config", nil)
	if err != nil {
		return Config{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Config{}, fmt.Errorf("GET %s/api/gateway/config: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Config{}, fmt.Errorf("GET %s/api/gateway/config status %d: %s", c.baseURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var cfg Config
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decoding gateway config: %w", err)
	}
	return cfg, nil
}

// QuerySessions fetches GET /api/gateway/sessions (both sessions and calls) from the remote Model Gateway.
func (c *Client) QuerySessions(ctx context.Context) (SessionsSnapshot, error) {
	if c == nil || c.baseURL == "" {
		return SessionsSnapshot{}, fmt.Errorf("model gateway baseURL is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/gateway/sessions", nil)
	if err != nil {
		return SessionsSnapshot{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return SessionsSnapshot{}, fmt.Errorf("GET %s/api/gateway/sessions: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return SessionsSnapshot{}, fmt.Errorf("GET %s/api/gateway/sessions status %d: %s", c.baseURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out SessionsSnapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return SessionsSnapshot{}, fmt.Errorf("decoding gateway sessions: %w", err)
	}
	return out, nil
}

// ListSessions fetches the sorted list of TaskSession entries from GET /api/gateway/sessions.
func (c *Client) ListSessions(ctx context.Context) ([]TaskSession, error) {
	snap, err := c.QuerySessions(ctx)
	if err != nil {
		return nil, err
	}
	return snap.Sessions, nil
}

// GetSession fetches GET /api/gateway/sessions/{taskName} from the remote Model Gateway.
// Returns (TaskSession{}, false, nil) if the session does not exist (HTTP 404).
func (c *Client) GetSession(ctx context.Context, taskName string) (TaskSession, bool, error) {
	if c == nil || c.baseURL == "" {
		return TaskSession{}, false, fmt.Errorf("model gateway baseURL is empty")
	}
	trimmed := strings.TrimSpace(taskName)
	if trimmed == "" {
		return TaskSession{}, false, fmt.Errorf("taskName is required")
	}
	endpoint := c.baseURL + "/api/gateway/sessions/" + url.PathEscape(trimmed)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return TaskSession{}, false, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return TaskSession{}, false, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return TaskSession{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return TaskSession{}, false, fmt.Errorf("GET %s status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var sess TaskSession
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&sess); err != nil {
		return TaskSession{}, false, fmt.Errorf("decoding gateway session %q: %w", trimmed, err)
	}
	return sess, true, nil
}
