package update

import (
	"context"
	"strings"
	"sync"
)

// tokenEnvVars are read, in order, for a GitHub token that lifts the
// unauthenticated API rate limit (60 requests per hour per IP). The release
// repository is public, so a token is optional.
var tokenEnvVars = []string{"BRAIN_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"}

// apiToken resolves the token once per Updater: Options.Token, then the
// environment, then Options.TokenSource. A token GitHub rejected is dropped.
type apiToken struct {
	explicit string
	getenv   func(string) string
	source   func(context.Context) (string, error)

	once     sync.Once
	mu       sync.Mutex
	value    string
	rejected bool
}

func (t *apiToken) get(ctx context.Context) string {
	t.once.Do(func() {
		t.value = strings.TrimSpace(t.explicit)
		for _, name := range tokenEnvVars {
			if t.value != "" {
				break
			}
			t.value = strings.TrimSpace(t.getenv(name))
		}
		if t.value == "" && t.source != nil {
			if value, err := t.source(ctx); err == nil {
				t.value = strings.TrimSpace(value)
			}
		}
	})
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rejected {
		return ""
	}
	return t.value
}

func (t *apiToken) reject() {
	t.mu.Lock()
	t.rejected = true
	t.mu.Unlock()
}
