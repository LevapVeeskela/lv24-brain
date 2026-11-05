package connectorbuild

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type fakeSecrets map[string]string

func (f fakeSecrets) Get(_ context.Context, key string) (string, error) {
	value, ok := f[key]
	if !ok {
		return "", domain.ErrNotFound
	}
	return value, nil
}

func (f fakeSecrets) Set(context.Context, string, string) error { return nil }
func (f fakeSecrets) Delete(context.Context, string) error      { return nil }
func (f fakeSecrets) Names(context.Context) ([]string, error)   { return nil, nil }

func TestBuildRequiresSecretsAndSink(t *testing.T) {
	_, err := Build(Options{Name: "github", Sink: connectors.DiscardSink{}})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "secrets") {
		t.Fatalf("missing secrets error = %v", err)
	}
	_, err = Build(Options{Name: "github", Secrets: fakeSecrets{}})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "sink") {
		t.Fatalf("missing sink error = %v", err)
	}
}

func TestBuildGitHubUsesEnvironmentRepos(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	connector, err := Build(Options{
		Name: "github", Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if connector.Name() != "github" {
		t.Fatalf("name = %q", connector.Name())
	}
}

func TestBuildGitHubFallsBackToProjectFilter(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "")
	connector, err := Build(Options{
		Name: "github", Projects: []string{"acme/widgets"},
		Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if connector.Name() != "github" {
		t.Fatalf("name = %q", connector.Name())
	}
}

func TestBuildIMAPFailsClosedWithoutEnvironment(t *testing.T) {
	for _, key := range []string{"BRAIN_IMAP_ACCOUNT", "BRAIN_IMAP_ADDRESS", "BRAIN_IMAP_USERNAME"} {
		t.Setenv(key, "")
	}
	_, err := Build(Options{Name: "imap", Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{}})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "BRAIN_IMAP_ACCOUNT") {
		t.Fatalf("Build error = %v", err)
	}
}

func TestBuildRejectsUnknownName(t *testing.T) {
	_, err := Build(Options{Name: "unknown", Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{}})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Build error = %v, want ErrValidation", err)
	}
}
