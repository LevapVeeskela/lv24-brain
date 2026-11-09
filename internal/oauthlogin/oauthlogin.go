// Package oauthlogin adapts locally stored source OAuth sessions to the
// credential-provider contract used by source adapters.
package oauthlogin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/sourceoauth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// MaxBundleBytes bounds stored token JSON before it is decoded.
	MaxBundleBytes = 1 << 20
	// MaxMetadataBytes bounds stored token-free session metadata.
	MaxMetadataBytes = 64 << 10
)

var (
	// ErrSessionNotFound indicates that one or both local OAuth session keys
	// are absent. It also matches domain.ErrNotFound.
	ErrSessionNotFound = fmt.Errorf("oauthlogin: session not found: %w", domain.ErrNotFound)
	// ErrReauthRequired is returned when the saved authorization was rejected.
	ErrReauthRequired = sourceoauth.ErrReauthRequired
)

// Session combines token-free provider metadata with its local OAuth bundle.
// Do not put Session in the synchronized connection catalogue.
type Session struct {
	Provider string
	ClientID string
	Scopes   []string
	Bundle   sourceoauth.Bundle
}

type metadata struct {
	Provider string   `json:"provider"`
	ClientID string   `json:"clientId"`
	Scopes   []string `json:"scopes,omitempty"`
}

// SessionStore persists OAuth credentials only in the supplied local secret
// store. The bundle key is also the key used by sourceoauth.TokenStore.
type SessionStore struct {
	Secrets ports.SecretStore
}

// BundleKey returns conn/<connectionID>/oauth after validating the ID.
func BundleKey(connectionID string) (string, error) {
	return domain.CredentialKey(connectionID, "oauth")
}

// MetadataKey returns conn/<connectionID>/oauthmeta after validating the ID.
func MetadataKey(connectionID string) (string, error) {
	return domain.CredentialKey(connectionID, "oauthmeta")
}

// Save validates and writes the bundle before its token-free metadata.
func (s SessionStore) Save(ctx context.Context, connectionID string, session Session) error {
	if s.Secrets == nil {
		return errors.New("oauthlogin: session store is not configured")
	}
	bundleKey, metaKey, err := sessionKeys(connectionID)
	if err != nil {
		return err
	}
	meta, err := makeMetadata(session)
	if err != nil {
		return err
	}
	bundleRaw, err := json.Marshal(session.Bundle)
	if err != nil || len(bundleRaw) > MaxBundleBytes {
		return errors.New("oauthlogin: token bundle is invalid")
	}
	metaRaw, err := json.Marshal(meta)
	if err != nil || len(metaRaw) > MaxMetadataBytes {
		return errors.New("oauthlogin: session metadata is invalid")
	}
	if err := s.Secrets.Set(ctx, bundleKey, string(bundleRaw)); err != nil {
		return errors.New("oauthlogin: token bundle could not be stored")
	}
	if err := s.Secrets.Set(ctx, metaKey, string(metaRaw)); err != nil {
		return errors.New("oauthlogin: session metadata could not be stored")
	}
	return nil
}

// Load reads both keys, returning ErrSessionNotFound if either is absent.
// Stored values and underlying secret-store errors are never included in
// returned errors.
func (s SessionStore) Load(ctx context.Context, connectionID string) (Session, error) {
	if s.Secrets == nil {
		return Session{}, errors.New("oauthlogin: session store is not configured")
	}
	bundleKey, metaKey, err := sessionKeys(connectionID)
	if err != nil {
		return Session{}, err
	}
	bundleRaw, err := s.Secrets.Get(ctx, bundleKey)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, errors.New("oauthlogin: token bundle is unavailable")
	}
	metaRaw, err := s.Secrets.Get(ctx, metaKey)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, errors.New("oauthlogin: session metadata is unavailable")
	}
	if len(bundleRaw) > MaxBundleBytes || len(metaRaw) > MaxMetadataBytes {
		return Session{}, errors.New("oauthlogin: stored session is invalid")
	}
	var bundle sourceoauth.Bundle
	if err := json.Unmarshal([]byte(bundleRaw), &bundle); err != nil {
		return Session{}, errors.New("oauthlogin: stored token bundle is invalid")
	}
	var meta metadata
	if err := decodeMetadata(metaRaw, &meta); err != nil {
		return Session{}, errors.New("oauthlogin: stored session metadata is invalid")
	}
	session := Session{Provider: meta.Provider, ClientID: meta.ClientID, Scopes: meta.Scopes, Bundle: bundle}
	if _, err := makeMetadata(session); err != nil {
		return Session{}, errors.New("oauthlogin: stored session metadata is invalid")
	}
	return cloneSession(session), nil
}

// Delete removes both OAuth session keys, attempting each removal even if the
// first fails.
func (s SessionStore) Delete(ctx context.Context, connectionID string) error {
	if s.Secrets == nil {
		return errors.New("oauthlogin: session store is not configured")
	}
	bundleKey, metaKey, err := sessionKeys(connectionID)
	if err != nil {
		return err
	}
	bundleErr := s.Secrets.Delete(ctx, bundleKey)
	metaErr := s.Secrets.Delete(ctx, metaKey)
	if bundleErr != nil || metaErr != nil {
		return errors.New("oauthlogin: session could not be deleted")
	}
	return nil
}

func sessionKeys(connectionID string) (string, string, error) {
	bundleKey, err := BundleKey(connectionID)
	if err != nil {
		return "", "", err
	}
	metaKey, err := MetadataKey(connectionID)
	if err != nil {
		return "", "", err
	}
	return bundleKey, metaKey, nil
}

func makeMetadata(session Session) (metadata, error) {
	if err := domain.ValidateProvider("oauth.provider", session.Provider); err != nil {
		return metadata{}, err
	}
	if strings.TrimSpace(session.ClientID) == "" || len(session.ClientID) > 4096 {
		return metadata{}, domain.NewValidationError("oauth.clientId", "is required and must be at most 4096 bytes")
	}
	if len(session.Scopes) > 128 {
		return metadata{}, domain.NewValidationError("oauth.scopes", "must contain at most 128 entries")
	}
	for _, scope := range session.Scopes {
		if scope == "" || len(scope) > 256 {
			return metadata{}, domain.NewValidationError("oauth.scopes", "contains an invalid entry")
		}
	}
	if err := session.Bundle.Validate(); err != nil {
		return metadata{}, errors.New("oauthlogin: token bundle is invalid")
	}
	return metadata{Provider: session.Provider, ClientID: session.ClientID, Scopes: append([]string(nil), session.Scopes...)}, nil
}

func decodeMetadata(raw string, out *metadata) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing metadata")
	}
	return nil
}

func cloneSession(session Session) Session {
	session.Scopes = append([]string(nil), session.Scopes...)
	session.Bundle.Scopes = append([]string(nil), session.Bundle.Scopes...)
	return session
}

type cachedRefresher struct {
	provider  string
	clientID  string
	scopes    string
	refresher *sourceoauth.Refresher
}

// Credentials resolves token requests from an OAuth session, falling back to
// Base when no complete session exists. Non-token fields always use Base.
type Credentials struct {
	Base      ports.CredentialProvider
	Sessions  *SessionStore
	Providers map[string]sourceoauth.Provider
	Now       func() time.Time

	mu         sync.Mutex
	refreshers map[string]cachedRefresher
}

var _ ports.CredentialProvider = (*Credentials)(nil)

// Get implements ports.CredentialProvider. Only a missing or partial OAuth
// session falls back to the base provider; malformed sessions fail closed.
func (c *Credentials) Get(ctx context.Context, connectionID, field string) (string, error) {
	if _, err := domain.CredentialKey(connectionID, field); err != nil {
		return "", err
	}
	if c == nil {
		return "", errors.New("oauthlogin: credential provider is not configured")
	}
	if field != "token" {
		return c.baseGet(ctx, connectionID, field)
	}
	if c.Sessions == nil {
		return c.baseGet(ctx, connectionID, field)
	}
	session, err := c.Sessions.Load(ctx, connectionID)
	if errors.Is(err, ErrSessionNotFound) {
		return c.baseGet(ctx, connectionID, field)
	}
	if err != nil {
		return "", err
	}
	refresher, err := c.refresherFor(connectionID, session)
	if err != nil {
		return "", err
	}
	token, err := refresher.Token(ctx)
	if errors.Is(err, sourceoauth.ErrReauthRequired) {
		return "", fmt.Errorf("oauthlogin: reauthorization required: %w", ErrReauthRequired)
	}
	if err != nil {
		return "", errors.New("oauthlogin: access token is unavailable")
	}
	return token, nil
}

func (c *Credentials) baseGet(ctx context.Context, connectionID, field string) (string, error) {
	if c == nil || c.Base == nil {
		return "", errors.New("oauthlogin: base credential provider is not configured")
	}
	return c.Base.Get(ctx, connectionID, field)
}

func (c *Credentials) refresherFor(connectionID string, session Session) (*sourceoauth.Refresher, error) {
	providers := sourceoauth.Providers()
	for name, provider := range c.Providers {
		providers[name] = provider
	}
	provider, ok := providers[session.Provider]
	if !ok {
		return nil, errors.New("oauthlogin: OAuth provider is not configured")
	}
	provider.DefaultScopes = append([]string(nil), session.Scopes...)
	scopeKey := strings.Join(session.Scopes, "\x00")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refreshers == nil {
		c.refreshers = make(map[string]cachedRefresher)
	}
	if cached, ok := c.refreshers[connectionID]; ok && cached.provider == session.Provider && cached.clientID == session.ClientID && cached.scopes == scopeKey {
		return cached.refresher, nil
	}
	bundleKey, err := BundleKey(connectionID)
	if err != nil {
		return nil, err
	}
	refresher := &sourceoauth.Refresher{
		Provider: provider, ClientID: session.ClientID,
		Store: sourceoauth.TokenStore{Secrets: c.Sessions.Secrets, Key: bundleKey}, Now: c.Now,
	}
	c.refreshers[connectionID] = cachedRefresher{provider: session.Provider, clientID: session.ClientID, scopes: scopeKey, refresher: refresher}
	return refresher, nil
}
