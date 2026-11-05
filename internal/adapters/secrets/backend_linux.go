//go:build linux

package secrets

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// platformBackend uses the freedesktop Secret Service through secret-tool.
// When secret-tool or the session bus is absent, reads fall through to the
// environment and writes fail closed with ErrUnsupported.
type platformBackend struct{}

func newPlatformBackend() backend {
	return platformBackend{}
}

func (platformBackend) Get(ctx context.Context, key string) (string, error) {
	value, err := secretToolGet(ctx, key)
	if err != nil {
		return "", err
	}
	return value, nil
}

func (platformBackend) Set(ctx context.Context, key, value string) error {
	if err := secretToolSet(ctx, key, value); err != nil {
		return err
	}
	names, err := secretToolNames(ctx)
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
	return secretToolSet(ctx, indexKey, encoded)
}

func (platformBackend) Delete(ctx context.Context, key string) error {
	if err := secretToolDelete(ctx, key); err != nil {
		return err
	}
	names, err := secretToolNames(ctx)
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
	return secretToolSet(ctx, indexKey, encoded)
}

func (platformBackend) Names(ctx context.Context) ([]string, error) {
	return secretToolNames(ctx)
}

func secretToolGet(ctx context.Context, key string) (string, error) {
	out, err := exec.CommandContext(ctx, "secret-tool",
		"lookup", "service", keychainService, "account", key,
	).Output()
	if err != nil {
		return "", classifySecretToolError(err, key, true)
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

func secretToolSet(ctx context.Context, key, value string) error {
	cmd := exec.CommandContext(ctx, "secret-tool",
		"store", "--label="+keychainService,
		"service", keychainService, "account", key,
	)
	cmd.Stdin = strings.NewReader(value)
	if _, err := cmd.Output(); err != nil {
		return classifySecretToolError(err, key, false)
	}
	return nil
}

func secretToolDelete(ctx context.Context, key string) error {
	_, err := exec.CommandContext(ctx, "secret-tool",
		"clear", "service", keychainService, "account", key,
	).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && !stderrMentionsBus(exitErr) {
			return nil
		}
		return classifySecretToolError(err, key, true)
	}
	return nil
}

func secretToolNames(ctx context.Context) ([]string, error) {
	value, err := secretToolGet(ctx, indexKey)
	if err != nil {
		if isNotFound(err) {
			return []string{}, nil
		}
		return nil, err
	}
	return decodeIndex(value)
}

func classifySecretToolError(err error, key string, missingIsNotFound bool) error {
	if errors.Is(err, exec.ErrNotFound) {
		return unavailable("secret-tool not found")
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if stderrMentionsBus(exitErr) {
			return unavailable("Secret Service session bus unavailable")
		}
		if missingIsNotFound && exitErr.ExitCode() == 1 {
			return notFound(key)
		}
	}
	return fmt.Errorf("Secret Service: %w", err)
}

func stderrMentionsBus(err *exec.ExitError) bool {
	text := strings.ToLower(string(err.Stderr))
	return strings.Contains(text, "dbus") ||
		strings.Contains(text, "d-bus") ||
		strings.Contains(text, "secret service") ||
		strings.Contains(text, "cannot autolaunch")
}
