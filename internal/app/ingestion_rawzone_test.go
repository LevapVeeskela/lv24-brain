package app

import (
	"context"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestIngestionPublishesRawZoneOnlyOnExplicitShared: a provider descriptor that
// allows sharing (github, feeds, jira) does not publish the raw zone by itself.
// Only the connection's own `publish: shared` does, and any other setting
// downgrades a previously shared directory so the host untracks it.
func TestIngestionPublishesRawZoneOnlyOnExplicitShared(t *testing.T) {
	descriptor := domain.SourceDescriptor{
		Provider: "sample", DisplayName: "Sample",
		Capabilities:   domain.Capabilities{AuthModes: []domain.AuthMode{domain.AuthNone}},
		DefaultPublish: domain.PublishShared,
		Streams:        []string{"inbox"},
	}
	newSource := func() *sequenceSource {
		return &sequenceSource{descriptor: descriptor, batches: []ports.ReadBatch{{Items: []domain.Item{{
			Ref:   domain.ItemRef{ExternalID: "1"},
			Kind:  domain.ItemIssue,
			Title: "title",
		}}}}}
	}
	newApplication := func(connection domain.Connection, items *publishAwareItemStore) *Ingestion {
		t.Helper()
		registry := connectorbuild.NewRegistry()
		source := newSource()
		if err := registry.Register(descriptor, func(domain.Connection, connectorbuild.Deps) (ports.Source, error) {
			return source, nil
		}); err != nil {
			t.Fatal(err)
		}
		return &Ingestion{
			Connections: oneConnectionStore{connection: connection},
			Registry:    registry,
			Items:       items, Checkpoints: &recordingCheckpointStore{}, Locker: &fakeLocker{},
			Now: func() time.Time { return ingestionTestTime },
		}
	}
	prepare := func(application *Ingestion, policies *[]domain.PublishPolicy) {
		t.Helper()
		application.PreparePublish = func(_ context.Context, _ string, policy domain.PublishPolicy) error {
			*policies = append(*policies, policy)
			return nil
		}
	}

	implicit := &publishAwareItemStore{recordingItemStore: &recordingItemStore{}, policy: domain.PublishShared}
	var implicitPolicies []domain.PublishPolicy
	unset := newApplication(domain.Connection{ID: "gh-work", Provider: "sample"}, implicit)
	prepare(unset, &implicitPolicies)
	result, err := unset.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: "gh-work"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Publish != domain.PublishLocalOnly || implicit.policy != domain.PublishLocalOnly {
		t.Fatalf("implicit publish = %q, store policy = %q", result.Publish, implicit.policy)
	}
	if len(implicitPolicies) != 1 || implicitPolicies[0] != domain.PublishLocalOnly {
		t.Fatalf("prepare policies = %v", implicitPolicies)
	}

	explicit := &publishAwareItemStore{recordingItemStore: &recordingItemStore{}, policy: domain.PublishLocalOnly}
	var explicitPolicies []domain.PublishPolicy
	shared := newApplication(domain.Connection{ID: "gh-work", Provider: "sample", Publish: domain.PublishShared}, explicit)
	prepare(shared, &explicitPolicies)
	result, err = shared.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: "gh-work"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Publish != domain.PublishShared || !result.PublishChanged || explicit.policy != domain.PublishShared {
		t.Fatalf("explicit publish = %q changed=%v store policy = %q", result.Publish, result.PublishChanged, explicit.policy)
	}
	if len(explicitPolicies) != 1 || explicitPolicies[0] != domain.PublishShared {
		t.Fatalf("prepare policies = %v", explicitPolicies)
	}

	// A host without a machine-local raw zone (server mode) keeps the descriptor
	// sensitivity policy: its items go to the shared database, so "local-only"
	// there means "must not be stored at all".
	serverItems := &publishAwareItemStore{recordingItemStore: &recordingItemStore{}, policy: domain.PublishLocalOnly}
	server := newApplication(domain.Connection{ID: "gh-work", Provider: "sample"}, serverItems)
	server.DisallowLocalOnlyItems = true
	var serverPolicies []domain.PublishPolicy
	prepare(server, &serverPolicies)
	result, err = server.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: "gh-work"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Publish != domain.PublishShared {
		t.Fatalf("server publish = %q, want the descriptor default %q", result.Publish, domain.PublishShared)
	}
}
