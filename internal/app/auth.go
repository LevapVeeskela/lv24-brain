package app

import (
	"context"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// LoginOptions holds local server credentials.
type LoginOptions struct {
	Login    string
	Password string
}

// LoginResult is the authenticated principal after a successful login.
type LoginResult struct {
	User      domain.User
	Role      domain.Role
	ExpiresAt time.Time
}

// Login authenticates against a server-mode brain and persists the issued
// tokens through the auth adapter.
func (a *App) Login(ctx context.Context, opts LoginOptions) (LoginResult, error) {
	if a.deps.Auth == nil {
		return LoginResult{}, notImplemented("login")
	}
	login := strings.TrimSpace(opts.Login)
	if login == "" {
		return LoginResult{}, domain.NewValidationError("login.login", "is required")
	}
	if opts.Password == "" {
		return LoginResult{}, domain.NewValidationError("login.password", "is required")
	}
	session, err := a.deps.Auth.Login(ctx, ports.Credentials{Login: login, Password: opts.Password})
	if err != nil {
		return LoginResult{}, err
	}
	if session.User.Login == "" {
		return LoginResult{}, domain.NewValidationError("login", "server returned an empty user")
	}
	return LoginResult{
		User:      session.User,
		Role:      session.User.Role,
		ExpiresAt: session.ExpiresAt,
	}, nil
}

// Logout revokes the current server session and clears stored tokens.
func (a *App) Logout(ctx context.Context) error {
	if a.deps.Auth == nil {
		return notImplemented("logout")
	}
	return a.deps.Auth.Logout(ctx, "")
}

// passwordChanger and allSessionsRevoker are client-only capabilities. The
// server-side auth service does not provide them, so they are checked at
// runtime rather than added to ports.AuthProvider.
type passwordChanger interface {
	ChangePassword(ctx context.Context, currentPassword, newPassword string) error
}

type allSessionsRevoker interface {
	RevokeAllSessions(ctx context.Context) error
}

// ChangePassword changes the signed-in account's local password.
func (a *App) ChangePassword(ctx context.Context, currentPassword, newPassword string) error {
	changer, ok := a.deps.Auth.(passwordChanger)
	if a.deps.Auth == nil || !ok {
		return notImplemented("password")
	}
	return changer.ChangePassword(ctx, currentPassword, newPassword)
}

// LogoutAll revokes every session of the signed-in account and clears the
// stored credentials.
func (a *App) LogoutAll(ctx context.Context) error {
	revoker, ok := a.deps.Auth.(allSessionsRevoker)
	if a.deps.Auth == nil || !ok {
		return notImplemented("logout --all")
	}
	return revoker.RevokeAllSessions(ctx)
}
