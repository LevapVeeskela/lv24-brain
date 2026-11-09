package connectorbuild

import (
	"context"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var _ ports.CredentialStore = SecretCredentials{}

// Set stores a connection credential under domain.CredentialKey. The underlying
// SecretStore owns OS keychain access and environment-shadowing policy.
func (s SecretCredentials) Set(ctx context.Context, connectionID, field, value string) error {
	if s.Store == nil {
		return domain.NewValidationError("credentials.store", "is required")
	}
	key, err := domain.CredentialKey(connectionID, field)
	if err != nil {
		return err
	}
	return s.Store.Set(ctx, key, value)
}

// Delete removes only this connection's field; legacy secrets are untouched.
func (s SecretCredentials) Delete(ctx context.Context, connectionID, field string) error {
	if s.Store == nil {
		return domain.NewValidationError("credentials.store", "is required")
	}
	key, err := domain.CredentialKey(connectionID, field)
	if err != nil {
		return err
	}
	return s.Store.Delete(ctx, key)
}
