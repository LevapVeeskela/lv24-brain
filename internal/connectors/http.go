package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxBodyBytes  = int64(8 << 20)
	defaultRetryAttempts = 3
	defaultRetryBase     = 500 * time.Millisecond
	defaultRetryMax      = 8 * time.Second
	defaultRetryAfterMax = 60 * time.Second
)

// RetryPolicy controls exponential backoff and rate-limit waits.
type RetryPolicy struct {
	MaxAttempts   int
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	MaxRetryAfter time.Duration
}

// Normalize fills defaults and bounds invalid values.
func (p RetryPolicy) Normalize() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = defaultRetryAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = defaultRetryBase
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = defaultRetryMax
	}
	if p.MaxDelay < p.BaseDelay {
		p.MaxDelay = p.BaseDelay
	}
	if p.MaxRetryAfter <= 0 {
		p.MaxRetryAfter = defaultRetryAfterMax
	}
	return p
}

// HTTPOptions configures the retrying read-only HTTP client.
type HTTPOptions struct {
	Client       *http.Client
	Retry        RetryPolicy
	Sleep        func(context.Context, time.Duration) error
	Now          func() time.Time
	MaxBodyBytes int64
}

// HTTP performs GET requests with retries for transient errors and rate
// limits. It never sends a non-GET request.
type HTTP struct {
	client       *http.Client
	retry        RetryPolicy
	sleep        func(context.Context, time.Duration) error
	now          func() time.Time
	maxBodyBytes int64
}

// NewHTTP builds an HTTP helper. A nil client uses a 30 second timeout.
func NewHTTP(opts HTTPOptions) *HTTP {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if client.CheckRedirect == nil {
		// Never follow redirects with a connector token attached.
		cloned := *client
		cloned.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &cloned
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}
	return &HTTP{
		client:       client,
		retry:        opts.Retry.Normalize(),
		sleep:        sleep,
		now:          now,
		maxBodyBytes: maxBody,
	}
}

// HTTPStatusError is a non-2xx response. It never contains request headers.
type HTTPStatusError struct {
	StatusCode  int
	Status      string
	Body        string
	RateLimited bool
	RetryAfter  time.Duration
}

func (e *HTTPStatusError) Error() string {
	status := strings.TrimSpace(e.Status)
	if status == "" {
		status = http.StatusText(e.StatusCode)
	}
	message := fmt.Sprintf("http %d %s", e.StatusCode, status)
	if e.RateLimited {
		message += " (rate limited)"
	}
	// Response bodies are deliberately not rendered: an upstream may echo
	// credentials or private data. Callers may inspect Body explicitly.
	return message
}

// GetJSON performs a read-only GET and decodes a JSON response into out.
func (h *HTTP) GetJSON(ctx context.Context, rawURL string, header http.Header, out any) error {
	_, err := h.GetJSONWithHeaders(ctx, rawURL, header, out)
	return err
}

// GetJSONWithHeaders behaves like GetJSON and also returns the response headers
// of the successful attempt (nil on error).
func (h *HTTP) GetJSONWithHeaders(ctx context.Context, rawURL string, header http.Header, out any) (http.Header, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse request url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported request scheme %q", parsed.Scheme)
	}

	var lastErr error
	for attempt := 1; attempt <= h.retry.MaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header = header.Clone()
		if req.Header == nil {
			req.Header = http.Header{}
		}

		resp, err := h.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if attempt == h.retry.MaxAttempts {
				return nil, fmt.Errorf("request failed: %w", err)
			}
			if err := h.sleep(ctx, h.backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}

		body, readErr := readLimited(resp.Body, h.maxBodyBytes)
		closeErr := resp.Body.Close()
		if readErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = readErr
			if attempt == h.retry.MaxAttempts {
				return nil, fmt.Errorf("read response: %w", readErr)
			}
			if err := h.sleep(ctx, h.backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		if closeErr != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil || len(body) == 0 {
				return resp.Header.Clone(), nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return nil, fmt.Errorf("decode response: %w", err)
			}
			return resp.Header.Clone(), nil
		}

		statusErr := &HTTPStatusError{
			StatusCode:  resp.StatusCode,
			Status:      resp.Status,
			Body:        SanitizeText(string(body), 300),
			RateLimited: isRateLimited(resp),
		}
		if retryableStatus(resp.StatusCode, statusErr.RateLimited) && attempt < h.retry.MaxAttempts {
			wait, ok := h.retryWait(resp)
			if !ok {
				if hasRetryHint(resp) {
					return nil, statusErr
				}
				wait = h.backoff(attempt)
			}
			if err := h.sleep(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}
		return nil, statusErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("request failed")
}

func (h *HTTP) backoff(attempt int) time.Duration {
	delay := h.retry.BaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= h.retry.MaxDelay {
			return h.retry.MaxDelay
		}
	}
	return delay
}

// retryWait returns the wait requested by Retry-After or rate-limit reset
// headers. The second result is false when the server asked for a wait longer
// than MaxRetryAfter (fail instead of sleeping without bound).
func (h *HTTP) retryWait(resp *http.Response) (time.Duration, bool) {
	if value := strings.TrimSpace(resp.Header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			return h.capWait(time.Duration(seconds) * time.Second)
		}
		if when, err := http.ParseTime(value); err == nil {
			return h.capWait(when.Sub(h.now().UTC()))
		}
	}
	for _, name := range []string{"X-RateLimit-Reset", "RateLimit-Reset"} {
		value := strings.TrimSpace(resp.Header.Get(name))
		if value == "" {
			continue
		}
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			continue
		}
		return h.capWait(time.Unix(seconds, 0).Sub(h.now().UTC()))
	}
	if isRateLimited(resp) {
		return h.capWait(h.retry.BaseDelay)
	}
	return 0, false
}

func (h *HTTP) capWait(wait time.Duration) (time.Duration, bool) {
	if wait < 0 {
		wait = 0
	}
	if wait > h.retry.MaxRetryAfter {
		return 0, false
	}
	return wait, true
}

func hasRetryHint(resp *http.Response) bool {
	if strings.TrimSpace(resp.Header.Get("Retry-After")) != "" {
		return true
	}
	for _, name := range []string{"X-RateLimit-Reset", "RateLimit-Reset"} {
		if strings.TrimSpace(resp.Header.Get(name)) != "" {
			return true
		}
	}
	return false
}

func isRateLimited(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	if strings.TrimSpace(resp.Header.Get("Retry-After")) != "" {
		return true
	}
	for _, name := range []string{"X-RateLimit-Remaining", "RateLimit-Remaining"} {
		if strings.TrimSpace(resp.Header.Get(name)) == "0" {
			return true
		}
	}
	return false
}

func retryableStatus(status int, rateLimited bool) bool {
	if rateLimited {
		return true
	}
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		return io.ReadAll(r)
	}
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return body, nil
}

func sleepContext(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
