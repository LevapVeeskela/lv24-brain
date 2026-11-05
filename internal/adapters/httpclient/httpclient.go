// Package httpclient implements ports.Store against a lv24-brain server.
//
// The client is used by the CLI in server mode. It sends the configured bearer
// token with every request, maps the server error envelope onto typed errors
// (400/404/409 unwrap to the matching domain sentinels), enforces a bounded
// response size, and redacts the token from any error text it returns.
package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	// DefaultTimeout bounds one HTTP round trip when Config.Timeout is zero.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxResponseBytes bounds a successful JSON response body.
	DefaultMaxResponseBytes = 4 << 20
	// maxErrorBytes bounds the message copied out of an error response.
	maxErrorBytes = 512
)

var (
	// ErrUnauthorized reports a missing or rejected bearer token (HTTP 401).
	ErrUnauthorized = errors.New("httpclient: authentication required")
	// ErrForbidden reports insufficient privilege (HTTP 403).
	ErrForbidden = errors.New("httpclient: access denied")
	// ErrTimeout reports a request that exceeded the client timeout.
	ErrTimeout = errors.New("httpclient: request timed out")

	bearerPattern = regexp.MustCompile(`(?i)bearer\s+\S+`)
	spacePattern  = regexp.MustCompile(`\s+`)
)

// TokenProvider resolves a bearer token lazily, for example after a refresh.
type TokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// Config configures a Client.
type Config struct {
	// BaseURL is the server root, for example https://brain.example.test.
	// A path prefix is allowed; credentials in the URL are rejected.
	BaseURL string
	// Token is the static bearer token. Ignored when TokenProvider is set.
	Token string
	// TokenProvider optionally supplies the token per request.
	TokenProvider TokenProvider
	// Refresh obtains a new access token after a 401. When nil, the client
	// returns the 401 unchanged. The refresher must persist the rotated token
	// so the next request resolves it through TokenProvider.
	Refresh func(ctx context.Context) (string, error)
	// HTTPClient overrides the default client. The caller then owns timeouts.
	HTTPClient *http.Client
	// Timeout bounds one request when HTTPClient is nil.
	Timeout time.Duration
	// MaxResponseBytes bounds a successful response body.
	MaxResponseBytes int64
}

// APIError reports a non-2xx response that has no more specific mapping.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("httpclient: server returned status %d", e.StatusCode)
	}
	return fmt.Sprintf("httpclient: server returned status %d: %s", e.StatusCode, e.Message)
}

// ConflictError reports an HTTP 409. It unwraps to domain.ErrConflict.
type ConflictError struct {
	Code            string
	Message         string
	CurrentRevision int64
}

func (e *ConflictError) Error() string {
	message := e.Message
	if message == "" {
		message = "revision conflict"
	}
	if e.CurrentRevision > 0 {
		return fmt.Sprintf("httpclient: conflict (current revision %d): %s", e.CurrentRevision, message)
	}
	return fmt.Sprintf("httpclient: conflict: %s", message)
}

// Unwrap makes errors.Is(err, domain.ErrConflict) true.
func (e *ConflictError) Unwrap() error { return domain.ErrConflict }

// Client talks to the brain HTTP API.
type Client struct {
	baseURL    string
	token      string
	provider   TokenProvider
	refresher  func(context.Context) (string, error)
	refreshMu  sync.Mutex
	httpClient *http.Client
	maxBody    int64
}

// New validates cfg and returns a client.
func New(cfg Config) (*Client, error) {
	baseURL, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxBody := cfg.MaxResponseBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxResponseBytes
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	return &Client{
		baseURL:    baseURL,
		token:      strings.TrimSpace(cfg.Token),
		provider:   cfg.TokenProvider,
		refresher:  cfg.Refresh,
		httpClient: httpClient,
		maxBody:    maxBody,
	}, nil
}

// SetTokenProvider replaces the per-request token source. Bootstrap uses it
// after the auth helper is constructed to keep the client's token in sync with
// the OS secret store.
func (c *Client) SetTokenProvider(provider TokenProvider) {
	if c != nil {
		c.provider = provider
	}
}

// SetRefresher replaces the 401 recovery hook.
func (c *Client) SetRefresher(refresh func(context.Context) (string, error)) {
	if c != nil {
		c.refresher = refresh
	}
}

// BaseURL returns the normalized server root.
func (c *Client) BaseURL() string { return c.baseURL }

func normalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", domain.NewValidationError("httpclient.base_url", "is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", domain.NewValidationError("httpclient.base_url", "must be an absolute http(s) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", domain.NewValidationError("httpclient.base_url", "must be an absolute http(s) URL")
	}
	if parsed.Host == "" {
		return "", domain.NewValidationError("httpclient.base_url", "must include a host")
	}
	if parsed.User != nil {
		return "", domain.NewValidationError("httpclient.base_url", "must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", domain.NewValidationError("httpclient.base_url", "must not contain a query or fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// do executes one authenticated request and decodes a successful JSON
// response into out. A 401 triggers one token refresh and retry when a
// refresher is configured.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.execute(ctx, method, path, query, body, out, "", false, true)
}

// doAnonymous executes a request without a bearer token. It is used for the
// login and refresh endpoints, which cannot authenticate with an expired
// access token.
func (c *Client) doAnonymous(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.execute(ctx, method, path, query, body, out, "", true, false)
}

// doWithToken executes a request with an explicit bearer token and never
// refreshes it.
func (c *Client) doWithToken(ctx context.Context, method, path string, query url.Values, body, out any, token string) error {
	return c.execute(ctx, method, path, query, body, out, strings.TrimSpace(token), false, false)
}

func (c *Client) execute(ctx context.Context, method, path string, query url.Values, body, out any, tokenOverride string, anonymous, allowRefresh bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("httpclient: encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("httpclient: build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	token := ""
	if !anonymous {
		if tokenOverride != "" {
			token = tokenOverride
		} else {
			token, err = c.bearerToken(ctx)
			if err != nil {
				return err
			}
		}
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return c.transportError(ctx, method, path, err, token)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes+1))
		var failure error
		if readErr != nil {
			failure = &APIError{StatusCode: response.StatusCode}
		} else {
			failure = c.responseError(method, path, response.StatusCode, payload, token)
		}
		if response.StatusCode == http.StatusUnauthorized && allowRefresh && !anonymous && c.refresher != nil {
			_ = response.Body.Close()
			fresh, refreshErr := c.refreshAccessToken(ctx, token)
			switch {
			case refreshErr == nil && fresh != "":
				return c.execute(ctx, method, path, query, body, out, fresh, false, false)
			case refreshErr != nil && !errors.Is(refreshErr, ErrUnauthorized) && !errors.Is(refreshErr, errNoRefreshToken):
				// Surface transient refresh failures (5xx, transport) instead
				// of pretending the original 401 was the whole story. A
				// credential 401 or a missing refresh token keeps the
				// original 401.
				return fmt.Errorf("httpclient: %s %s: refresh after 401 failed: %w", method, path, refreshErr)
			}
		}
		return failure
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.maxBody))
		return nil
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, c.maxBody+1))
	if err != nil {
		return c.transportError(ctx, method, path, err, token)
	}
	if int64(len(payload)) > c.maxBody {
		return fmt.Errorf("httpclient: %s %s: response exceeds %d bytes", method, path, c.maxBody)
	}
	if len(payload) == 0 {
		return fmt.Errorf("httpclient: %s %s: empty response body", method, path)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("httpclient: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// refreshAccessToken serializes refreshes and reuses a token another goroutine
// already rotated, so concurrent 401s do not burn two refresh tokens.
func (c *Client) refreshAccessToken(ctx context.Context, used string) (string, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.refresher == nil {
		return "", errors.New("httpclient: no token refresher configured")
	}
	if current, err := c.bearerToken(ctx); err == nil && current != "" && current != used {
		return current, nil
	}
	token, err := c.refresher(ctx)
	if err != nil {
		return "", fmt.Errorf("httpclient: refresh access token: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("httpclient: refresh returned an empty token")
	}
	return token, nil
}

func (c *Client) bearerToken(ctx context.Context) (string, error) {
	if c.provider != nil {
		token, err := c.provider.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("httpclient: resolve token: %w", err)
		}
		return strings.TrimSpace(token), nil
	}
	return c.token, nil
}

func (c *Client) transportError(ctx context.Context, method, path string, err error, requestToken string) error {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("httpclient: %s %s: %w", method, path, ErrTimeout)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("httpclient: %s %s: %w", method, path, ctxErr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("httpclient: %s %s: %w", method, path, ErrTimeout)
	}
	return fmt.Errorf("httpclient: %s %s: %s", method, path, c.redact(err.Error(), requestToken))
}

func (c *Client) responseError(method, path string, status int, payload []byte, requestToken string) error {
	var envelope struct {
		Error struct {
			Code            string `json:"code"`
			Message         string `json:"message"`
			CurrentRevision *int64 `json:"currentRevision"`
		} `json:"error"`
	}
	_ = json.Unmarshal(payload, &envelope)
	code := strings.TrimSpace(envelope.Error.Code)
	message := c.redact(envelope.Error.Message, requestToken)
	if message == "" {
		message = strings.TrimSpace(string(payload))
		message = c.redact(message, requestToken)
		message = strings.Join(strings.Fields(message), " ")
		if len(message) > maxErrorBytes {
			message = message[:maxErrorBytes]
		}
	}

	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("httpclient: %s %s: %w", method, path, ErrUnauthorized)
	case http.StatusForbidden:
		return fmt.Errorf("httpclient: %s %s: %w", method, path, ErrForbidden)
	case http.StatusNotFound:
		return fmt.Errorf("httpclient: %s %s: %w", method, path, domain.ErrNotFound)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if message == "" {
			return fmt.Errorf("httpclient: %s %s: %w", method, path, domain.ErrValidation)
		}
		return fmt.Errorf("httpclient: %s %s: %w: %s", method, path, domain.ErrValidation, message)
	case http.StatusConflict:
		current := int64(0)
		if envelope.Error.CurrentRevision != nil {
			current = *envelope.Error.CurrentRevision
		}
		return &ConflictError{Code: code, Message: message, CurrentRevision: current}
	default:
		return &APIError{StatusCode: status, Code: code, Message: message}
	}
}

// redact removes the configured bearer token, the token sent with this
// request, and any Bearer credential from text that may reach a terminal or
// log. The request token matters when a proxy echoes it back in an error body.
func (c *Client) redact(text string, requestTokens ...string) string {
	text = spacePattern.ReplaceAllString(text, " ")
	text = strings.TrimSpace(text)
	if c.token != "" {
		text = strings.ReplaceAll(text, c.token, "***")
	}
	for _, token := range requestTokens {
		if token != "" && token != c.token {
			text = strings.ReplaceAll(text, token, "***")
		}
	}
	text = bearerPattern.ReplaceAllString(text, "Bearer ***")
	if len(text) > maxErrorBytes {
		text = text[:maxErrorBytes]
	}
	return text
}
