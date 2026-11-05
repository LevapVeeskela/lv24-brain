package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// UserDirectory resolves accounts for the auth service.
type UserDirectory interface {
	// LookupUser returns the public user and the stored password hash.
	LookupUser(ctx context.Context, login string) (domain.User, string, error)
	// FindUser returns a user by ID.
	FindUser(ctx context.Context, id string) (domain.User, error)
}

// APITokenAuthenticator validates an opaque API token and returns its owner.
type APITokenAuthenticator interface {
	AuthenticateAPIToken(ctx context.Context, rawToken string) (domain.User, error)
}

// RefreshRecord is one rotating refresh token. TokenHash is the SHA-256 of the
// raw token; raw values never reach storage.
type RefreshRecord struct {
	SessionID string
	FamilyID  string
	UserID    string
	TokenHash string
	IssuedAt  time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	RevokedAt *time.Time
}

// SessionStore persists refresh-token rotations and revocations.
type SessionStore interface {
	CreateRefresh(ctx context.Context, record RefreshRecord) error
	// ConsumeRefresh atomically marks an unused token as used. A token that
	// was already used is returned together with ErrRefreshReused so the
	// caller can revoke the family.
	ConsumeRefresh(ctx context.Context, tokenHash string, now time.Time) (RefreshRecord, error)
	RevokeSession(ctx context.Context, sessionID string, now time.Time) error
	RevokeFamily(ctx context.Context, familyID string, now time.Time) error
}

// OIDCClaims are the provider assertions the server trusts after ID-token
// verification.
type OIDCClaims struct {
	Issuer      string
	Subject     string
	Login       string
	DisplayName string
	Role        domain.Role
	Groups      []string
}

// OIDCProvider starts and completes an authorization-code + PKCE flow.
type OIDCProvider interface {
	AuthCodeURL(state, nonce, codeVerifier string) string
	// Exchange redeems the code and must verify that the ID token carries the
	// nonce issued for this flow.
	Exchange(ctx context.Context, code, codeVerifier, nonce string) (OIDCClaims, error)
}

// ServiceConfig configures the authentication service.
type ServiceConfig struct {
	Signer         *Signer
	Users          UserDirectory
	Sessions       SessionStore
	APITokens      APITokenAuthenticator
	RefreshTTL     time.Duration
	PasswordParams Argon2Params
	Now            func() time.Time
}

// Service authenticates local accounts, OIDC identities, and API tokens.
type Service struct {
	signer      *Signer
	users       UserDirectory
	sessions    SessionStore
	apiTokens   APITokenAuthenticator
	refreshTTL  time.Duration
	params      Argon2Params
	now         func() time.Time
	dummyHash   string
	refreshSize int
}

var _ ports.AuthProvider = (*Service)(nil)

// NewService validates config and prepares timing-equalisation material.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Signer == nil {
		return nil, errors.New("auth signer is required")
	}
	if cfg.Users == nil {
		return nil, errors.New("auth user directory is required")
	}
	if cfg.Sessions == nil {
		return nil, errors.New("auth session store is required")
	}
	if cfg.RefreshTTL <= 0 {
		return nil, errors.New("refresh ttl must be positive")
	}
	params := cfg.PasswordParams
	if params == (Argon2Params{}) {
		params = DefaultArgon2Params
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	secret, err := RandomToken(32)
	if err != nil {
		return nil, err
	}
	dummyHash, err := HashPassword(secret, params)
	if err != nil {
		return nil, fmt.Errorf("prepare auth timing hash: %w", err)
	}
	return &Service{
		signer:      cfg.Signer,
		users:       cfg.Users,
		sessions:    cfg.Sessions,
		apiTokens:   cfg.APITokens,
		refreshTTL:  cfg.RefreshTTL,
		params:      params,
		now:         now,
		dummyHash:   dummyHash,
		refreshSize: 32,
	}, nil
}

// Login authenticates a local account.
func (s *Service) Login(ctx context.Context, credentials ports.Credentials) (ports.Session, error) {
	login := strings.TrimSpace(credentials.Login)
	if login == "" || credentials.Password == "" || len(credentials.Password) > MaxPasswordLength {
		s.verifyDummy(credentials.Password)
		return ports.Session{}, ErrInvalidCredentials
	}
	user, hash, err := s.users.LookupUser(ctx, login)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			return ports.Session{}, err
		}
		s.verifyDummy(credentials.Password)
		return ports.Session{}, ErrInvalidCredentials
	}
	if hash == "" {
		s.verifyDummy(credentials.Password)
		return ports.Session{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(hash, credentials.Password)
	if err != nil {
		return ports.Session{}, fmt.Errorf("verify stored password: %w", err)
	}
	if !ok {
		return ports.Session{}, ErrInvalidCredentials
	}
	return s.issueSession(ctx, user, "")
}

// Refresh rotates a refresh token and returns a fresh session.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (ports.Session, error) {
	raw := strings.TrimSpace(refreshToken)
	if raw == "" {
		return ports.Session{}, ErrInvalidRefresh
	}
	now := s.now().UTC()
	record, err := s.sessions.ConsumeRefresh(ctx, HashToken(raw), now)
	if err != nil {
		if errors.Is(err, ErrRefreshReused) && record.FamilyID != "" {
			_ = s.sessions.RevokeFamily(ctx, record.FamilyID, now)
		}
		return ports.Session{}, ErrInvalidRefresh
	}
	user, err := s.users.FindUser(ctx, record.UserID)
	if err != nil {
		return ports.Session{}, ErrInvalidRefresh
	}
	return s.issueSession(ctx, user, record.FamilyID)
}

// Logout revokes one refresh session.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	return s.sessions.RevokeSession(ctx, sessionID, s.now().UTC())
}

// LogoutFamily revokes every rotated token of one login family.
func (s *Service) LogoutFamily(ctx context.Context, familyID string) error {
	if strings.TrimSpace(familyID) == "" {
		return nil
	}
	return s.sessions.RevokeFamily(ctx, familyID, s.now().UTC())
}

// Principal returns the user behind a validated access token.
func (s *Service) Principal(ctx context.Context, accessToken string) (domain.User, error) {
	principal, err := s.AuthenticateJWT(ctx, accessToken)
	if err != nil {
		return domain.User{}, err
	}
	return principal.User, nil
}

// AuthenticateJWT validates an access token without a database round-trip.
func (s *Service) AuthenticateJWT(_ context.Context, accessToken string) (Principal, error) {
	claims, err := s.signer.Parse(strings.TrimSpace(accessToken))
	if err != nil {
		return Principal{}, ErrInvalidToken
	}
	role, err := domain.ParseRole(claims.Role)
	if err != nil {
		return Principal{}, ErrInvalidToken
	}
	return Principal{
		User: domain.User{
			ID:          claims.Subject,
			Login:       claims.Login,
			DisplayName: claims.DisplayName,
			Role:        role,
			AuthSource:  "jwt",
		},
		SessionID:  claims.SessionID,
		FamilyID:   claims.FamilyID,
		ExpiresAt:  claims.ExpiresAt.Time,
		AuthSource: "jwt",
	}, nil
}

// AuthenticateAPIToken validates a scoped API token.
func (s *Service) AuthenticateAPIToken(ctx context.Context, rawToken string) (domain.User, error) {
	if s.apiTokens == nil || strings.TrimSpace(rawToken) == "" {
		return domain.User{}, ErrInvalidToken
	}
	user, err := s.apiTokens.AuthenticateAPIToken(ctx, strings.TrimSpace(rawToken))
	if err != nil {
		return domain.User{}, ErrInvalidToken
	}
	return user, nil
}

// IssueSessionForUser mints a token pair for an already authenticated
// principal (OIDC or bootstrap flows).
func (s *Service) IssueSessionForUser(ctx context.Context, user domain.User) (ports.Session, error) {
	return s.issueSession(ctx, user, "")
}

// PasswordParams reports the hashing policy for account provisioning.
func (s *Service) PasswordParams() Argon2Params { return s.params }

func (s *Service) issueSession(ctx context.Context, user domain.User, familyID string) (ports.Session, error) {
	now := s.now().UTC()
	sessionID, err := RandomID("sess")
	if err != nil {
		return ports.Session{}, err
	}
	if familyID == "" {
		familyID = sessionID
	}
	rawRefresh, err := RandomToken(s.refreshSize)
	if err != nil {
		return ports.Session{}, err
	}
	record := RefreshRecord{
		SessionID: sessionID,
		FamilyID:  familyID,
		UserID:    user.ID,
		TokenHash: HashToken(rawRefresh),
		IssuedAt:  now,
		ExpiresAt: now.Add(s.refreshTTL),
	}
	if err := s.sessions.CreateRefresh(ctx, record); err != nil {
		return ports.Session{}, fmt.Errorf("store refresh token: %w", err)
	}
	accessToken, expires, err := s.signer.Issue(user, sessionID, familyID)
	if err != nil {
		return ports.Session{}, err
	}
	return ports.Session{
		AccessToken:  accessToken,
		RefreshToken: rawRefresh,
		ExpiresAt:    expires,
		User:         user,
	}, nil
}

func (s *Service) verifyDummy(password string) {
	if len(password) > MaxPasswordLength {
		password = password[:MaxPasswordLength]
	}
	_, _ = VerifyPassword(s.dummyHash, password)
}
