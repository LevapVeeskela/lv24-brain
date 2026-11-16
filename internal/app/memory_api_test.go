package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// memoryAPIFixture is a local brain used by the local-API use cases.
type memoryAPIFixture struct {
	app     *app.App
	store   *fsstore.Store
	clock   time.Time
	dataDir string
}

func newMemoryAPIFixture(t *testing.T) *memoryAPIFixture {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	clock := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	fixture := &memoryAPIFixture{
		app:     app.New(cfg, app.Dependencies{Store: store, Index: index, Proposals: store, Now: func() time.Time { return clock }}),
		store:   store,
		clock:   clock,
		dataDir: dataDir,
	}
	if _, err := fixture.app.SaveProject(context.Background(), app.ProjectInput{ID: "demo"}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestStateRevisionCAS(t *testing.T) {
	fixture := newMemoryAPIFixture(t)
	ctx := context.Background()

	// No state yet: the revision is the documented "none" sentinel.
	project, state, err := fixture.app.GetState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if project.ID != "demo" || app.StateRevision(state) != "none" {
		t.Fatalf("empty state revision = %q", app.StateRevision(state))
	}

	goal := "ship 1.2"
	state, err = fixture.app.SetStateIfRevision(ctx, app.StateInput{ProjectID: "demo", Goal: &goal}, "none")
	if err != nil {
		t.Fatalf("CAS from none: %v", err)
	}
	first := app.StateRevision(state)
	if first == "" || first == "none" {
		t.Fatalf("revision after write = %q", first)
	}

	// A stale expected revision is refused and names the current one.
	next := "ship 1.2.1"
	_, err = fixture.app.SetStateIfRevision(ctx, app.StateInput{ProjectID: "demo", NextAction: &next}, "none")
	var conflict *app.StateRevisionConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("stale CAS error = %v, want StateRevisionConflict", err)
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflict does not unwrap to ErrConflict: %v", err)
	}
	if conflict.Current != first || conflict.Expected != "none" || conflict.ProjectID != "demo" {
		t.Fatalf("conflict = %+v, want current %q", conflict, first)
	}
	if conflict.Error() == "" {
		t.Fatal("conflict has no message")
	}

	// The same revision succeeds and an empty expected revision stays
	// unconditional.
	if _, err := fixture.app.SetStateIfRevision(ctx, app.StateInput{ProjectID: "demo", NextAction: &next}, first); err != nil {
		t.Fatalf("CAS with current revision: %v", err)
	}
	other := "unconditional"
	if _, err := fixture.app.SetStateIfRevision(ctx, app.StateInput{ProjectID: "demo", NextAction: &other}, ""); err != nil {
		t.Fatalf("unconditional write: %v", err)
	}
	_, state, err = fixture.app.GetState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if state.NextAction != other {
		t.Fatalf("next action = %q", state.NextAction)
	}
	if _, err := fixture.app.SetStateIfRevision(ctx, app.StateInput{ProjectID: "demo"}, first); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty CAS write = %v, want validation", err)
	}
}

func TestRecordSummaryCarriesProvenancePermalinkAndReview(t *testing.T) {
	fixture := newMemoryAPIFixture(t)
	ctx := context.Background()
	result, err := fixture.app.Note(ctx, app.NoteInput{
		ProjectID: "demo", Title: "Quarantined", Body: "external text",
		Provenance: domain.Provenance{Origin: domain.OriginAgent, Reviewed: false, Trust: domain.TrustStated},
	})
	if err != nil {
		t.Fatal(err)
	}
	records, err := fixture.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	record := records[0]
	if record.ID != result.Note.ID {
		t.Fatalf("record = %+v", record)
	}
	if !record.Provenance.Quarantined() || record.Provenance.EffectiveTrust() != domain.TrustStated {
		t.Fatalf("provenance = %+v", record.Provenance)
	}
	if record.ReviewAfter.IsZero() || record.ReviewSource == "" {
		t.Fatalf("review schedule = %+v (%q)", record.ReviewAfter, record.ReviewSource)
	}
	if got := app.RecordPath("demo", record.Kind, record.ID); got != "projects/demo/notes/"+record.ID+".md" {
		t.Fatalf("record path = %q", got)
	}
}

func TestListProposalsFiltersAndHealth(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	connections, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	instance := app.New(cfg, app.Dependencies{
		Store: store, Connections: connections, Items: store.Items(), Proposals: store,
		Now: func() time.Time { return clock },
	})
	if _, err := instance.SaveProject(ctx, app.ProjectInput{ID: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := connections.Create(ctx, domain.Connection{ID: "gh", Provider: "github", Projects: []string{"demo"}, Publish: domain.PublishLocalOnly}); err != nil {
		t.Fatal(err)
	}
	item := domain.Item{
		Ref:        domain.ItemRef{ConnectionID: "gh", ExternalID: "issue-1"},
		ProjectID:  "demo",
		Kind:       "issue",
		Title:      "Adopt SQLite",
		URL:        "https://example.test/issues/1",
		Labels:     []string{"decision"},
		ObservedAt: clock,
	}
	item.Hash = item.ComputeHash()
	if _, err := store.Items().Upsert(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", ProjectID: "demo", All: true, Write: true}); err != nil {
		t.Fatal(err)
	}
	listing, err := instance.ListProposals(ctx, app.ProposalQuery{ProjectID: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if listing.Total != 1 || len(listing.Items) != 1 {
		t.Fatalf("listing = %+v", listing)
	}
	proposal := listing.Items[0]
	if proposal.ProjectID != "demo" || proposal.ExternalID != "issue-1" || proposal.Status != domain.ProposalPending {
		t.Fatalf("proposal = %+v", proposal)
	}
	if !proposal.LocalOnly {
		t.Fatalf("local-only source was not reported: %+v", proposal)
	}
	if listing, err := instance.ListProposals(ctx, app.ProposalQuery{ProjectID: "demo", Statuses: []domain.ProposalStatus{domain.ProposalPromoted}}); err != nil {
		t.Fatal(err)
	} else if listing.Total != 0 || len(listing.Items) != 0 {
		t.Fatalf("status filter = %+v", listing)
	}
	if _, err := instance.ListProposals(ctx, app.ProposalQuery{ProjectID: "demo", Statuses: []domain.ProposalStatus{"wrong"}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid status = %v", err)
	}
	if _, err := instance.ListProposals(ctx, app.ProposalQuery{ProjectID: "missing"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown project = %v", err)
	}
	if items, count, err := instance.PendingProposals(ctx, "demo"); err != nil || len(items) != 1 || count != 1 {
		t.Fatalf("pending = %+v %d %v", items, count, err)
	}

	// A stale or orphaned source needs force; the typed conflict lets the API
	// answer 409 instead of 500.
	if err := connections.Delete(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID, ProjectID: "demo"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("orphan promote = %v, want conflict", err)
	}
	if got, err := instance.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID, ProjectID: "demo", Force: true, Yes: true}); err != nil {
		t.Fatalf("forced promote: %v", err)
	} else if got.Record.ID == "" {
		t.Fatalf("forced promote = %+v", got)
	}
}

// static assertion: the stores used by the agentcontrol memory service keep
// satisfying the optional ports the listing relies on.
var _ ports.ProposalStore = (*fsstore.Store)(nil)
