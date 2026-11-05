package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type fakeAuth struct {
	session     ports.Session
	loginErr    error
	loginCalls  int
	logoutCalls int
	lastLogin   ports.Credentials
}

func (f *fakeAuth) Login(_ context.Context, credentials ports.Credentials) (ports.Session, error) {
	f.loginCalls++
	f.lastLogin = credentials
	if f.loginErr != nil {
		return ports.Session{}, f.loginErr
	}
	return f.session, nil
}

func (f *fakeAuth) Refresh(context.Context, string) (ports.Session, error) {
	return f.session, nil
}

func (f *fakeAuth) Logout(context.Context, string) error {
	f.logoutCalls++
	return nil
}

func (f *fakeAuth) Principal(context.Context, string) (domain.User, error) {
	return f.session.User, nil
}

func TestLoginUseCase(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC()
	auth := &fakeAuth{session: ports.Session{
		AccessToken: "access",
		ExpiresAt:   expires,
		User:        domain.User{ID: "user_1", Login: "alice", Role: domain.RoleMember},
	}}
	useCase := app.New(config.Default(), app.Dependencies{Auth: auth})

	result, err := useCase.Login(context.Background(), app.LoginOptions{Login: "  alice  ", Password: "secret"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if auth.loginCalls != 1 || auth.lastLogin.Login != "alice" || auth.lastLogin.Password != "secret" {
		t.Fatalf("auth calls = %+v", auth)
	}
	if result.User.Login != "alice" || result.Role != domain.RoleMember || !result.ExpiresAt.Equal(expires) {
		t.Fatalf("result = %+v", result)
	}
}

func TestLoginUseCaseValidation(t *testing.T) {
	auth := &fakeAuth{}
	useCase := app.New(config.Default(), app.Dependencies{Auth: auth})

	if _, err := useCase.Login(context.Background(), app.LoginOptions{Password: "secret"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty login error = %v", err)
	}
	if _, err := useCase.Login(context.Background(), app.LoginOptions{Login: "alice"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty password error = %v", err)
	}
	if auth.loginCalls != 0 {
		t.Fatalf("login calls = %d", auth.loginCalls)
	}
}

func TestLoginUseCaseReportsAuthErrors(t *testing.T) {
	auth := &fakeAuth{loginErr: errors.New("invalid credentials")}
	useCase := app.New(config.Default(), app.Dependencies{Auth: auth})
	if _, err := useCase.Login(context.Background(), app.LoginOptions{Login: "alice", Password: "wrong"}); err == nil {
		t.Fatal("expected login error")
	}
}

func TestLoginUseCaseRejectsEmptyUser(t *testing.T) {
	auth := &fakeAuth{session: ports.Session{AccessToken: "access"}}
	useCase := app.New(config.Default(), app.Dependencies{Auth: auth})
	if _, err := useCase.Login(context.Background(), app.LoginOptions{Login: "alice", Password: "secret"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty user error = %v", err)
	}
}

func TestLogoutUseCase(t *testing.T) {
	auth := &fakeAuth{}
	useCase := app.New(config.Default(), app.Dependencies{Auth: auth})
	if err := useCase.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if auth.logoutCalls != 1 {
		t.Fatalf("logout calls = %d", auth.logoutCalls)
	}
}

func TestAuthUseCasesRequireAdapter(t *testing.T) {
	useCase := app.New(config.Default(), app.Dependencies{})
	if _, err := useCase.Login(context.Background(), app.LoginOptions{Login: "alice", Password: "secret"}); !errors.Is(err, app.ErrNotImplemented) {
		t.Fatalf("Login error = %v, want ErrNotImplemented", err)
	}
	if err := useCase.Logout(context.Background()); !errors.Is(err, app.ErrNotImplemented) {
		t.Fatalf("Logout error = %v, want ErrNotImplemented", err)
	}
}
