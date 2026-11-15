package mcpsource

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// A stdio MCP server is a third-party program that may be untrusted. It must
// not inherit the brain's own credentials, so the child starts from a minimal
// environment: a fixed allowlist of runtime variables plus the variables the
// connection explicitly configures.

const (
	// maximumChildEnvEntries bounds an explicitly configured environment.
	maximumChildEnvEntries = 32
	// maximumChildEnvBytes bounds the raw JSON stored in the credential store.
	maximumChildEnvBytes = 16 << 10
	// maximumChildEnvValue bounds one value.
	maximumChildEnvValue = 8 << 10
	// maximumChildEnvName bounds one variable name.
	maximumChildEnvName = 128
)

// childEnvNames are the environment variables a stdio server inherits. They
// carry only locations and locale, never credentials.
var childEnvNames = []string{
	"PATH", "HOME", "USERPROFILE",
	"TMP", "TEMP", "TMPDIR",
	"SystemRoot", "ComSpec", "PATHEXT",
	"APPDATA", "LOCALAPPDATA",
	"LANG",
}

// childEnvPrefixes are inherited families. LC_* carries the locale only.
var childEnvPrefixes = []string{"LC_"}

// childEnvDeniedNames are never inherited, even if a future allowlist would
// admit them. They name the brain's own credentials and common cloud secrets.
var childEnvDeniedNames = []string{
	"BRAIN_TOKEN", "BRAIN_DATABASE_URL", "DATABASE_URL",
	"GITHUB_TOKEN", "GITLAB_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_API_KEY",
}

// childEnvDeniedPrefixes are never inherited.
var childEnvDeniedPrefixes = []string{"BRAIN_SECRET_", "AWS_"}

// inheritedEnvName reports whether the parent's variable may pass to the child.
func inheritedEnvName(key string) bool {
	if deniedEnvName(key) {
		return false
	}
	for _, name := range childEnvNames {
		if envKeyEqual(name, key) {
			return true
		}
	}
	for _, prefix := range childEnvPrefixes {
		if hasEnvPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// deniedEnvName reports whether the variable carries a secret the child has no
// business seeing.
func deniedEnvName(key string) bool {
	for _, name := range childEnvDeniedNames {
		if envKeyEqual(name, key) {
			return true
		}
	}
	for _, prefix := range childEnvDeniedPrefixes {
		if hasEnvPrefix(key, prefix) {
			return true
		}
	}
	// The *_SECRET* family: MY_SECRET, APP_SECRET_KEY, SECRET.
	return envKeyEqual("SECRET", key) || strings.Contains(foldEnvKey(key), "_SECRET")
}

// envKeyEqual compares variable names. Windows environment names are
// case-insensitive; elsewhere they are not.
func envKeyEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// hasEnvPrefix reports whether key starts with prefix under the same
// case rules as envKeyEqual.
func hasEnvPrefix(key, prefix string) bool {
	if runtime.GOOS == "windows" {
		return len(key) >= len(prefix) && strings.EqualFold(key[:len(prefix)], prefix)
	}
	return strings.HasPrefix(key, prefix)
}

// foldEnvKey is the deduplication key of a variable name.
func foldEnvKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

// validEnvName reports whether a configured key is a portable variable name.
func validEnvName(key string) bool {
	if key == "" || len(key) > maximumChildEnvName {
		return false
	}
	for index, r := range key {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && index > 0:
		default:
			return false
		}
	}
	return true
}

// parseChildEnv decodes the connection's `env` credential: a JSON object of
// extra variables for the child. It is stored in the local credential store, so
// an environment value never lands in the synced connections catalogue.
func parseChildEnv(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maximumChildEnvBytes {
		return nil, domain.NewValidationError("mcp.env", "must be at most 16384 bytes")
	}
	var decoded map[string]*string
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, domain.NewValidationError("mcp.env", "must be a JSON object of string values")
	}
	if len(decoded) > maximumChildEnvEntries {
		return nil, domain.NewValidationError("mcp.env", "must configure at most 32 variables")
	}
	values := make(map[string]string, len(decoded))
	for key, value := range decoded {
		if value == nil {
			return nil, domain.NewValidationError("mcp.env", "value of "+key+" must be a string")
		}
		if !validEnvName(key) {
			return nil, domain.NewValidationError("mcp.env", "contains an invalid variable name")
		}
		if len(*value) > maximumChildEnvValue || strings.ContainsRune(*value, 0) || strings.ContainsAny(*value, "\r\n") {
			return nil, domain.NewValidationError("mcp.env", "value of "+key+" must be at most 8192 bytes and contain no NUL or newline")
		}
		values[key] = *value
	}
	if len(values) == 0 {
		return nil, nil
	}
	return values, nil
}

// MinimalChildEnv builds the environment of a stdio child server: the
// allowlisted variables of base plus extra, which is validated. base order is
// not preserved; the result is sorted so a test (and a log) is deterministic.
func MinimalChildEnv(base []string, extra map[string]string) ([]string, error) {
	order := make([]string, 0, len(extra)+8)
	values := make(map[string]string, len(extra)+8)
	set := func(key, value string) {
		folded := foldEnvKey(key)
		if _, ok := values[folded]; !ok {
			order = append(order, folded)
		}
		values[folded] = key + "=" + value
	}
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || !inheritedEnvName(key) {
			continue
		}
		set(key, value)
	}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := extra[key]
		if !validEnvName(key) {
			return nil, domain.NewValidationError("mcp.env", "contains an invalid variable name")
		}
		if len(value) > maximumChildEnvValue || strings.ContainsRune(value, 0) || strings.ContainsAny(value, "\r\n") {
			return nil, domain.NewValidationError("mcp.env", "value of "+key+" must be at most 8192 bytes and contain no NUL or newline")
		}
		set(key, value)
	}
	out := make([]string, 0, len(order))
	for _, folded := range order {
		out = append(out, values[folded])
	}
	sort.Strings(out)
	return out, nil
}

// childEnv is the environment of one connection's stdio child. The configured
// variables are read from the local credential store, next to command and
// arguments, so a shared catalogue cannot add an environment on another
// machine.
func (s *Source) childEnv(ctx context.Context) ([]string, error) {
	var extra map[string]string
	if s.credentials != nil {
		raw, err := s.credentials.Get(ctx, s.connection.ID, "env")
		switch {
		case errors.Is(err, domain.ErrNotFound):
		case err != nil:
			return nil, errors.New("mcp stdio: environment could not be retrieved")
		default:
			parsed, parseErr := parseChildEnv(raw)
			if parseErr != nil {
				return nil, parseErr
			}
			extra = parsed
		}
	}
	return MinimalChildEnv(os.Environ(), extra)
}
