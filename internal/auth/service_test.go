package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type stubUsers struct{}

func (stubUsers) LookupUser(context.Context, string) (domain.User, string, error) {
	return domain.User{}, "", domain.ErrNotFound
}

func (stubUsers) FindUser(context.Context, string) (domain.User, error) {
	return domain.User{}, domain.ErrNotFound
}

type stubSessions struct{}

func (stubSessions) CreateRefresh(context.Context, RefreshRecord) error { return nil }

func (stubSessions) ConsumeRefresh(context.Context, string, time.Time) (RefreshRecord, error) {
	return RefreshRecord{}, ErrInvalidRefresh
}

func (stubSessions) RevokeSession(context.Context, string, time.Time) error { return nil }

func (stubSessions) RevokeFamily(context.Context, string, time.Time) error { return nil }

type stubAPITokens struct {
	user  domain.User
	err   error
	calls int
}

func (s *stubAPITokens) AuthenticateAPIToken(context.Context, string) (domain.User, error) {
	s.calls++
	return s.user, s.err
}

func newAPITokenService(t *testing.T, authenticator *stubAPITokens) *Service {
	t.Helper()
	signer := testSigner(t, time.Now)
	service, err := NewService(ServiceConfig{
		Signer:         signer,
		Users:          stubUsers{},
		Sessions:       stubSessions{},
		APITokens:      authenticator,
		RefreshTTL:     time.Hour,
		PasswordParams: testParams,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func TestAuthenticateAPITokenUnknownTokenIsInvalid(t *testing.T) {
	authenticator := &stubAPITokens{err: domain.ErrNotFound}
	service := newAPITokenService(t, authenticator)

	if _, err := service.AuthenticateAPIToken(context.Background(), "brain_missing"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("error = %v, want ErrInvalidToken", err)
	}
	if authenticator.calls != 1 {
		t.Fatalf("calls = %d, want 1", authenticator.calls)
	}
}

func TestAuthenticateAPITokenBridgeInvalidTokenPassesThrough(t *testing.T) {
	authenticator := &stubAPITokens{err: ErrInvalidToken}
	service := newAPITokenService(t, authenticator)

	if _, err := service.AuthenticateAPIToken(context.Background(), "brain_missing"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("error = %v, want ErrInvalidToken", err)
	}
}

func TestAuthenticateAPITokenPropagatesInfrastructureErrors(t *testing.T) {
	infra := errors.New("database unavailable")
	authenticator := &stubAPITokens{err: infra}
	service := newAPITokenService(t, authenticator)

	_, err := service.AuthenticateAPIToken(context.Background(), "brain_token")
	if err == nil {
		t.Fatal("expected an infrastructure error")
	}
	if errors.Is(err, ErrInvalidToken) {
		t.Fatalf("infrastructure error was mapped to ErrInvalidToken: %v", err)
	}
	if !errors.Is(err, infra) {
		t.Fatalf("error = %v, want the wrapped infrastructure error", err)
	}
}

func TestAuthenticateAPITokenRequiresToken(t *testing.T) {
	authenticator := &stubAPITokens{user: domain.User{ID: "user_1", Login: "alice", Role: domain.RoleMember}}
	service := newAPITokenService(t, authenticator)

	if _, err := service.AuthenticateAPIToken(context.Background(), "  "); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("error = %v, want ErrInvalidToken", err)
	}
	if authenticator.calls != 0 {
		t.Fatalf("calls = %d, want 0", authenticator.calls)
	}
}

type testSessions struct {
	consume      func(ctx context.Context, tokenHash string, now time.Time) (RefreshRecord, error)
	revokeFamily func(ctx context.Context, familyID string, now time.Time) error
}

func (testSessions) CreateRefresh(context.Context, RefreshRecord) error { return nil }

func (s testSessions) ConsumeRefresh(ctx context.Context, tokenHash string, now time.Time) (RefreshRecord, error) {
	return s.consume(ctx, tokenHash, now)
}

func (testSessions) RevokeSession(context.Context, string, time.Time) error { return nil }

func (s testSessions) RevokeFamily(ctx context.Context, familyID string, now time.Time) error {
	if s.revokeFamily == nil {
		return nil
	}
	return s.revokeFamily(ctx, familyID, now)
}

type testUsers struct {
	user    domain.User
	findErr error
}

func (testUsers) LookupUser(context.Context, string) (domain.User, string, error) {
	return domain.User{}, "", domain.ErrNotFound
}

func (s testUsers) FindUser(context.Context, string) (domain.User, error) {
	if s.findErr != nil {
		return domain.User{}, s.findErr
	}
	return s.user, nil
}

func newRefreshService(t *testing.T, users UserDirectory, sessions SessionStore) *Service {
	t.Helper()
	service, err := NewService(ServiceConfig{
		Signer:         testSigner(t, time.Now),
		Users:          users,
		Sessions:       sessions,
		RefreshTTL:     time.Hour,
		PasswordParams: testParams,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func TestRefreshMapsCredentialErrors(t *testing.T) {
	for _, cause := range []error{
		ErrInvalidRefresh,
		ErrRefreshReused,
	} {
		sessions := testSessions{consume: func(context.Context, string, time.Time) (RefreshRecord, error) {
			return RefreshRecord{}, cause
		}}
		service := newRefreshService(t, testUsers{}, sessions)
		if _, err := service.Refresh(context.Background(), "token"); !errors.Is(err, ErrInvalidRefresh) {
			t.Fatalf("cause %v: error = %v, want ErrInvalidRefresh", cause, err)
		}
	}
}

func TestRefreshPropagatesDomainErrors(t *testing.T) {
	domainErrors := []error{
		domain.ErrNotFound,
		domain.NewValidationError("refresh", "invalid"),
	}
	for _, cause := range domainErrors {
		sessions := testSessions{consume: func(context.Context, string, time.Time) (RefreshRecord, error) {
			return RefreshRecord{}, cause
		}}
		service := newRefreshService(t, testUsers{}, sessions)
		_, err := service.Refresh(context.Background(), "token")
		if !errors.Is(err, cause) {
			t.Fatalf("consume cause %v: error = %v, want the domain error", cause, err)
		}
		if errors.Is(err, ErrInvalidRefresh) {
			t.Fatalf("consume cause %v: domain error was mapped to ErrInvalidRefresh: %v", cause, err)
		}
	}

	for _, cause := range domainErrors {
		sessions := testSessions{consume: func(context.Context, string, time.Time) (RefreshRecord, error) {
			return RefreshRecord{UserID: "user_1", FamilyID: "fam_1"}, nil
		}}
		service := newRefreshService(t, testUsers{findErr: cause}, sessions)
		_, err := service.Refresh(context.Background(), "token")
		if !errors.Is(err, cause) {
			t.Fatalf("user lookup cause %v: error = %v, want the domain error", cause, err)
		}
		if errors.Is(err, ErrInvalidRefresh) {
			t.Fatalf("user lookup cause %v: domain error was mapped to ErrInvalidRefresh: %v", cause, err)
		}
	}
}

func TestRefreshPropagatesConsumeInfrastructureError(t *testing.T) {
	infra := errors.New("db down")
	sessions := testSessions{consume: func(context.Context, string, time.Time) (RefreshRecord, error) {
		return RefreshRecord{}, infra
	}}
	service := newRefreshService(t, testUsers{}, sessions)

	_, err := service.Refresh(context.Background(), "token")
	if !errors.Is(err, infra) {
		t.Fatalf("error = %v, want the wrapped infrastructure error", err)
	}
	if errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("infrastructure error was mapped to ErrInvalidRefresh: %v", err)
	}
}

func TestRefreshReuseRevocationFailurePropagatesInfra(t *testing.T) {
	infra := errors.New("db down")
	revokedFamily := ""
	sessions := testSessions{
		consume: func(context.Context, string, time.Time) (RefreshRecord, error) {
			return RefreshRecord{FamilyID: "fam_1"}, ErrRefreshReused
		},
		revokeFamily: func(_ context.Context, familyID string, _ time.Time) error {
			revokedFamily = familyID
			return infra
		},
	}
	service := newRefreshService(t, testUsers{}, sessions)

	_, err := service.Refresh(context.Background(), "token")
	if !errors.Is(err, infra) {
		t.Fatalf("error = %v, want the revocation infrastructure error", err)
	}
	if errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("revocation failure was mapped to ErrInvalidRefresh: %v", err)
	}
	if !strings.Contains(err.Error(), "reuse") {
		t.Fatalf("error does not preserve the reuse signal: %v", err)
	}
	if revokedFamily != "fam_1" {
		t.Fatalf("revoked family = %q, want fam_1", revokedFamily)
	}
}

func TestRefreshReuseRevokesFamilyAndRejects(t *testing.T) {
	revokedFamily := ""
	sessions := testSessions{
		consume: func(context.Context, string, time.Time) (RefreshRecord, error) {
			return RefreshRecord{FamilyID: "fam_1"}, ErrRefreshReused
		},
		revokeFamily: func(_ context.Context, familyID string, _ time.Time) error {
			revokedFamily = familyID
			return nil
		},
	}
	service := newRefreshService(t, testUsers{}, sessions)

	if _, err := service.Refresh(context.Background(), "token"); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("error = %v, want ErrInvalidRefresh", err)
	}
	if revokedFamily != "fam_1" {
		t.Fatalf("revoked family = %q, want fam_1", revokedFamily)
	}
}

func TestRefreshPropagatesUserLookupInfrastructureError(t *testing.T) {
	infra := errors.New("db down")
	sessions := testSessions{consume: func(context.Context, string, time.Time) (RefreshRecord, error) {
		return RefreshRecord{UserID: "user_1", FamilyID: "fam_1"}, nil
	}}
	service := newRefreshService(t, testUsers{findErr: infra}, sessions)

	_, err := service.Refresh(context.Background(), "token")
	if !errors.Is(err, infra) {
		t.Fatalf("error = %v, want the wrapped user lookup error", err)
	}
	if errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("user lookup error was mapped to ErrInvalidRefresh: %v", err)
	}
}
