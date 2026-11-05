package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const keychainService = "lv24-brain"

// backend is the platform secret-store contract. Implementations never write
// secrets to files and never include values in returned errors.
type backend interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
	Names(ctx context.Context) ([]string, error)
}

func unavailable(reason string) error {
	return fmt.Errorf("secret store unavailable (%s): %w", reason, domain.ErrUnsupported)
}

func notFound(key string) error {
	return fmt.Errorf("secret %q: %w", key, domain.ErrNotFound)
}

func isUnavailable(err error) bool {
	return errors.Is(err, domain.ErrUnsupported)
}

func isNotFound(err error) bool {
	return errors.Is(err, domain.ErrNotFound)
}

func encodeIndex(names []string) (string, error) {
	raw, err := json.Marshal(uniqueStrings(names))
	if err != nil {
		return "", fmt.Errorf("encode secret index: %w", err)
	}
	return string(raw), nil
}

func decodeIndex(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(value), &names); err != nil {
		return nil, fmt.Errorf("decode secret index: %w", err)
	}
	return uniqueStrings(names), nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		seen[value] = struct{}{}
	}
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func updateIndex(names []string, add, remove string) ([]string, error) {
	seen := make(map[string]struct{}, len(names)+1)
	for _, name := range names {
		if name != "" && name != remove {
			seen[name] = struct{}{}
		}
	}
	if add != "" {
		seen[add] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
