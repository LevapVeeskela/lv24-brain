package httpapi

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSearchHandlerFiltersBySince(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	seedSearchKinds(t, store)
	token := loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken

	past := url.QueryEscape(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	future := url.QueryEscape(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"no since", "q=rollout", 3},
		{"since in the past", "q=rollout&since=" + past, 3},
		{"since in the future", "q=rollout&since=" + future, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/search?"+tc.query, nil, token)
			requireStatus(t, resp, payload, http.StatusOK)
			if got := searchKindsOf(t, payload); len(got) != tc.want {
				t.Fatalf("kinds = %v, want %d hits", got, tc.want)
			}
		})
	}
}

func TestSearchHandlerRejectsMalformedSince(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	token := loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken

	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/search?q=rollout&since=7d", nil, token)
	requireStatus(t, resp, payload, http.StatusBadRequest)
	if got := string(payload); !strings.Contains(got, "invalid_argument") || !strings.Contains(got, "RFC3339") {
		t.Fatalf("error body = %s, want invalid_argument about RFC3339", got)
	}
}
