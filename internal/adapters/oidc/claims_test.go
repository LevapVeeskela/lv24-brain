package oidc

import "testing"

func TestClaimsFromMapUsesOnlyVerifiedEmailAsLogin(t *testing.T) {
	cases := []struct {
		name         string
		raw          map[string]any
		wantLogin    string
		wantVerified bool
	}{
		{"verified bool", map[string]any{"sub": "s1", "email": "a@example.test", "email_verified": true, "preferred_username": "alice"}, "a@example.test", true},
		{"verified string", map[string]any{"sub": "s1", "email": "a@example.test", "email_verified": "true"}, "a@example.test", true},
		{"unverified falls back to username", map[string]any{"sub": "s1", "email": "a@example.test", "email_verified": false, "preferred_username": "alice"}, "alice", false},
		{"missing verified falls back to subject", map[string]any{"sub": "s1", "email": "a@example.test"}, "s1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := ClaimsFromMap("https://idp.example", tc.raw, "groups")
			if claims.Login != tc.wantLogin || claims.EmailVerified != tc.wantVerified {
				t.Fatalf("claims = %+v, want login %q verified %v", claims, tc.wantLogin, tc.wantVerified)
			}
			if claims.Email != "a@example.test" || claims.Subject != "s1" || claims.Issuer != "https://idp.example" {
				t.Fatalf("claims = %+v", claims)
			}
		})
	}
}

func TestValidateIssuerURL(t *testing.T) {
	cases := map[string]bool{
		"https://idp.example":          true,
		"http://127.0.0.1:8081":        true,
		"http://localhost:8081/realms": true,
		"http://[::1]:9000":            true,
		"http://idp.example":           false,
		"ftp://idp.example":            false,
		"idp.example":                  false,
		"":                             false,
	}
	for raw, ok := range cases {
		if err := ValidateIssuerURL(raw); (err == nil) != ok {
			t.Errorf("ValidateIssuerURL(%q) = %v, want ok=%v", raw, err, ok)
		}
	}
}
