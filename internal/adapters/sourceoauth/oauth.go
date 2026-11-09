// Package sourceoauth contains bounded OAuth helpers for source adapters.
// It supports native-app PKCE loopback and OAuth device authorization without
// embedding provider-specific policy in callers.
package sourceoauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const maxResponseBytes = 1 << 20

// DefaultLoopbackPort is used by NewFlow for providers registered to a fixed
// native-app callback port. Set Flow.Port to zero to request an ephemeral port.
const DefaultLoopbackPort = 47615

// TokenAuthStyle controls client authentication at a token endpoint.
type TokenAuthStyle string

const (
	TokenAuthForm  TokenAuthStyle = "form"
	TokenAuthBasic TokenAuthStyle = "basic"
)

// Provider describes OAuth endpoints and public-client behavior. URLs must be
// HTTPS except for HTTP loopback URLs, which are useful for local providers and
// deterministic tests. Slack and other providers can be supplied by callers.
type Provider struct {
	Name           string
	AuthorizeURL   string
	TokenURL       string
	DeviceURL      string
	DefaultScopes  []string
	PKCE           bool
	Device         bool
	TokenAuthStyle TokenAuthStyle
}

// Providers returns the MVP provider definitions. GitHub uses device flow;
// GitLab uses public-client PKCE. Other providers can be configured directly.
func Providers() map[string]Provider {
	return map[string]Provider{
		"github": {
			Name: "github", TokenURL: "https://github.com/login/oauth/access_token",
			DeviceURL: "https://github.com/login/device/code", DefaultScopes: []string{"repo", "read:user"},
			Device: true, TokenAuthStyle: TokenAuthForm,
		},
		"gitlab": {
			Name: "gitlab", AuthorizeURL: "https://gitlab.com/oauth/authorize",
			TokenURL: "https://gitlab.com/oauth/token", DefaultScopes: []string{"read_api", "read_user"},
			PKCE: true, TokenAuthStyle: TokenAuthForm,
		},
	}
}

// Bundle is a validated OAuth credential set. Expiry is optional; when present
// it is serialized as an RFC3339 UTC timestamp.
type Bundle struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
	Expiry       time.Time `json:"-"`
}

type bundleJSON struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	TokenType    string   `json:"token_type,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	Expiry       string   `json:"expiry,omitempty"`
}

// Validate rejects empty and oversized tokens and malformed or non-UTC expiry.
func (b Bundle) Validate() error {
	if strings.TrimSpace(b.AccessToken) == "" {
		return errors.New("sourceoauth: access token is required")
	}
	if len(b.AccessToken) > 16<<10 || len(b.RefreshToken) > 16<<10 || len(b.TokenType) > 128 {
		return errors.New("sourceoauth: token bundle exceeds size limits")
	}
	if len(b.Scopes) > 128 {
		return errors.New("sourceoauth: too many scopes")
	}
	for _, scope := range b.Scopes {
		if scope == "" || len(scope) > 256 {
			return errors.New("sourceoauth: invalid scope")
		}
	}
	if !b.Expiry.IsZero() {
		_, offset := b.Expiry.Zone()
		if offset != 0 || b.Expiry.Location() != time.UTC {
			return errors.New("sourceoauth: expiry must be UTC")
		}
		if _, err := time.Parse(time.RFC3339, b.Expiry.Format(time.RFC3339)); err != nil {
			return errors.New("sourceoauth: expiry must be RFC3339 UTC")
		}
	}
	return nil
}

func (b Bundle) MarshalJSON() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	wire := bundleJSON{AccessToken: b.AccessToken, RefreshToken: b.RefreshToken, TokenType: b.TokenType, Scopes: b.Scopes}
	if !b.Expiry.IsZero() {
		wire.Expiry = b.Expiry.UTC().Format(time.RFC3339)
	}
	return json.Marshal(wire)
}

func (b *Bundle) UnmarshalJSON(data []byte) error {
	var wire bundleJSON
	if err := json.Unmarshal(data, &wire); err != nil {
		return errors.New("sourceoauth: invalid token bundle")
	}
	var expiry time.Time
	if wire.Expiry != "" {
		parsed, err := time.Parse(time.RFC3339, wire.Expiry)
		if err != nil || !strings.HasSuffix(wire.Expiry, "Z") {
			return errors.New("sourceoauth: expiry must be RFC3339 UTC")
		}
		expiry = parsed.UTC()
	}
	*b = Bundle{AccessToken: wire.AccessToken, RefreshToken: wire.RefreshToken, TokenType: wire.TokenType, Scopes: wire.Scopes, Expiry: expiry}
	return b.Validate()
}

// DevicePrompt is the user-facing portion of a device authorization response.
// DeviceCode is deliberately not exposed to prompts or error messages.
type DevicePrompt struct {
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               time.Duration
}

// ErrReauthRequired indicates that the provider rejected or could not refresh
// the stored authorization; the caller should ask the user to authenticate.
var ErrReauthRequired = errors.New("sourceoauth: reauthorization required")

// ValidateProvider checks all configured endpoints before use.
func ValidateProvider(p Provider) error {
	if strings.TrimSpace(p.Name) == "" || len(p.Name) > 64 {
		return errors.New("sourceoauth: provider name is required")
	}
	if p.TokenAuthStyle != "" && p.TokenAuthStyle != TokenAuthForm && p.TokenAuthStyle != TokenAuthBasic {
		return errors.New("sourceoauth: unsupported token authentication style")
	}
	if p.PKCE {
		if err := validateEndpoint(p.AuthorizeURL); err != nil {
			return errors.New("sourceoauth: invalid authorization endpoint")
		}
	}
	if p.Device {
		if err := validateEndpoint(p.DeviceURL); err != nil {
			return errors.New("sourceoauth: invalid device endpoint")
		}
	}
	if err := validateEndpoint(p.TokenURL); err != nil {
		return errors.New("sourceoauth: invalid token endpoint")
	}
	return nil
}

func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return errors.New("invalid endpoint")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return errors.New("endpoint must use HTTPS")
}

func newHTTPClient(client *http.Client) *http.Client {
	copyClient := &http.Client{Timeout: 30 * time.Second, Transport: http.DefaultTransport}
	if client != nil {
		*copyClient = *client
		if copyClient.Transport == nil {
			copyClient.Transport = http.DefaultTransport
		}
		if copyClient.Timeout <= 0 || copyClient.Timeout > 30*time.Second {
			copyClient.Timeout = 30 * time.Second
		}
	}
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return copyClient
}

func readBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil || len(b) > maxResponseBytes {
		return nil, errors.New("sourceoauth: invalid provider response")
	}
	return b, nil
}

type tokenResponse struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	TokenType    string   `json:"token_type"`
	Scope        string   `json:"scope"`
	Scopes       []string `json:"scopes"`
	ExpiresIn    *int64   `json:"expires_in"`
	Expiry       string   `json:"expiry"`
	ErrorCode    string   `json:"error"`
}

func parseTokenResponse(body []byte, contentType string, now time.Time) (Bundle, string, error) {
	var response tokenResponse
	if strings.Contains(strings.ToLower(contentType), "application/x-www-form-urlencoded") {
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return Bundle{}, "", errors.New("sourceoauth: invalid provider response")
		}
		response = tokenResponse{
			AccessToken: values.Get("access_token"), RefreshToken: values.Get("refresh_token"),
			TokenType: values.Get("token_type"), Scope: values.Get("scope"), ErrorCode: values.Get("error"),
		}
		if raw := values.Get("expires_in"); raw != "" {
			expiresIn, err := parseInt64(raw)
			if err != nil {
				return Bundle{}, "", errors.New("sourceoauth: invalid provider response")
			}
			response.ExpiresIn = &expiresIn
		}
		response.Expiry = values.Get("expiry")
	} else if err := json.Unmarshal(body, &response); err != nil {
		return Bundle{}, "", errors.New("sourceoauth: invalid provider response")
	}
	if response.ErrorCode != "" {
		if response.ErrorCode == "invalid_grant" {
			return Bundle{}, response.ErrorCode, ErrReauthRequired
		}
		return Bundle{}, response.ErrorCode, errors.New("sourceoauth: provider rejected authorization")
	}
	bundle := Bundle{AccessToken: response.AccessToken, RefreshToken: response.RefreshToken, TokenType: response.TokenType}
	if len(response.Scopes) > 0 {
		bundle.Scopes = response.Scopes
	} else if response.Scope != "" {
		bundle.Scopes = strings.Fields(response.Scope)
	}
	if response.Expiry != "" {
		parsed, err := time.Parse(time.RFC3339, response.Expiry)
		if err != nil || !strings.HasSuffix(response.Expiry, "Z") {
			return Bundle{}, "", errors.New("sourceoauth: invalid provider response")
		}
		bundle.Expiry = parsed.UTC()
	} else if response.ExpiresIn != nil {
		if *response.ExpiresIn < 0 || *response.ExpiresIn > int64((365*24*time.Hour)/time.Second) {
			return Bundle{}, "", errors.New("sourceoauth: invalid provider response")
		}
		bundle.Expiry = now.UTC().Add(time.Duration(*response.ExpiresIn) * time.Second).Truncate(time.Second)
	}
	if err := bundle.Validate(); err != nil {
		return Bundle{}, "", errors.New("sourceoauth: invalid provider response")
	}
	return bundle, "", nil
}

func parseInt64(value string) (int64, error) {
	return strconv.ParseInt(value, 10, 64)
}

func postForm(ctx context.Context, client *http.Client, endpoint string, values url.Values, clientID, clientSecret string, style TokenAuthStyle) (*http.Response, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, errors.New("sourceoauth: invalid OAuth endpoint")
	}
	if style != TokenAuthBasic || clientSecret == "" {
		if clientID != "" {
			values.Set("client_id", clientID)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, errors.New("sourceoauth: could not create OAuth request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if style == TokenAuthBasic && clientSecret != "" {
		req.SetBasicAuth(clientID, clientSecret)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("sourceoauth: OAuth request failed")
	}
	return resp, nil
}

func exchange(ctx context.Context, client *http.Client, provider Provider, clientID, clientSecret string, values url.Values, now time.Time) (Bundle, string, error) {
	resp, err := postForm(ctx, client, provider.TokenURL, values, clientID, clientSecret, provider.TokenAuthStyle)
	if err != nil {
		return Bundle{}, "", err
	}
	defer resp.Body.Close()
	body, err := readBounded(resp.Body)
	if err != nil {
		return Bundle{}, "", err
	}
	bundle, code, err := parseTokenResponse(body, resp.Header.Get("Content-Type"), now)
	if err != nil {
		return Bundle{}, code, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Bundle{}, "", errors.New("sourceoauth: provider rejected authorization")
	}
	return bundle, "", nil
}

func constantTimeEqual(a, b string) bool {
	left, right := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func randomHex(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", errors.New("sourceoauth: secure random generation failed")
	}
	return hex.EncodeToString(buf), nil
}

func pkceVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", errors.New("sourceoauth: secure random generation failed")
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func scopesOrDefault(given, defaults []string) string {
	if len(given) == 0 {
		given = defaults
	}
	return strings.Join(given, " ")
}

// TokenStore persists validated bundles only through ports.SecretStore.
type TokenStore struct {
	Secrets ports.SecretStore
	Key     string
}

// Load reads and validates the bundle. Missing, malformed, or invalid data is
// returned as an error and is never replaced with a plaintext fallback.
func (s TokenStore) Load(ctx context.Context) (Bundle, error) {
	if s.Secrets == nil || strings.TrimSpace(s.Key) == "" {
		return Bundle{}, errors.New("sourceoauth: token store is not configured")
	}
	raw, err := s.Secrets.Get(ctx, s.Key)
	if err != nil {
		return Bundle{}, errors.New("sourceoauth: token bundle unavailable")
	}
	if len(raw) > maxResponseBytes {
		return Bundle{}, errors.New("sourceoauth: stored token bundle is invalid")
	}
	var bundle Bundle
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		return Bundle{}, errors.New("sourceoauth: stored token bundle is invalid")
	}
	return bundle, nil
}

// Save validates and writes the complete bundle to the supplied secret key.
func (s TokenStore) Save(ctx context.Context, bundle Bundle) error {
	if s.Secrets == nil || strings.TrimSpace(s.Key) == "" {
		return errors.New("sourceoauth: token store is not configured")
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		return errors.New("sourceoauth: token bundle is invalid")
	}
	if err := s.Secrets.Set(ctx, s.Key, string(raw)); err != nil {
		return errors.New("sourceoauth: token bundle could not be stored")
	}
	return nil
}
