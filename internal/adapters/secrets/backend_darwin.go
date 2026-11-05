//go:build darwin

package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// platformBackend stores generic passwords in the login keychain. The small
// name index lives in the same keychain item namespace, never in a file.
type platformBackend struct{}

func newPlatformBackend() backend {
	return platformBackend{}
}

func (platformBackend) Get(ctx context.Context, key string) (string, error) {
	value, err := securityGet(ctx, key)
	if err != nil {
		return "", err
	}
	return value, nil
}

func (platformBackend) Set(ctx context.Context, key, value string) error {
	if err := securitySet(ctx, key, value); err != nil {
		return err
	}
	names, err := securityNames(ctx)
	if err != nil {
		return err
	}
	updated, err := updateIndex(names, key, "")
	if err != nil {
		return err
	}
	encoded, err := encodeIndex(updated)
	if err != nil {
		return err
	}
	return securitySet(ctx, indexKey, encoded)
}

func (platformBackend) Delete(ctx context.Context, key string) error {
	if err := securityDelete(ctx, key); err != nil {
		return err
	}
	names, err := securityNames(ctx)
	if err != nil {
		return err
	}
	updated, err := updateIndex(names, "", key)
	if err != nil {
		return err
	}
	encoded, err := encodeIndex(updated)
	if err != nil {
		return err
	}
	return securitySet(ctx, indexKey, encoded)
}

func (platformBackend) Names(ctx context.Context) ([]string, error) {
	return securityNames(ctx)
}

func securityGet(ctx context.Context, key string) (string, error) {
	out, err := exec.CommandContext(ctx, "security",
		"find-generic-password", "-s", keychainService, "-a", key, "-w",
	).Output()
	if err != nil {
		return "", classifySecurityError(err, key)
	}
	return string(bytes.TrimSuffix(out, []byte("\n"))), nil
}

func securitySet(ctx context.Context, key, value string) error {
	_, err := exec.CommandContext(ctx, "security",
		"add-generic-password", "-U", "-s", keychainService, "-a", key, "-w", value,
	).Output()
	if err != nil {
		return classifySecurityError(err, key)
	}
	return nil
}

func securityDelete(ctx context.Context, key string) error {
	_, err := exec.CommandContext(ctx, "security",
		"delete-generic-password", "-s", keychainService, "-a", key,
	).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
			return nil
		}
		return classifySecurityError(err, key)
	}
	return nil
}

func securityNames(ctx context.Context) ([]string, error) {
	value, err := securityGet(ctx, indexKey)
	if err != nil {
		if isNotFound(err) {
			return []string{}, nil
		}
		return nil, err
	}
	return decodeIndex(value)
}

func classifySecurityError(err error, key string) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
		return notFound(key)
	}
	if errors.Is(err, exec.ErrNotFound) {
		return unavailable("macOS security command not found")
	}
	if strings.Contains(err.Error(), "interaction is not allowed") {
		return unavailable("macOS keychain is locked")
	}
	return fmt.Errorf("macOS keychain: %w", err)
}
