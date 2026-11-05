package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// SecretSet stores a secret in the OS secret store and returns the requested
// name. Values are never returned from this package.
func (a *App) SecretSet(ctx context.Context, name, value string) (string, error) {
	if err := a.requireSecrets(); err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	name = strings.TrimSpace(name)
	if err := a.deps.Secrets.Set(ctx, name, value); err != nil {
		return "", err
	}
	return name, nil
}

// SecretList returns secret names without values.
func (a *App) SecretList(ctx context.Context) ([]string, error) {
	if err := a.requireSecrets(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return a.deps.Secrets.Names(ctx)
}

// SecretRemove deletes a secret from the OS secret store.
func (a *App) SecretRemove(ctx context.Context, name string) error {
	if err := a.requireSecrets(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return a.deps.Secrets.Delete(ctx, strings.TrimSpace(name))
}

func (a *App) requireSecrets() error {
	if a.deps.Secrets == nil {
		return fmt.Errorf("%w: secret store is not available", domain.ErrUnsupported)
	}
	return nil
}
