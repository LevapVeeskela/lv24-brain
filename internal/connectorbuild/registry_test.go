package connectorbuild

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type stubSource struct{ descriptor domain.SourceDescriptor }

func (s stubSource) Descriptor() domain.SourceDescriptor { return s.descriptor }
func (s stubSource) Check(context.Context) error         { return nil }
func (s stubSource) Read(context.Context, ports.ReadRequest) (ports.ReadBatch, error) {
	return ports.ReadBatch{}, nil
}

func stubDescriptor(provider string) domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:       provider,
		DisplayName:    provider,
		Capabilities:   domain.Capabilities{AuthModes: []domain.AuthMode{domain.AuthNone}},
		DefaultPublish: domain.PublishShared,
	}
}

func stubFactory(conn domain.Connection, _ Deps) (ports.Source, error) {
	return stubSource{descriptor: stubDescriptor(conn.Provider)}, nil
}

func TestRegistryDescriptorsSortedByProvider(t *testing.T) {
	// Arrange
	registry := NewRegistry()
	for _, provider := range []string{"slack", "jira", "notion"} {
		if err := registry.Register(stubDescriptor(provider), stubFactory); err != nil {
			t.Fatal(err)
		}
	}

	// Act
	providers := registry.Providers()

	// Assert
	if !slices.Equal(providers, []string{"jira", "notion", "slack"}) {
		t.Fatalf("Providers() = %v", providers)
	}
}

func TestRegistryRejectsDuplicatesInvalidAndNilFactory(t *testing.T) {
	// Arrange
	registry := NewRegistry()
	if err := registry.Register(stubDescriptor("jira"), stubFactory); err != nil {
		t.Fatal(err)
	}

	// Act
	duplicate := registry.Register(stubDescriptor("jira"), stubFactory)
	invalid := registry.Register(stubDescriptor("Jira"), stubFactory)
	nilFactory := registry.Register(stubDescriptor("linear"), nil)

	// Assert
	if !errors.Is(duplicate, domain.ErrConflict) {
		t.Fatalf("duplicate = %v", duplicate)
	}
	if !errors.Is(invalid, domain.ErrValidation) || !errors.Is(nilFactory, domain.ErrValidation) {
		t.Fatalf("invalid = %v, nilFactory = %v", invalid, nilFactory)
	}
}

func TestRegistryUnknownProvider(t *testing.T) {
	// Arrange
	registry := Builtin()

	// Act
	_, lookupErr := registry.Lookup("bitbucket")
	_, buildErr := registry.Build(domain.Connection{ID: "bb", Provider: "bitbucket"}, Deps{})

	// Assert
	if !errors.Is(lookupErr, domain.ErrNotFound) || !errors.Is(buildErr, domain.ErrNotFound) {
		t.Fatalf("Lookup = %v, Build = %v", lookupErr, buildErr)
	}
}

func TestRegistryDescriptorsAreCopies(t *testing.T) {
	// Arrange
	registry := Builtin()

	// Act
	registry.Descriptors()[0].ConfigFields[0].Key = "mutated"
	entry, err := registry.Lookup(registry.Providers()[0])

	// Assert
	if err != nil || entry.Descriptor.ConfigFields[0].Key == "mutated" {
		t.Fatalf("registry state mutated through a copy: %+v, %v", entry.Descriptor, err)
	}
}

func TestRegistryBuildRejectsMismatchedFactory(t *testing.T) {
	// Arrange
	registry := NewRegistry()
	wrong := func(domain.Connection, Deps) (ports.Source, error) {
		return stubSource{descriptor: stubDescriptor("other")}, nil
	}
	if err := registry.Register(stubDescriptor("jira"), wrong); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := registry.Build(domain.Connection{ID: "jira-1", Provider: "jira"}, Deps{})

	// Assert
	if err == nil {
		t.Fatal("Build accepted a source reporting another provider")
	}
}

func TestBuiltinRegistersLegacyProviders(t *testing.T) {
	// Arrange
	registry := Builtin()

	// Act
	providers := registry.Providers()

	// Assert
	if !slices.Equal(providers, []string{"github", "gitlab", "imap"}) {
		t.Fatalf("Providers() = %v", providers)
	}
	imapEntry, _ := registry.Lookup("imap")
	if imapEntry.Descriptor.DefaultPublish != domain.PublishLocalOnly {
		t.Fatalf("imap default publish = %q", imapEntry.Descriptor.DefaultPublish)
	}
}

func TestBuiltinBuildsLegacySources(t *testing.T) {
	deps := Deps{Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{}, Now: func() time.Time { return time.Unix(0, 0) }}
	cases := map[string]domain.Connection{
		"github": {ID: "gh", Provider: "github", Settings: map[string]string{"repos": "acme/widgets"}},
		"gitlab": {ID: "gl", Provider: "gitlab", BaseURL: "https://gitlab.example.com/api/v4", Settings: map[string]string{"projects": "acme/api"}},
		"imap":   {ID: "mail", Provider: "imap", Account: "work", BaseURL: "imaps://imap.example.com", Settings: map[string]string{"username": "me"}},
	}
	for name, conn := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			source, err := Builtin().Build(conn, deps)

			// Assert
			if err != nil {
				t.Fatalf("Build() = %v", err)
			}
			legacy, ok := source.(*LegacySource)
			if !ok || legacy.Connector().Name() != name {
				t.Fatalf("source = %T, want *LegacySource for %s", source, name)
			}
		})
	}
}

func TestBuiltinBuildValidatesSettingsAndDeps(t *testing.T) {
	// Arrange
	registry := Builtin()
	deps := Deps{Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{}}
	missingRepos := domain.Connection{ID: "gh", Provider: "github"}
	secretInSettings := domain.Connection{ID: "gh", Provider: "github", Settings: map[string]string{"repos": "a/b", "token": "ghp_x"}}
	plainIMAP := domain.Connection{ID: "m", Provider: "imap", Account: "w", BaseURL: "imap://host", Settings: map[string]string{"username": "me"}}
	noSink := domain.Connection{ID: "gh", Provider: "github", Settings: map[string]string{"repos": "a/b"}}

	// Act / Assert
	for name, tc := range map[string]struct {
		conn domain.Connection
		deps Deps
	}{
		"missing repos":      {missingRepos, deps},
		"secret in settings": {secretInSettings, deps},
		"plain imap":         {plainIMAP, deps},
		"no sink":            {noSink, Deps{Secrets: fakeSecrets{}}},
	} {
		if _, err := registry.Build(tc.conn, tc.deps); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: Build() = %v, want validation error", name, err)
		}
	}
}

type fakeConnector struct {
	run  domain.ConnectorRun
	err  error
	opts ports.ConnectorOptions
}

func (f *fakeConnector) Name() string { return "github" }
func (f *fakeConnector) Run(_ context.Context, opts ports.ConnectorOptions) (domain.ConnectorRun, error) {
	f.opts = opts
	return f.run, f.err
}

func TestLegacySourceRunsConnectorOncePerRead(t *testing.T) {
	// Arrange
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	connector := &fakeConnector{run: domain.ConnectorRun{Source: "github", Status: domain.ConnectorPartial, Counts: map[string]int{"issues": 2}}}
	source, err := NewLegacySource(GitHubDescriptor(), connector)
	if err != nil {
		t.Fatal(err)
	}
	var _ ports.Source = source

	// Act
	batch, readErr := source.Read(context.Background(), ports.ReadRequest{
		Connection: domain.Connection{ID: "gh", Provider: "github"},
		Since:      &since,
		Limit:      7,
	})
	run, ok := source.LastRun()

	// Assert
	if readErr != nil {
		t.Fatalf("Read() = %v", readErr)
	}
	if batch.More || len(batch.Items) != 0 || len(batch.Checkpoints) != 0 {
		t.Fatalf("legacy batch = %+v, want empty final batch", batch)
	}
	if connector.opts.Limit != 7 || connector.opts.Since == nil || !connector.opts.Since.Equal(since) {
		t.Fatalf("options = %+v", connector.opts)
	}
	if !ok || run.Status != domain.ConnectorPartial || run.ConnectionID != "gh" || run.Counts["issues"] != 2 {
		t.Fatalf("LastRun = %+v, %v", run, ok)
	}
}

func TestLegacySourceReportsFailures(t *testing.T) {
	// Arrange
	failed := &fakeConnector{run: domain.ConnectorRun{Source: "github", Status: domain.ConnectorFailed, Error: "token rejected"}}
	broken := &fakeConnector{err: errors.New("boom")}
	failedSource, _ := NewLegacySource(GitHubDescriptor(), failed)
	brokenSource, _ := NewLegacySource(GitHubDescriptor(), broken)

	// Act
	_, failedErr := failedSource.Read(context.Background(), ports.ReadRequest{})
	_, brokenErr := brokenSource.Read(context.Background(), ports.ReadRequest{})
	checkErr := failedSource.Check(context.Background())

	// Assert
	if failedErr == nil || failedErr.Error() != "token rejected" {
		t.Fatalf("failed run error = %v", failedErr)
	}
	if brokenErr == nil || brokenErr.Error() != "boom" {
		t.Fatalf("connector error = %v", brokenErr)
	}
	if !errors.Is(checkErr, domain.ErrUnsupported) {
		t.Fatalf("Check() = %v", checkErr)
	}
}

func TestSecretCredentialsUsesConnectionNamespace(t *testing.T) {
	// Arrange
	credentials := SecretCredentials{Store: fakeSecrets{"conn/gitlab-work/token": "glpat-1", "gitlab/token": "legacy"}}

	// Act
	value, err := credentials.Get(context.Background(), "gitlab-work", "token")
	_, missing := credentials.Get(context.Background(), "gitlab-home", "token")
	_, invalid := credentials.Get(context.Background(), "gitlab-work", "../token")

	// Assert
	if err != nil || value != "glpat-1" {
		t.Fatalf("Get() = %q, %v", value, err)
	}
	if !errors.Is(missing, domain.ErrNotFound) || !errors.Is(invalid, domain.ErrValidation) {
		t.Fatalf("missing = %v, invalid = %v", missing, invalid)
	}
}
