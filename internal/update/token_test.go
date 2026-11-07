package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestUpdaterSendsTokenOnlyToTheAPI(t *testing.T) {
	var apiAuth, assetAuth atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/asset" {
			assetAuth.Store(r.Header.Get("Authorization"))
		} else {
			apiAuth.Store(r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	sourceCalls := 0
	updater := testUpdater(t, server.URL+"/api", func(opts *Options) {
		opts.Getenv = func(name string) string {
			if name == "GITHUB_TOKEN" {
				return "env-token"
			}
			return ""
		}
		opts.TokenSource = func(context.Context) (string, error) { sourceCalls++; return "source-token", nil }
	})

	ctx := context.Background()
	if _, err := updater.getBytes(ctx, server.URL+"/api/repos/x/releases", 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := updater.getBytes(ctx, server.URL+"/asset", 1024); err != nil {
		t.Fatal(err)
	}
	if apiAuth.Load() != "Bearer env-token" || assetAuth.Load() != "" {
		t.Fatalf("api auth %q, asset auth %q", apiAuth.Load(), assetAuth.Load())
	}
	if sourceCalls != 0 {
		t.Fatalf("token source called %d times although the environment had a token", sourceCalls)
	}
}

func TestUpdaterTokenSourceIsLazyAndRejectedTokenIsDropped(t *testing.T) {
	var requests, authorized atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			authorized.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	sourceCalls := 0
	updater := testUpdater(t, server.URL, func(opts *Options) {
		opts.TokenSource = func(context.Context) (string, error) { sourceCalls++; return "stale-token", nil }
	})
	if sourceCalls != 0 {
		t.Fatal("token source called before any request")
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := updater.getBytes(ctx, server.URL+"/repos/x/releases", 1024); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if sourceCalls != 1 || authorized.Load() != 1 || requests.Load() != 3 {
		t.Fatalf("source calls %d, authorized %d, requests %d", sourceCalls, authorized.Load(), requests.Load())
	}
}
