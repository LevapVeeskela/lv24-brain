package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestListAPITokens(t *testing.T) {
	tests := []struct {
		name      string
		all       bool
		wantQuery string
		body      string
		wantIDs   []string
	}{
		{"all", true, "all=true", `{"items":[{"id":"token_1","userId":"u1","revokedAt":"2026-01-02T00:00:00Z"},{"id":"token_2","userId":"u2"}]}`, []string{"token_1", "token_2"}},
		{"own", false, "", `{"items":[{"id":"token_3","userId":"u1"}]}`, []string{"token_3"}},
		{"empty", true, "all=true", `{"items":null}`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string
			client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/tokens" {
					http.NotFound(w, r)
					return
				}
				gotQuery = r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			tokens, err := client.ListAPITokens(context.Background(), tc.all)
			if err != nil {
				t.Fatalf("ListAPITokens: %v", err)
			}
			if gotQuery != tc.wantQuery {
				t.Fatalf("query = %q, want %q", gotQuery, tc.wantQuery)
			}
			if tokens == nil {
				t.Fatal("tokens is nil, want an empty slice")
			}
			if len(tokens) != len(tc.wantIDs) {
				t.Fatalf("tokens = %+v, want ids %v", tokens, tc.wantIDs)
			}
			for i, id := range tc.wantIDs {
				if tokens[i].ID != id {
					t.Fatalf("token %d id = %q, want %q", i, tokens[i].ID, id)
				}
			}
			if tc.name == "all" && (tokens[0].RevokedAt == nil || tokens[1].RevokedAt != nil) {
				t.Fatalf("revokedAt not decoded: %+v", tokens)
			}
		})
	}
}

func TestListAPITokensForbidden(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"no"}}`)
	}))
	if _, err := client.ListAPITokens(context.Background(), false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func TestRevokeAPIToken(t *testing.T) {
	var gotPath string
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		gotPath = r.URL.EscapedPath()
		if gotPath == "/v1/tokens/token_missing" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"not found"}}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := client.RevokeAPIToken(context.Background(), " token_1 "); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if gotPath != "/v1/tokens/token_1" {
		t.Fatalf("path = %q", gotPath)
	}
	if err := client.RevokeAPIToken(context.Background(), "a/b"); err != nil {
		t.Fatalf("RevokeAPIToken escaped: %v", err)
	}
	if gotPath != "/v1/tokens/a%2Fb" {
		t.Fatalf("path = %q, want an escaped id", gotPath)
	}
	if err := client.RevokeAPIToken(context.Background(), "token_missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := client.RevokeAPIToken(context.Background(), "  "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want a validation error", err)
	}
}

func TestCreateAPITokenSendsExpiresAt(t *testing.T) {
	var got map[string]any
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"brain_raw","apiToken":{"id":"token_9","expiresAt":"2027-01-01T00:00:00Z"}}`)
	}))
	at := "2027-01-01T00:00:00Z"
	raw, record, err := client.CreateAPIToken(context.Background(), APITokenRequest{Name: "ci", ExpiresAt: &at})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if raw != "brain_raw" || record.ID != "token_9" || record.ExpiresAt == nil {
		t.Fatalf("raw=%q record=%+v", raw, record)
	}
	if got["expiresAt"] != at {
		t.Fatalf("request = %v, want expiresAt", got)
	}
	if _, ok := got["expiresInDays"]; ok {
		t.Fatalf("request = %v, want no expiresInDays", got)
	}
}
