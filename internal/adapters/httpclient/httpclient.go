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
		httpClient: httpClient,
		maxBody:    maxBody,
	}, nil
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

// do executes one request and decodes a successful JSON response into out.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
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
	token, err := c.bearerToken(ctx)
	if err != nil {
		return err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return c.transportError(ctx, method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes+1))
		if readErr != nil {
			return &APIError{StatusCode: response.StatusCode}
		}
		return c.responseError(method, path, response.StatusCode, payload)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.maxBody))
		return nil
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, c.maxBody+1))
	if err != nil {
		return c.transportError(ctx, method, path, err)
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

func (c *Client) transportError(ctx context.Context, method, path string, err error) error {
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
	return fmt.Errorf("httpclient: %s %s: %s", method, path, c.redact(err.Error()))
}

func (c *Client) responseError(method, path string, status int, payload []byte) error {
	var envelope struct {
		Error struct {
			Code            string `json:"code"`
			Message         string `json:"message"`
			CurrentRevision *int64 `json:"currentRevision"`
		} `json:"error"`
	}
	_ = json.Unmarshal(payload, &envelope)
	code := strings.TrimSpace(envelope.Error.Code)
	message := c.redact(envelope.Error.Message)
	if message == "" {
		message = strings.TrimSpace(string(payload))
		message = c.redact(message)
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

// redact removes the configured bearer token and any Bearer credential from
// text that may reach a terminal or log.
func (c *Client) redact(text string) string {
	text = spacePattern.ReplaceAllString(text, " ")
	text = strings.TrimSpace(text)
	if c.token != "" {
		text = strings.ReplaceAll(text, c.token, "***")
	}
	text = bearerPattern.ReplaceAllString(text, "Bearer ***")
	if len(text) > maxErrorBytes {
		text = text[:maxErrorBytes]
	}
	return text
}
