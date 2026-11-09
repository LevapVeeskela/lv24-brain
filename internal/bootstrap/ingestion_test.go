package bootstrap_test

import (
	"context"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type wiringSource struct{}

func (wiringSource) Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{Provider: "fixture", DisplayName: "Fixture", DefaultPublish: domain.PublishShared, Capabilities: domain.Capabilities{Backfill: true, AuthModes: []domain.AuthMode{domain.AuthNone}}}
}
func (wiringSource) Check(context.Context) error { return nil }
func (wiringSource) Read(_ context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	return ports.ReadBatch{Items: []domain.Item{{Ref: domain.ItemRef{ConnectionID: req.Connection.ID, ExternalID: "one"}, Kind: domain.ItemDocument, Title: "Wiring fixture", Body: "searchable ingestion"}}}, nil
}

func TestBuildIngestionStoresIndexesAndRecords(t *testing.T) {
	cfg := config.Default()
	cfg.Mode, cfg.DataDir = config.ModeLocal, t.TempDir()
	store, err := fsstore.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := bootstrap.BuildIngestion(cfg, bootstrap.Options{Secrets: &fakeSecretStore{}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	host := runtime.Ingestion
	if err := host.Registry.Register(wiringSource{}.Descriptor(), func(domain.Connection, connectorbuild.Deps) (ports.Source, error) { return wiringSource{}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := host.Connections.Create(context.Background(), domain.Connection{ID: "work", Provider: "fixture"}); err != nil {
		t.Fatal(err)
	}
	result, err := host.RunConnection(context.Background(), app.RunConnectionOptions{ConnectionID: "work"})
	if err != nil || result.Run.Status != domain.ConnectorSuccess {
		t.Fatalf("%+v %v", result, err)
	}
	item, err := store.Items().Get(context.Background(), domain.ItemRef{ConnectionID: "work", ExternalID: "one"})
	if err != nil || item.Body != "searchable ingestion" {
		t.Fatalf("%+v %v", item, err)
	}
	runs, err := store.ListConnectorRuns(context.Background(), "fixture", 10)
	if err != nil || len(runs) != 1 || runs[0].Stats == nil || runs[0].Stats.Upserted != 1 || runs[0].ConnectionID != "work" {
		t.Fatalf("%+v %v", runs, err)
	}
	if result.Run.Counts[app.CountIndexed] != 1 {
		t.Fatalf("index not wired: %+v", result)
	}
}
