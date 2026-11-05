// Package auth implements password hashing, JWT access tokens, rotating
// refresh tokens, API tokens, and the server authentication service.
package auth

import "errors"

var (
	// ErrInvalidCredentials reports a failed login without revealing whether
	// the account exists.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrInvalidToken reports a malformed, expired, or foreign access token.
	ErrInvalidToken = errors.New("invalid token")
	// ErrInvalidRefresh reports an unusable refresh token.
	ErrInvalidRefresh = errors.New("invalid refresh token")
	// ErrRefreshReused marks a refresh token presented after it was rotated.
	// The caller must revoke the whole token family.
	ErrRefreshReused = errors.New("refresh token reused")
	// ErrForbidden reports an authenticated principal without enough rights.
	ErrForbidden = errors.New("forbidden")
)
