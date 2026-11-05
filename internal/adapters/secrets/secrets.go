// Package secrets resolves connector and server credentials from the
// environment first and the operating-system secret store second.
//
// Environment variables are read-only: Set and Delete only touch the OS store.
// A key supplied by the environment shadows the OS store, and Set/Delete on
// such a key fail closed instead of silently doing nothing. Secret values are
// never logged, written to files, or included in error messages.
package secrets

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	// DefaultEnvPrefix is prepended to canonical secret names.
	DefaultEnvPrefix = "BRAIN_SECRET_"
	// maxKeyLength bounds canonical names before they reach a platform store.
	maxKeyLength = 200
	// indexKey is reserved for the name index on stores without enumeration.
	indexKey = "__LV24_BRAIN_INDEX__"
)

// Options configures the composite store. Zero values are safe.
type Options struct {
	// EnvPrefix defaults to BRAIN_SECRET_.
	EnvPrefix string
	// Backend overrides the platform secret store (tests and custom hosts).
	Backend backend
	// LookupEnv overrides os.LookupEnv (tests).
	LookupEnv func(string) (string, bool)
	// Environ overrides os.Environ (tests).
	Environ func() []string
}

// Store implements ports.SecretStore with env-first resolution.
type Store struct {
	prefix    string
	backend   backend
	lookupEnv func(string) (string, bool)
	environ   func() []string
}

// New builds a secret store. The platform backend is selected only when no
// custom backend is supplied.
func New(opts Options) *Store {
	prefix := opts.EnvPrefix
	if prefix == "" {
		prefix = DefaultEnvPrefix
	}
	backendImpl := opts.Backend
	if backendImpl == nil {
		backendImpl = newPlatformBackend()
	}
	lookupEnv := opts.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	environ := opts.Environ
	if environ == nil {
		environ = os.Environ
	}
	return &Store{prefix: prefix, backend: backendImpl, lookupEnv: lookupEnv, environ: environ}
}

// Default returns a store with environment and platform defaults.
func Default() *Store {
	return New(Options{})
}

// Get resolves a secret from the environment first, then the OS store.
func (s *Store) Get(ctx context.Context, key string) (string, error) {
	canonical, err := NormalizeKey(key)
	if err != nil {
		return "", err
	}
	if value, ok := s.lookupEnv(s.prefix + canonical); ok && value != "" {
		return value, nil
	}
	value, err := s.backend.Get(ctx, canonical)
	if err != nil {
		return "", fmt.Errorf("secret %q: %w", canonical, err)
	}
	if value == "" {
		return "", fmt.Errorf("secret %q: %w", canonical, domain.ErrNotFound)
	}
	return value, nil
}

// Set stores a secret in the OS store. It fails when the environment already
// supplies the key or when the platform backend is unavailable.
func (s *Store) Set(ctx context.Context, key, value string) error {
	canonical, err := NormalizeKey(key)
	if err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return domain.NewValidationError("secret.value", "must not be empty")
	}
	if value, ok := s.lookupEnv(s.prefix + canonical); ok && value != "" {
		return fmt.Errorf("secret %q is provided by the environment and cannot be overwritten", canonical)
	}
	if err := s.backend.Set(ctx, canonical, value); err != nil {
		return fmt.Errorf("secret %q: %w", canonical, err)
	}
	return nil
}

// Delete removes a secret from the OS store. Deleting a missing OS item is
// successful. An environment-provided key cannot be deleted here.
func (s *Store) Delete(ctx context.Context, key string) error {
	canonical, err := NormalizeKey(key)
	if err != nil {
		return err
	}
	if value, ok := s.lookupEnv(s.prefix + canonical); ok && value != "" {
		return fmt.Errorf("secret %q is provided by the environment and cannot be deleted", canonical)
	}
	if err := s.backend.Delete(ctx, canonical); err != nil {
		return fmt.Errorf("secret %q: %w", canonical, err)
	}
	return nil
}

// Names returns the union of environment and OS-store names, sorted. An
// unavailable platform store still reports environment names.
func (s *Store) Names(ctx context.Context) ([]string, error) {
	seen := make(map[string]struct{})
	for _, envName := range s.environ() {
		name, _, _ := strings.Cut(envName, "=")
		if !strings.HasPrefix(name, s.prefix) {
			continue
		}
		canonical, err := NormalizeKey(strings.TrimPrefix(name, s.prefix))
		if err == nil {
			seen[canonical] = struct{}{}
		}
	}

	names, err := s.backend.Names(ctx)
	if err != nil {
		if !isUnavailable(err) {
			return nil, err
		}
	} else {
		for _, name := range names {
			if name != "" {
				seen[name] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// NormalizeKey maps user-facing names such as github/token to a canonical
// environment/OS-store name such as GITHUB_TOKEN. The mapping is
// case-insensitive and deterministic.
func NormalizeKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", domain.NewValidationError("secret.key", "is required")
	}
	if len(key) > maxKeyLength {
		return "", domain.NewValidationError("secret.key", fmt.Sprintf("must be at most %d characters", maxKeyLength))
	}

	var b strings.Builder
	b.Grow(len(key))
	hasAlnum := false
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
			hasAlnum = true
			b.WriteByte(byte(r - 'a' + 'A'))
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			hasAlnum = true
			b.WriteRune(r)
		case r == '.', r == '_', r == '/', r == '-':
			b.WriteByte('_')
		default:
			return "", domain.NewValidationError("secret.key", "may contain only letters, digits, '.', '_', '/', '-'")
		}
	}
	canonical := b.String()
	if !hasAlnum {
		return "", domain.NewValidationError("secret.key", "must contain at least one letter or digit")
	}
	if canonical == indexKey {
		return "", domain.NewValidationError("secret.key", "uses a reserved name")
	}
	return canonical, nil
}
