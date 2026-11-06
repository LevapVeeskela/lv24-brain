// Package oidc adapts a generic OpenID Connect provider to the brain auth
// contract. It performs authorization-code + PKCE login, ID-token validation,
// and configurable group-to-role mapping.
package oidc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	oidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Config describes one OIDC client.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	// GroupsClaim is the ID-token claim carrying group names.
	GroupsClaim string
	// GroupRoles maps a provider group to a brain role.
	GroupRoles map[string]domain.Role
	// DefaultRole is used when no configured group matches.
	DefaultRole domain.Role
}

// Authenticator implements auth.OIDCProvider against a discovered issuer.
type Authenticator struct {
	cfg      Config
	verifier *oidc.IDTokenVerifier
	oauth2   oauth2.Config
}

var _ auth.OIDCProvider = (*Authenticator)(nil)

// New discovers the issuer and builds the OAuth2 client. Discovery failures
// are returned so the server can fail fast on bad configuration.
func New(ctx context.Context, cfg Config) (*Authenticator, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("oidc issuer is required")
	}
	if err := ValidateIssuerURL(cfg.Issuer); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("oidc client id is required")
	}
	if strings.TrimSpace(cfg.RedirectURL) == "" {
		return nil, errors.New("oidc redirect url is required")
	}
	cfg.GroupsClaim = strings.TrimSpace(cfg.GroupsClaim)
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	if cfg.DefaultRole == "" {
		cfg.DefaultRole = domain.RoleViewer
	}
	if !cfg.DefaultRole.Valid() {
		return nil, fmt.Errorf("oidc default role %q is invalid", cfg.DefaultRole)
	}
	for group, role := range cfg.GroupRoles {
		if !role.Valid() {
			return nil, fmt.Errorf("oidc role %q for group %q is invalid", role, group)
		}
	}
	scopes := append([]string(nil), cfg.Scopes...)
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}

	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discover oidc issuer: %w", err)
	}
	return &Authenticator{
		cfg:      cfg,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
		},
	}, nil
}

// AuthCodeURL builds the provider authorization URL with nonce and PKCE S256.
func (a *Authenticator) AuthCodeURL(state, nonce, codeVerifier string) string {
	return a.oauth2.AuthCodeURL(
		state,
		oauth2.AccessTypeOnline,
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.SetAuthURLParam("code_challenge", PKCEChallenge(codeVerifier)),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

// Exchange redeems the code, verifies the ID token and nonce, and maps claims
// to a principal role.
func (a *Authenticator) Exchange(ctx context.Context, code, codeVerifier, nonce string) (auth.OIDCClaims, error) {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(codeVerifier) == "" || strings.TrimSpace(nonce) == "" {
		return auth.OIDCClaims{}, errors.New("oidc code, verifier and nonce are required")
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	token, err := a.oauth2.Exchange(exchangeCtx, code, oauth2.SetAuthURLParam("code_verifier", codeVerifier))
	if err != nil {
		return auth.OIDCClaims{}, fmt.Errorf("exchange oidc code: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return auth.OIDCClaims{}, errors.New("oidc response has no id_token")
	}
	idToken, err := a.verifier.Verify(exchangeCtx, rawIDToken)
	if err != nil {
		return auth.OIDCClaims{}, fmt.Errorf("verify oidc id token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return auth.OIDCClaims{}, errors.New("oidc id token nonce mismatch")
	}
	var raw map[string]any
	if err := idToken.Claims(&raw); err != nil {
		return auth.OIDCClaims{}, fmt.Errorf("decode oidc claims: %w", err)
	}
	claims := ClaimsFromMap(idToken.Issuer, raw, a.cfg.GroupsClaim)
	if strings.TrimSpace(claims.Subject) == "" {
		return auth.OIDCClaims{}, errors.New("oidc id token has no subject")
	}
	claims.Role = MapRole(claims.Groups, a.cfg.GroupRoles, a.cfg.DefaultRole)
	return claims, nil
}

// ClaimsFromMap converts verified ID-token claims to brain claims. The email
// becomes the login only when the provider asserts email_verified; otherwise
// preferred_username, then the subject, is used. Role is left for the caller.
func ClaimsFromMap(issuer string, raw map[string]any, groupsClaim string) auth.OIDCClaims {
	text := func(key string) string {
		value, _ := raw[key].(string)
		return strings.TrimSpace(value)
	}
	subject := text("sub")
	email := text("email")
	verified := claimBool(raw["email_verified"])
	login := ""
	if email != "" && verified {
		login = email
	}
	if login == "" {
		login = text("preferred_username")
	}
	if login == "" {
		login = subject
	}
	displayName := text("name")
	if displayName == "" {
		displayName = login
	}
	return auth.OIDCClaims{
		Issuer:        issuer,
		Subject:       subject,
		Login:         login,
		Email:         email,
		EmailVerified: verified,
		DisplayName:   displayName,
		Groups:        claimStrings(raw[groupsClaim]),
	}
}

// claimBool accepts a JSON boolean or the string "true"; some providers send
// email_verified as a string.
func claimBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

// ValidateIssuerURL requires an absolute https issuer; plain http is allowed
// only for loopback hosts (local development and tests).
func ValidateIssuerURL(raw string) error {
	issuerURL, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || issuerURL.Host == "" {
		return errors.New("oidc issuer must be an absolute https URL")
	}
	switch issuerURL.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(issuerURL.Hostname()) {
			return nil
		}
		return errors.New("oidc issuer must use https (http is allowed only for loopback hosts)")
	default:
		return errors.New("oidc issuer must be an absolute https URL")
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// PKCEChallenge computes the S256 code challenge for a verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// MapRole picks the highest-privilege role assigned to any group. Unknown
// groups are ignored and an empty match uses fallback.
func MapRole(groups []string, mapping map[string]domain.Role, fallback domain.Role) domain.Role {
	best := fallback
	for _, group := range groups {
		role, ok := mapping[strings.TrimSpace(group)]
		if !ok {
			continue
		}
		if roleRank(role) > roleRank(best) {
			best = role
		}
	}
	if !best.Valid() {
		return domain.RoleViewer
	}
	return best
}

func roleRank(role domain.Role) int {
	switch role {
	case domain.RoleAdmin:
		return 3
	case domain.RoleMember:
		return 2
	case domain.RoleViewer:
		return 1
	default:
		return 0
	}
}

func claimStrings(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	default:
		return nil
	}
}
