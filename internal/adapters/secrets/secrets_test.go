package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type fakeBackend struct {
	values map[string]string
	err    error
}

func newFakeBackend(values map[string]string) *fakeBackend {
	if values == nil {
		values = map[string]string{}
	}
	return &fakeBackend{values: values}
}

func (f *fakeBackend) Get(ctx context.Context, key string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	value, ok := f.values[key]
	if !ok {
		return "", notFound(key)
	}
	return value, nil
}

func (f *fakeBackend) Set(ctx context.Context, key, value string) error {
	if f.err != nil {
		return f.err
	}
	f.values[key] = value
	return nil
}

func (f *fakeBackend) Delete(ctx context.Context, key string) error {
	if f.err != nil {
		return f.err
	}
	delete(f.values, key)
	return nil
}

func (f *fakeBackend) Names(ctx context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	names := make([]string, 0, len(f.values))
	for name := range f.values {
		names = append(names, name)
	}
	return uniqueStrings(names), nil
}

func TestNormalizeKey(t *testing.T) {
	cases := map[string]string{
		"github/token":      "GITHUB_TOKEN",
		"GitLab.PAT":        "GITLAB_PAT",
		" imap/acct/pass ":  "IMAP_ACCT_PASS",
		"already_UPPER-key": "ALREADY_UPPER_KEY",
	}
	for input, want := range cases {
		got, err := NormalizeKey(input)
		if err != nil || got != want {
			t.Fatalf("NormalizeKey(%q) = (%q, %v), want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "   ", "bad key", "РєР»СЋС‡", indexKey, "___"} {
		if _, err := NormalizeKey(input); err == nil {
			t.Fatalf("NormalizeKey(%q) = nil error", input)
		}
	}
}

func TestEnvTakesPrecedenceOverBackend(t *testing.T) {
	backend := newFakeBackend(map[string]string{"GITHUB_TOKEN": "from-store"})
	store := New(Options{
		Backend:   backend,
		LookupEnv: lookup(map[string]string{"BRAIN_SECRET_GITHUB_TOKEN": "from-env"}),
		Environ:   environ(map[string]string{"BRAIN_SECRET_GITHUB_TOKEN": "from-env"}),
	})
	value, err := store.Get(context.Background(), "github/token")
	if err != nil || value != "from-env" {
		t.Fatalf("Get = (%q, %v)", value, err)
	}
}

func TestSetAndDeleteTouchOnlyBackend(t *testing.T) {
	backend := newFakeBackend(nil)
	store := New(Options{
		Backend:   backend,
		LookupEnv: lookup(nil),
		Environ:   environ(nil),
	})
	ctx := context.Background()
	if err := store.Set(ctx, "github/token", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if backend.values["GITHUB_TOKEN"] != "value" {
		t.Fatalf("backend = %#v", backend.values)
	}
	value, err := store.Get(ctx, "GitHub/Token")
	if err != nil || value != "value" {
		t.Fatalf("Get = (%q, %v)", value, err)
	}
	if err := store.Delete(ctx, "github/token"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "github/token"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete = %v", err)
	}
}

func TestSetAndDeleteFailForEnvironmentSecret(t *testing.T) {
	store := New(Options{
		Backend:   newFakeBackend(nil),
		LookupEnv: lookup(map[string]string{"BRAIN_SECRET_GITHUB_TOKEN": "env"}),
		Environ:   environ(map[string]string{"BRAIN_SECRET_GITHUB_TOKEN": "env"}),
	})
	ctx := context.Background()
	if err := store.Set(ctx, "github/token", "new"); err == nil {
		t.Fatal("Set over env = nil error")
	}
	if err := store.Delete(ctx, "github/token"); err == nil {
		t.Fatal("Delete over env = nil error")
	}
}

func TestNamesMergesEnvironmentAndBackend(t *testing.T) {
	backend := newFakeBackend(map[string]string{"GITLAB_TOKEN": "a", "IMAP_PASSWORD": "b"})
	store := New(Options{
		Backend:   backend,
		LookupEnv: lookup(map[string]string{"BRAIN_SECRET_GITHUB_TOKEN": "env"}),
		Environ:   environ(map[string]string{"BRAIN_SECRET_GITHUB_TOKEN": "env"}),
	})
	names, err := store.Names(context.Background())
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	want := []string{"GITHUB_TOKEN", "GITLAB_TOKEN", "IMAP_PASSWORD"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("Names = %v, want %v", names, want)
	}
}

func TestNamesStillReturnsEnvironmentWhenBackendUnavailable(t *testing.T) {
	store := New(Options{
		Backend:   &fakeBackend{err: unavailable("no store")},
		LookupEnv: lookup(map[string]string{"BRAIN_SECRET_GITHUB_TOKEN": "env"}),
		Environ:   environ(map[string]string{"BRAIN_SECRET_GITHUB_TOKEN": "env"}),
	})
	names, err := store.Names(context.Background())
	if err != nil || len(names) != 1 || names[0] != "GITHUB_TOKEN" {
		t.Fatalf("Names = (%v, %v)", names, err)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	store := New(Options{
		Backend:   newFakeBackend(nil),
		LookupEnv: lookup(nil),
		Environ:   environ(nil),
	})
	_, err := store.Get(context.Background(), "github/token")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	if strings.Count(err.Error(), `secret "GITHUB_TOKEN"`) != 1 {
		t.Fatalf("missing-secret error repeats the key: %v", err)
	}
}

func TestEmptyEnvironmentValueFallsThrough(t *testing.T) {
	backend := newFakeBackend(map[string]string{"GITHUB_TOKEN": "from-store"})
	store := New(Options{
		Backend:   backend,
		LookupEnv: lookup(map[string]string{"BRAIN_SECRET_GITHUB_TOKEN": ""}),
		Environ:   environ(map[string]string{"BRAIN_SECRET_GITHUB_TOKEN": ""}),
	})
	value, err := store.Get(context.Background(), "github/token")
	if err != nil || value != "from-store" {
		t.Fatalf("Get = (%q, %v)", value, err)
	}
}

func TestErrorsDoNotContainSecretValues(t *testing.T) {
	secret := "super-secret-value"
	backend := newFakeBackend(nil)
	store := New(Options{Backend: backend, LookupEnv: lookup(nil), Environ: environ(nil)})
	err := store.Set(context.Background(), "github/token", secret)
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
	}
	_ = store.Delete(context.Background(), "github/token")
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func environ(values map[string]string) func() []string {
	return func() []string {
		out := make([]string, 0, len(values))
		for key, value := range values {
			out = append(out, key+"="+value)
		}
		return out
	}
}

var _ ports.SecretStore = (*Store)(nil)
