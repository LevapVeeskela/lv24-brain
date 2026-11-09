package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type cliItemSource struct{}

func (cliItemSource) Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{Provider: "fixture", DisplayName: "Fixture", DefaultPublish: domain.PublishLocalOnly, Capabilities: domain.Capabilities{Backfill: true, AuthModes: []domain.AuthMode{domain.AuthNone}}}
}
func (cliItemSource) Check(context.Context) error { return nil }
func (cliItemSource) Read(_ context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	return ports.ReadBatch{Items: []domain.Item{{Ref: domain.ItemRef{ConnectionID: req.Connection.ID, ExternalID: "one"}, Kind: domain.ItemDocument, Title: "CLI persisted raw item"}}}, nil
}

func TestConnectionRunAndAllUseConfiguredCatalogue(t *testing.T) {
	path, dataDir := connectionConfig(t, config.ModeLocal)
	store, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(t.Context(), domain.Connection{ID: "work", Provider: "fixture"}); err != nil {
		t.Fatal(err)
	}
	previous := buildIngestion
	buildIngestion = func(cfg config.BrainConfig, opts bootstrap.Options) (*bootstrap.IngestionRuntime, error) {
		opts.Secrets = newCLISecretStore()
		runtime, err := bootstrap.BuildIngestion(cfg, opts)
		if err != nil {
			return nil, err
		}
		err = runtime.Ingestion.Registry.Register(cliItemSource{}.Descriptor(), func(domain.Connection, connectorbuild.Deps) (ports.Source, error) { return cliItemSource{}, nil })
		if err != nil {
			runtime.Close()
			return nil, err
		}
		return runtime, nil
	}
	t.Cleanup(func() { buildIngestion = previous })
	code, out, errOut := runForTest(t, "connection", "run", "work", "--json", "--config", path)
	if code != 0 || !strings.Contains(out, `"connectionId": "work"`) {
		t.Fatalf("run %d %s %s", code, out, errOut)
	}
	code, out, errOut = runForTest(t, "connector", "run", "--all", "--config", path)
	if code != 0 || !strings.Contains(out, "== work") {
		t.Fatalf("all %d %s %s", code, out, errOut)
	}
	// A non-empty catalogue must win outright: no legacy github/gitlab plans
	// and no Markdown dumps from the legacy path.
	if strings.Contains(out, "== github") || strings.Contains(out, "== gitlab") {
		t.Fatalf("--all ran legacy targets with a non-empty catalogue: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "github")); !os.IsNotExist(err) {
		t.Fatalf("--all wrote a legacy GitHub dump: %v", err)
	}
	raw, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Items().Get(t.Context(), domain.ItemRef{ConnectionID: "work", ExternalID: "one"}); err != nil {
		t.Fatal(err)
	}
	runs, err := raw.ListConnectorRuns(t.Context(), "fixture", 10)
	if err != nil || len(runs) != 2 || runs[0].Stats == nil {
		t.Fatalf("runs %+v %v", runs, err)
	}
}

func TestConnectionRunRejectsInvalidBudget(t *testing.T) {
	path, _ := connectionConfig(t, config.ModeLocal)
	for _, args := range [][]string{
		{"--limit", "-1"},
		{"--since", "yesterday"},
		{"--days", "-1"},
		{"--days", "7", "--since", "2026-11-01T00:00:00Z"},
	} {
		command := append([]string{"connection", "run", "work", "--config", path}, args...)
		code, _, _ := runForTest(t, command...)
		if code != 2 {
			t.Fatalf("%v exit=%d", command, code)
		}
	}
}

type sinceRecorder struct{ since *time.Time }

func (*sinceRecorder) Descriptor() domain.SourceDescriptor { return cliItemSource{}.Descriptor() }
func (*sinceRecorder) Check(context.Context) error         { return nil }
func (r *sinceRecorder) Read(_ context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	r.since = req.Since
	return ports.ReadBatch{}, nil
}

func TestConnectionRunDaysSetsSince(t *testing.T) {
	path, dataDir := connectionConfig(t, config.ModeLocal)
	store, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), domain.Connection{ID: "work", Provider: "fixture"}); err != nil {
		t.Fatal(err)
	}
	recorder := &sinceRecorder{}
	previous := buildIngestion
	buildIngestion = func(cfg config.BrainConfig, opts bootstrap.Options) (*bootstrap.IngestionRuntime, error) {
		opts.Secrets = newCLISecretStore()
		runtime, err := bootstrap.BuildIngestion(cfg, opts)
		if err != nil {
			return nil, err
		}
		if err := runtime.Ingestion.Registry.Register(cliItemSource{}.Descriptor(), func(domain.Connection, connectorbuild.Deps) (ports.Source, error) {
			return recorder, nil
		}); err != nil {
			runtime.Close()
			return nil, err
		}
		return runtime, nil
	}
	t.Cleanup(func() { buildIngestion = previous })

	before := time.Now().UTC().AddDate(0, 0, -7)
	code, out, errOut := runForTest(t, "connection", "run", "work", "--days", "7", "--config", path)
	if code != 0 {
		t.Fatalf("run %d %s %s", code, out, errOut)
	}
	if recorder.since == nil {
		t.Fatal("source did not receive a Since cutoff")
	}
	if recorder.since.Before(before.Add(-time.Minute)) || recorder.since.After(before.Add(time.Minute)) {
		t.Fatalf("Since = %s, want about %s", recorder.since, before)
	}
}
