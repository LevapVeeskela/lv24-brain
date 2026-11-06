package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientIPHonoursOnlyTrustedProxies(t *testing.T) {
	trusted, err := ParseTrustedProxies("127.0.0.1, 10.0.0.0/8")
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	cases := []struct {
		name    string
		remote  string
		xff     []string
		trusted bool
		want    string
	}{
		{"no trusted proxies ignores header", "127.0.0.1:5000", []string{"203.0.113.9"}, false, "127.0.0.1"},
		{"untrusted peer ignores header", "198.51.100.7:5000", []string{"203.0.113.9"}, true, "198.51.100.7"},
		{"trusted peer uses header", "127.0.0.1:5000", []string{"203.0.113.9"}, true, "203.0.113.9"},
		{"rightmost untrusted hop wins over spoofed left", "127.0.0.1:5000", []string{"1.1.1.1, 203.0.113.9, 10.1.2.3"}, true, "203.0.113.9"},
		{"multiple header lines", "127.0.0.1:5000", []string{"1.1.1.1", "203.0.113.9"}, true, "203.0.113.9"},
		{"malformed hop falls back to peer", "127.0.0.1:5000", []string{"not-an-ip"}, true, "127.0.0.1"},
		{"all hops trusted falls back to peer", "127.0.0.1:5000", []string{"10.0.0.1"}, true, "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
			r.RemoteAddr = tc.remote
			for _, value := range tc.xff {
				r.Header.Add("X-Forwarded-For", value)
			}
			list := trusted
			if !tc.trusted {
				list = nil
			}
			if got := clientIP(r, list); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseTrustedProxiesRejectsGarbage(t *testing.T) {
	if _, err := ParseTrustedProxies("10.0.0.0/33"); err == nil {
		t.Fatal("invalid prefix accepted")
	}
	if _, err := ParseTrustedProxies("proxy.local"); err == nil {
		t.Fatal("hostname accepted")
	}
	if got, err := ParseTrustedProxies(" , "); err != nil || len(got) != 0 {
		t.Fatalf("empty list = %v, %v", got, err)
	}
}

func TestLoginIsLimitedPerAccountAcrossClients(t *testing.T) {
	ts, _, _ := newTestServer(t, func(cfg *Config) {
		cfg.LoginRateLimit = 2
		parsed, err := ParseTrustedProxies("127.0.0.1")
		if err != nil {
			t.Fatalf("ParseTrustedProxies: %v", err)
		}
		cfg.TrustedProxies = parsed
	})
	attempt := func(clientAddr string) int {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/auth/login",
			stringsReader(`{"login":"Victim","password":"wrong-password"}`))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", clientAddr)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	// Each attempt comes from a different client, so only the per-account
	// budget can stop the third one.
	for i, addr := range []string{"203.0.113.1", "203.0.113.2"} {
		if status := attempt(addr); status != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, status)
		}
	}
	if status := attempt("203.0.113.3"); status != http.StatusTooManyRequests {
		t.Fatalf("third attempt status = %d, want 429", status)
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }
