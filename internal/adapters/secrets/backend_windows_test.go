//go:build windows

package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"golang.org/x/sys/windows"
)

func TestCredentialErrorMapping(t *testing.T) {
	if err := mapCredentialError("read credential", "GITHUB_TOKEN", windows.ERROR_NOT_FOUND); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("not found mapping = %v", err)
	}
	if err := mapCredentialError("read credential", "GITHUB_TOKEN", windows.ERROR_ACCESS_DENIED); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("access denied mapping = %v", err)
	}
}

func TestWindowsCredentialManagerRoundTrip(t *testing.T) {
	if os.Getenv("BRAIN_TEST_KEYCHAIN") != "1" {
		t.Skip("set BRAIN_TEST_KEYCHAIN=1 to exercise the Windows Credential Manager")
	}
	backend := newPlatformBackend()
	ctx := context.Background()
	key := fmt.Sprintf("TEST_%d", time.Now().UnixNano())
	value := "test-value-not-a-real-secret"

	if err := backend.Set(ctx, key, value); err != nil {
		t.Fatalf("Set: %v", err)
	}
	t.Cleanup(func() { _ = backend.Delete(context.Background(), key) })

	got, err := backend.Get(ctx, key)
	if err != nil || got != value {
		t.Fatalf("Get = (%q, %v)", got, err)
	}
	names, err := backend.Names(ctx)
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if !slices.Contains(names, key) {
		t.Fatalf("Names missing %q: %v", key, names)
	}
	if err := backend.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := backend.Get(ctx, key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete = %v", err)
	}
}
