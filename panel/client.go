// Package panel is a small, dependency-free client for the PasarGuard panel
// API endpoints needed by tblocker.
package panel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBody = 1 << 20 // 1 MiB

// User mirrors the subset of PasarGuard's UserResponse that tblocker needs.
type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Status   string `json:"status"`
	Note     string `json:"note"`
}

// PasarGuard user statuses.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusLimited  = "limited"
	StatusExpired  = "expired"
	StatusOnHold   = "on_hold"
)

// Node is the subset of a PasarGuard node needed by the failsafe.
type Node struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// PasarGuard node statuses used by the failsafe filter.
const (
	NodeStatusDisabled = "disabled"
	NodeStatusLimited  = "limited"
)

type nodesResponse struct {
	Nodes []Node `json:"nodes"`
	Total int    `json:"total"`
}

// APIError is returned for non-2xx panel responses.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Detail     string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("PasarGuard API %s %s returned HTTP %d", e.Method, e.Path, e.StatusCode)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// IsNotFound reports whether the panel returned 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsDefinitive reports whether retrying the same request cannot help. Network
// errors, timeouts, 429, and 5xx are left as non-definitive so callers can
// reconcile or retry.
func IsDefinitive(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// Client talks to a PasarGuard panel.
type Client struct {
	baseURL         string
	apiKey          string
	httpClient      *http.Client
	reconnectClient *http.Client
	retries         int
	retryBase       time.Duration
	userAgent       string
	insecure        bool
}

// Option customises a Client.
type Option func(*Client)

// WithRetries sets the number of retries after the first attempt.
func WithRetries(n int) Option {
	return func(c *Client) {
		if n >= 0 {
			c.retries = n
		}
	}
}

// WithRetryBackoff sets the base delay between retries.
func WithRetryBackoff(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.retryBase = d
		}
	}
}

// WithInsecureTLS disables certificate verification.
func WithInsecureTLS(insecure bool) Option {
	return func(c *Client) {
		c.insecure = insecure
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if strings.TrimSpace(ua) != "" {
			c.userAgent = ua
		}
	}
}

// WithHTTPClient replaces the HTTP client (used by tests).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// New creates a panel client. baseURL is the panel root, optionally with a
// path prefix; trailing slashes are ignored.
func New(baseURL, apiKey string, timeout time.Duration, opts ...Option) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse panel base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("panel base URL must use http or https")
	}
	if u.Host == "" {
		return nil, errors.New("panel base URL must include a host")
	}
	if u.User != nil {
		return nil, errors.New("panel base URL must not contain credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("panel base URL must not contain a query or fragment")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("panel API key must not be empty")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	c := &Client{
		baseURL:   baseURL,
		apiKey:    strings.TrimSpace(apiKey),
		retries:   2,
		retryBase: 250 * time.Millisecond,
		userAgent: "tblocker",
	}
	for _, opt := range opts {
		opt(c)
	}

	if c.httpClient == nil {
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			base = &http.Transport{}
		}
		transport := base.Clone()
		if c.insecure {
			if transport.TLSClientConfig == nil {
				transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			} else {
				transport.TLSClientConfig = transport.TLSClientConfig.Clone()
			}
			// The operator explicitly opted in through config.
			transport.TLSClientConfig.InsecureSkipVerify = true
		}
		c.httpClient = &http.Client{
			Timeout:   timeout,
			Transport: transport,
			// Never forward X-Api-Key or other headers to a redirect target.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	// A PasarGuard node reconnect stops and starts the core and waits for the
	// new core. That can take much longer than a normal user API call, so give
	// it a separate long timeout. Retries are disabled for this operation: a
	// timed-out request may still have restarted the core, and retrying would
	// restart it again.
	reconnectClient := *c.httpClient
	reconnectClient.Timeout = 3 * time.Minute
	c.reconnectClient = &reconnectClient

	return c, nil
}

// GetUserByID fetches a user by numeric ID.
func (c *Client) GetUserByID(ctx context.Context, id int) (*User, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid PasarGuard user ID %d", id)
	}
	var user User
	path := fmt.Sprintf("/api/user/by-id/%d", id)
	if err := c.do(ctx, http.MethodGet, path, nil, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// GetUserByUsername fetches a user by username.
func (c *Client) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, errors.New("empty PasarGuard username")
	}
	var user User
	path := "/api/user/by-username/" + url.PathEscape(username)
	if err := c.do(ctx, http.MethodGet, path, nil, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// SetDisabledByID enables or disables a user by numeric ID.
func (c *Client) SetDisabledByID(ctx context.Context, id int, disabled bool) (*User, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid PasarGuard user ID %d", id)
	}
	var user User
	path := fmt.Sprintf("/api/user/by-id/%d/disabled", id)
	body := struct {
		Disabled bool `json:"disabled"`
	}{Disabled: disabled}
	if err := c.do(ctx, http.MethodPut, path, body, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// SetDisabledByUsername enables or disables a user by username.
func (c *Client) SetDisabledByUsername(ctx context.Context, username string, disabled bool) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, errors.New("empty PasarGuard username")
	}
	var user User
	path := "/api/user/by-username/" + url.PathEscape(username) + "/disabled"
	body := struct {
		Disabled bool `json:"disabled"`
	}{Disabled: disabled}
	if err := c.do(ctx, http.MethodPut, path, body, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// Admin is the subset of /api/admin used for a startup auth check.
type Admin struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Status   string `json:"status"`
}

// ListEnabledNodes returns nodes that PasarGuard considers enabled. The panel
// excludes disabled and limited nodes from this query; the caller filters the
// status again as a defensive check.
func (c *Client) ListEnabledNodes(ctx context.Context) ([]Node, error) {
	var response nodesResponse
	if err := c.do(ctx, http.MethodGet, "/api/nodes?enabled=true", nil, &response); err != nil {
		return nil, err
	}
	return response.Nodes, nil
}

// ReconnectNode asks PasarGuard to reconnect one node. On the node side this
// stops the running core and starts it again, which drops established
// connections that a panel-side disable could not terminate.
func (c *Client) ReconnectNode(ctx context.Context, id int) error {
	if id <= 0 {
		return fmt.Errorf("invalid PasarGuard node ID %d", id)
	}
	return c.doWithClient(ctx, http.MethodPost, fmt.Sprintf("/api/nodes/%d/reconnect", id), nil, nil, c.reconnectClient, 0)
}

// GetCurrentAdmin verifies the base URL and credentials.
func (c *Client) GetCurrentAdmin(ctx context.Context) (*Admin, error) {
	var admin Admin
	if err := c.do(ctx, http.MethodGet, "/api/admin", nil, &admin); err != nil {
		return nil, err
	}
	return &admin, nil
}

// Check probes the API. It verifies that the URL points at PasarGuard, that the
// credentials are accepted, and that the key can read users. A 404 for the
// probe username is the expected (successful) answer for the last check.
func (c *Client) Check(ctx context.Context) error {
	if _, err := c.GetCurrentAdmin(ctx); err != nil {
		return fmt.Errorf("auth check: %w", err)
	}
	_, err := c.GetUserByUsername(ctx, "__tblocker_startup_probe__")
	if err == nil || IsNotFound(err) {
		return nil
	}
	return fmt.Errorf("user read check: %w", err)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	return c.doWithClient(ctx, method, path, body, out, c.httpClient, c.retries)
}

func (c *Client) doWithClient(ctx context.Context, method, path string, body any, out any, httpClient *http.Client, retries int) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s body: %w", method, path, err)
		}
	}

	endpoint := c.baseURL + path

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("create %s %s request: %w", method, path, err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		c.setAuth(req)

		resp, err := httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%s %s: %w", method, path, ctx.Err())
			}
			if attempt >= retries {
				return fmt.Errorf("%s %s: %w", method, path, err)
			}
			if !sleepContext(ctx, c.backoff(attempt)) {
				return fmt.Errorf("%s %s: %w", method, path, ctx.Err())
			}
			continue
		}

		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		closeErr := resp.Body.Close()
		if readErr != nil {
			if attempt >= retries {
				return fmt.Errorf("read %s %s response: %w", method, path, readErr)
			}
			if !sleepContext(ctx, c.backoff(attempt)) {
				return fmt.Errorf("%s %s: %w", method, path, ctx.Err())
			}
			continue
		}
		if closeErr != nil {
			// A close error after a fully read body is not actionable.
			_ = closeErr
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil || len(bytes.TrimSpace(respBody)) == 0 {
				return nil
			}
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("decode %s %s response: %w", method, path, err)
			}
			return nil
		}

		detail := responseDetail(respBody)
		if c.apiKey != "" {
			detail = strings.ReplaceAll(detail, c.apiKey, "<redacted>")
		}
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
			Detail:     detail,
		}
		if isRetryableStatus(resp.StatusCode) && attempt < retries {
			wait := retryAfter(resp, c.backoff(attempt))
			if !sleepContext(ctx, wait) {
				return fmt.Errorf("%s %s: %w", method, path, ctx.Err())
			}
			continue
		}
		return apiErr
	}
}

func (c *Client) setAuth(req *http.Request) {
	// PasarGuard API keys are accepted through X-Api-Key. A JWT pasted into
	// APIKey is sent as a Bearer token instead.
	if strings.HasPrefix(c.apiKey, "pg_key_") {
		req.Header.Set("X-Api-Key", c.apiKey)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
}

func (c *Client) backoff(attempt int) time.Duration {
	d := c.retryBase
	for i := 0; i < attempt; i++ {
		d *= 2
		if d > 5*time.Second {
			return 5 * time.Second
		}
	}
	return d
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryAfter(resp *http.Response, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		return fallback
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		d := time.Duration(seconds) * time.Second
		if d < 0 {
			return fallback
		}
		if d > 5*time.Second {
			return 5 * time.Second
		}
		return d
	}
	if when, err := http.ParseTime(value); err == nil {
		d := time.Until(when)
		if d < 0 {
			return 0
		}
		if d > 5*time.Second {
			return 5 * time.Second
		}
		return d
	}
	return fallback
}

func responseDetail(body []byte) string {
	var wrapper struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && len(wrapper.Detail) > 0 {
		var text string
		if err := json.Unmarshal(wrapper.Detail, &text); err == nil {
			return truncate(text, 400)
		}
		return truncate(string(wrapper.Detail), 400)
	}
	return truncate(strings.TrimSpace(string(body)), 400)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
