package httpapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/auth"
)

type listedToken struct {
	ID        string  `json:"id"`
	UserID    string  `json:"userId"`
	RevokedAt *string `json:"revokedAt"`
}

func decodeTokenList(t *testing.T, payload []byte) []listedToken {
	t.Helper()
	var body struct {
		Items []listedToken `json:"items"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode token list: %v; body = %s", err, payload)
	}
	return body.Items
}

func tokenOwners(items []listedToken) []string {
	owners := make([]string, 0, len(items))
	for _, item := range items {
		if !slices.Contains(owners, item.UserID) {
			owners = append(owners, item.UserID)
		}
	}
	slices.Sort(owners)
	return owners
}

func TestListTokensAuthz(t *testing.T) {
	f := newACLFixture(t)
	adminID := decodeUserID(t, f.do(t, http.MethodGet, "/v1/auth/me", nil, f.admin, http.StatusOK))
	mintToken(t, f, f.admin, map[string]any{}, http.StatusCreated)
	every := []string{adminID, f.users["alice"].ID, f.users["bob"].ID, f.users["carol"].ID, f.users["vera"].ID}
	slices.Sort(every)

	tests := []struct {
		name   string
		caller string
		query  string
		want   int
		owners []string
	}{
		{"admin all", f.admin, "?all=true", http.StatusOK, every},
		{"admin default lists own", f.admin, "", http.StatusOK, []string{adminID}},
		{"admin by user", f.admin, "?userId=" + f.users["alice"].ID, http.StatusOK, []string{f.users["alice"].ID}},
		{"member all is ignored", f.tokens["bob"], "?all=true", http.StatusOK, []string{f.users["bob"].ID}},
		{"viewer all is ignored", f.tokens["vera"], "?all=true", http.StatusOK, []string{f.users["vera"].ID}},
		{"member other user", f.tokens["bob"], "?userId=" + f.users["alice"].ID, http.StatusForbidden, nil},
		{"viewer other user", f.tokens["vera"], "?userId=" + f.users["bob"].ID, http.StatusForbidden, nil},
		{"anonymous", "", "", http.StatusUnauthorized, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := f.do(t, http.MethodGet, "/v1/tokens"+tc.query, nil, tc.caller, tc.want)
			if tc.want != http.StatusOK {
				return
			}
			if got := tokenOwners(decodeTokenList(t, payload)); !slices.Equal(got, tc.owners) {
				t.Fatalf("owners = %v, want %v", got, tc.owners)
			}
		})
	}
}

func TestListTokensHidesSecretsAndShowsRevoked(t *testing.T) {
	f := newACLFixture(t)
	raw, created := mintToken(t, f, f.tokens["bob"], map[string]any{"scopes": []string{"read"}}, http.StatusCreated)
	var minted createTokenResponse
	if err := json.Unmarshal(created, &minted); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	f.do(t, http.MethodDelete, "/v1/tokens/"+minted.APIToken.ID, nil, f.tokens["bob"], http.StatusNoContent)

	for _, caller := range []string{f.tokens["bob"], f.admin} {
		payload := f.do(t, http.MethodGet, "/v1/tokens?all=true", nil, caller, http.StatusOK)
		body := string(payload)
		for _, secret := range []string{raw, auth.HashToken(raw), f.tokens["bob"], auth.HashToken(f.tokens["bob"])} {
			if strings.Contains(body, secret) {
				t.Fatalf("token list leaks a secret or hash: %s", body)
			}
		}
		for _, field := range []string{`"token"`, `"tokenHash"`, `"token_hash"`, `"hash"`} {
			if strings.Contains(body, field) {
				t.Fatalf("token list has field %s: %s", field, body)
			}
		}
		var revoked *listedToken
		for _, item := range decodeTokenList(t, payload) {
			if item.ID == minted.APIToken.ID {
				revoked = &item
			}
		}
		if revoked == nil || revoked.RevokedAt == nil {
			t.Fatalf("revoked token missing or without revokedAt: %s", body)
		}
	}
	// A revoked token no longer authenticates.
	f.do(t, http.MethodGet, "/v1/tokens", nil, raw, http.StatusUnauthorized)
}

func TestRevokeTokenAuthz(t *testing.T) {
	f := newACLFixture(t)
	_, created := mintToken(t, f, f.tokens["alice"], map[string]any{}, http.StatusCreated)
	var minted createTokenResponse
	if err := json.Unmarshal(created, &minted); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	path := "/v1/tokens/" + minted.APIToken.ID
	f.do(t, http.MethodDelete, path, nil, f.tokens["bob"], http.StatusForbidden)
	f.do(t, http.MethodDelete, path, nil, f.tokens["vera"], http.StatusForbidden)
	f.do(t, http.MethodDelete, path, nil, f.admin, http.StatusNoContent)
	f.do(t, http.MethodDelete, path, nil, f.admin, http.StatusNoContent)
	f.do(t, http.MethodDelete, "/v1/tokens/token_missing", nil, f.admin, http.StatusNotFound)
}

func decodeUserID(t *testing.T, payload []byte) string {
	t.Helper()
	var body struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.User.ID == "" {
		t.Fatalf("decode user id: %v; body = %s", err, payload)
	}
	return body.User.ID
}
