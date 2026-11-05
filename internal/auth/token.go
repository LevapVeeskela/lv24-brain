package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	// AccessTokenType marks JWTs minted for API access.
	AccessTokenType = "access"
	// MinJWTSecretLength is the smallest accepted HMAC signing key.
	MinJWTSecretLength = 32
)

// AccessClaims are the signed access-token claims.
type AccessClaims struct {
	Login       string `json:"login"`
	DisplayName string `json:"name,omitempty"`
	Role        string `json:"role"`
	SessionID   string `json:"sid"`
	FamilyID    string `json:"fid"`
	TokenType   string `json:"typ"`
	jwt.RegisteredClaims
}

// SignerConfig configures HS256 access tokens.
type SignerConfig struct {
	Secret   []byte
	Issuer   string
	Audience string
	TTL      time.Duration
	Leeway   time.Duration
	Now      func() time.Time
}

// Signer mints and validates access tokens.
type Signer struct {
	secret   []byte
	issuer   string
	audience string
	ttl      time.Duration
	leeway   time.Duration
	now      func() time.Time
}

// NewSigner validates config and returns a Signer.
func NewSigner(cfg SignerConfig) (*Signer, error) {
	if len(cfg.Secret) < MinJWTSecretLength {
		return nil, fmt.Errorf("jwt secret must be at least %d bytes", MinJWTSecretLength)
	}
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("jwt issuer is required")
	}
	if strings.TrimSpace(cfg.Audience) == "" {
		return nil, errors.New("jwt audience is required")
	}
	if cfg.TTL <= 0 {
		return nil, errors.New("access token ttl must be positive")
	}
	if cfg.Leeway < 0 {
		return nil, errors.New("jwt leeway must not be negative")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Signer{
		secret:   append([]byte(nil), cfg.Secret...),
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		ttl:      cfg.TTL,
		leeway:   cfg.Leeway,
		now:      now,
	}, nil
}

// Issue mints one access token for user and session.
func (s *Signer) Issue(user domain.User, sessionID, familyID string) (string, time.Time, error) {
	if user.ID == "" || user.Login == "" || !user.Role.Valid() {
		return "", time.Time{}, errors.New("user is not issuable")
	}
	if sessionID == "" || familyID == "" {
		return "", time.Time{}, errors.New("session identity is required")
	}
	now := s.now().UTC()
	expires := now.Add(s.ttl)
	claims := AccessClaims{
		Login:       user.Login,
		DisplayName: user.DisplayName,
		Role:        string(user.Role),
		SessionID:   sessionID,
		FamilyID:    familyID,
		TokenType:   AccessTokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   user.ID,
			Audience:  jwt.ClaimStrings{s.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
			ID:        sessionID + "." + expires.Format("20060102150405"),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expires, nil
}

// Parse validates an access token and returns its claims.
func (s *Signer) Parse(raw string) (AccessClaims, error) {
	var claims AccessClaims
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(s.issuer),
		jwt.WithAudience(s.audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(s.leeway),
		jwt.WithTimeFunc(s.now),
	)
	_, err := parser.ParseWithClaims(raw, &claims, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, ErrInvalidToken
		}
		return s.secret, nil
	})
	if err != nil {
		return AccessClaims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if claims.TokenType != AccessTokenType || claims.Subject == "" || claims.SessionID == "" || claims.FamilyID == "" {
		return AccessClaims{}, ErrInvalidToken
	}
	if _, err := domain.ParseRole(claims.Role); err != nil {
		return AccessClaims{}, ErrInvalidToken
	}
	if claims.ExpiresAt == nil {
		return AccessClaims{}, ErrInvalidToken
	}
	return claims, nil
}

// RandomToken returns n random bytes encoded with unpadded base64url.
func RandomToken(n int) (string, error) {
	if n < 16 {
		n = 16
	}
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// RandomID returns a prefixed opaque identifier.
func RandomID(prefix string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	encoded := hex.EncodeToString(raw)
	if prefix == "" {
		return encoded, nil
	}
	return prefix + "_" + encoded, nil
}

// HashToken returns the hex SHA-256 digest used to store opaque tokens at
// rest. Raw tokens are never persisted.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// GenerateToken creates a raw token with prefix, its storage hash, and a
// display prefix that is safe to show in listings.
func GenerateToken(prefix string, n int) (raw, hash, display string, err error) {
	secret, err := RandomToken(n)
	if err != nil {
		return "", "", "", err
	}
	raw = prefix + secret
	hash = HashToken(raw)
	display = raw
	if len(display) > len(prefix)+8 {
		display = display[:len(prefix)+8]
	}
	return raw, hash, display, nil
}

// Principal is an authenticated caller.
type Principal struct {
	User       domain.User
	SessionID  string
	FamilyID   string
	ExpiresAt  time.Time
	AuthSource string
}
