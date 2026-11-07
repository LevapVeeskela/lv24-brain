package connectors

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetJSONRetriesServerErrors(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if attempts.Add(1) == 1 {
			http.Error(w, "temporary", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	var sleeps []time.Duration
	client := NewHTTP(HTTPOptions{
		Retry: RetryPolicy{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: 20 * time.Millisecond},
		Sleep: func(ctx context.Context, wait time.Duration) error {
			sleeps = append(sleeps, wait)
			return nil
		},
	})
	var out struct {
		OK bool `json:"ok"`
	}
	if err := client.GetJSON(context.Background(), server.URL, nil, &out); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if !out.OK || attempts.Load() != 2 || len(sleeps) != 1 {
		t.Fatalf("ok=%v attempts=%d sleeps=%v", out.OK, attempts.Load(), sleeps)
	}
}

func TestGetJSONHonorsRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	var slept bool
	client := NewHTTP(HTTPOptions{
		Retry: RetryPolicy{MaxAttempts: 2},
		Sleep: func(ctx context.Context, wait time.Duration) error {
			slept = true
			if wait != 0 {
				t.Fatalf("wait = %v, want 0", wait)
			}
			return nil
		},
	})
	if err := client.GetJSON(context.Background(), server.URL, nil, nil); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if attempts.Load() != 2 || !slept {
		t.Fatalf("attempts=%d slept=%v", attempts.Load(), slept)
	}
}

func TestGetJSONFailsWhenRateLimitExceedsCap(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "4102444800")
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer server.Close()

	client := NewHTTP(HTTPOptions{
		Retry: RetryPolicy{MaxAttempts: 3, MaxRetryAfter: time.Second},
		Sleep: func(ctx context.Context, wait time.Duration) error {
			t.Fatalf("unexpected sleep %v", wait)
			return nil
		},
	})
	err := client.GetJSON(context.Background(), server.URL, nil, nil)
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || !statusErr.RateLimited {
		t.Fatalf("err = %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
}

func TestGetJSONDoesNotRetryUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad token super-secret-token", http.StatusUnauthorized)
	}))
	defer server.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer super-secret-token")
	client := NewHTTP(HTTPOptions{})
	err := client.GetJSON(context.Background(), server.URL, header, nil)
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("error leaked header or body token: %v", err)
	}
}

func TestGetJSONRejectsNonHTTPScheme(t *testing.T) {
	client := NewHTTP(HTTPOptions{})
	if err := client.GetJSON(context.Background(), "file:///etc/passwd", nil, nil); err == nil {
		t.Fatal("expected scheme error")
	}
}

func TestGetJSONDoesNotFollowRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/somewhere-else", http.StatusFound)
	}))
	defer server.Close()

	client := NewHTTP(HTTPOptions{})
	err := client.GetJSON(context.Background(), server.URL, nil, nil)
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusFound {
		t.Fatalf("err = %v", err)
	}
}

func TestGetJSONBoundsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 128)))
	}))
	defer server.Close()

	client := NewHTTP(HTTPOptions{MaxBodyBytes: 32})
	if err := client.GetJSON(context.Background(), server.URL, nil, nil); err == nil {
		t.Fatal("expected body limit error")
	}
}

type failingTransport struct {
	attempts atomic.Int32
}

func (f *failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.attempts.Add(1) < 2 {
		return nil, errors.New("connection reset")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Request:    req,
	}, nil
}

func TestGetJSONRetriesNetworkErrors(t *testing.T) {
	transport := &failingTransport{}
	client := NewHTTP(HTTPOptions{
		Client: &http.Client{Transport: transport},
		Retry:  RetryPolicy{MaxAttempts: 2},
		Sleep:  func(ctx context.Context, wait time.Duration) error { return nil },
	})
	var out struct {
		OK bool `json:"ok"`
	}
	if err := client.GetJSON(context.Background(), "https://api.example.test/items", nil, &out); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if !out.OK || transport.attempts.Load() != 2 {
		t.Fatalf("ok=%v attempts=%d", out.OK, transport.attempts.Load())
	}
}

func TestGetJSONWithHeadersReturnsSuccessfulAttemptHeaders(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("X-Next-Page", "2")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := NewHTTP(HTTPOptions{
		Client: server.Client(),
		Retry:  RetryPolicy{MaxAttempts: 2},
		Sleep:  func(context.Context, time.Duration) error { return nil },
	})
	var out struct {
		OK bool `json:"ok"`
	}
	headers, err := client.GetJSONWithHeaders(context.Background(), server.URL, http.Header{}, &out)
	if err != nil {
		t.Fatalf("GetJSONWithHeaders: %v", err)
	}
	if got := headers.Get("X-Next-Page"); got != "2" {
		t.Fatalf("X-Next-Page = %q, want 2", got)
	}
	if !out.OK || attempts.Load() != 2 {
		t.Fatalf("out = %+v attempts = %d", out, attempts.Load())
	}
}

func TestGetJSONWithHeadersReturnsNoHeadersOnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer server.Close()

	client := NewHTTP(HTTPOptions{Client: server.Client(), Retry: RetryPolicy{MaxAttempts: 1}})
	headers, err := client.GetJSONWithHeaders(context.Background(), server.URL, http.Header{}, nil)
	if err == nil || headers != nil {
		t.Fatalf("headers = %v err = %v, want nil headers and error", headers, err)
	}
}
