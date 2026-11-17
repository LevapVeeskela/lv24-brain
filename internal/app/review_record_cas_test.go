package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestReviewRecordReviewsByIDAloneAndClearsQuarantine(t *testing.T) {
	ctx := context.Background()
	fixture := newContextFixture(t, true)
	created, err := fixture.app.Note(ctx, app.NoteInput{
		ProjectID:     "p",
		Title:         "Connector note",
		Body:          "external text connectorpayload",
		CreateProject: true,
		Provenance:    domain.Provenance{Origin: domain.OriginConnector},
	})
	if err != nil {
		t.Fatalf("Note: %v", err)
	}

	// The id is unambiguous inside the project: no kind is passed.
	reviewed, err := fixture.app.ReviewRecord(ctx, app.ReviewRecordInput{
		ProjectID: "p", ID: created.Note.ID, Trust: domain.TrustStated,
	})
	if err != nil {
		t.Fatalf("ReviewRecord: %v", err)
	}
	if reviewed.ID != created.Note.ID || reviewed.Provenance.ReviewedState() != true {
		t.Fatalf("reviewed = %+v", reviewed)
	}
	if reviewed.Provenance.Trust != domain.TrustStated {
		t.Fatalf("trust = %q, want stated", reviewed.Provenance.Trust)
	}
	if reviewed.Provenance.Quarantined() {
		t.Fatalf("review did not clear the quarantine: %+v", reviewed.Provenance)
	}
	if reviewed.UpdatedAt != created.Note.UpdatedAt {
		t.Fatalf("review moved updated_at: %s != %s", reviewed.UpdatedAt, created.Note.UpdatedAt)
	}

	after, err := fixture.app.Bootstrap(ctx, app.BootstrapOptions{ProjectID: "p"})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(after.Quarantined) != 0 || strings.Contains(after.Summary, "quarantined:") {
		t.Fatalf("quarantine survived the review: %+v", after.Quarantined)
	}

	// A repeated review is an idempotent no-op.
	again, err := fixture.app.ReviewRecord(ctx, app.ReviewRecordInput{ProjectID: "p", ID: created.Note.ID})
	if err != nil {
		t.Fatalf("repeated ReviewRecord: %v", err)
	}
	if again.UpdatedAt != reviewed.UpdatedAt || again.Provenance.Trust != domain.TrustStated {
		t.Fatalf("repeated review changed the record: %+v", again)
	}
}

func TestReviewRecordCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	fixture := newContextFixture(t, true)
	created, err := fixture.app.Note(ctx, app.NoteInput{
		ProjectID:     "p",
		Title:         "Agent note",
		Body:          "first text",
		CreateProject: true,
		Provenance:    domain.Provenance{Origin: domain.OriginAgent},
	})
	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	stale := created.Note.UpdatedAt

	body := "second text"
	edited, err := fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "p", ID: created.Note.ID, Body: &body,
	})
	if err != nil {
		t.Fatalf("EditRecord: %v", err)
	}
	editedSummary := edited.Record.Summary()
	if editedSummary.UpdatedAt.Equal(stale) {
		t.Fatalf("edit did not move updated_at: %s", editedSummary.UpdatedAt)
	}

	// A stale expected_updated_at is refused before anything is written.
	_, err = fixture.app.ReviewRecord(ctx, app.ReviewRecordInput{
		ProjectID: "p", ID: created.Note.ID, ExpectedUpdatedAt: stale,
	})
	var conflict *domain.RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale review = %v, want a revision conflict", err)
	}
	if conflict.CurrentRevision() != editedSummary.UpdatedAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("conflict revision = %q", conflict.CurrentRevision())
	}

	fresh, err := fixture.app.ReviewRecord(ctx, app.ReviewRecordInput{
		ProjectID: "p", ID: created.Note.ID, ExpectedUpdatedAt: editedSummary.UpdatedAt,
	})
	if err != nil {
		t.Fatalf("fresh review: %v", err)
	}
	if !fresh.Provenance.ReviewedState() {
		t.Fatalf("fresh review did not mark reviewed: %+v", fresh.Provenance)
	}
}

func TestReviewRecordRefusals(t *testing.T) {
	ctx := context.Background()
	fixture := newContextFixture(t, true)
	// Register the project so an unknown record id is a not-found, not a
	// missing project.
	if _, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "p", Body: "seed", CreateProject: true}); err != nil {
		t.Fatalf("seed Note: %v", err)
	}
	if _, err := fixture.app.ReviewRecord(ctx, app.ReviewRecordInput{ProjectID: "p", ID: " "}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty id = %v, want validation", err)
	}
	if _, err := fixture.app.ReviewRecord(ctx, app.ReviewRecordInput{
		ProjectID: "p", ID: "note-x", Trust: domain.Trust("guessed"),
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown trust = %v, want validation", err)
	}
	if _, err := fixture.app.ReviewRecord(ctx, app.ReviewRecordInput{ProjectID: "p", ID: "note-x"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown id = %v, want not found", err)
	}
}

func TestReviewStateCompareAndSwapAndIdempotency(t *testing.T) {
	ctx := context.Background()
	fixture := newContextFixture(t, true)
	if _, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "p", Body: "seed", CreateProject: true}); err != nil {
		t.Fatalf("Note: %v", err)
	}
	goal := "ship"
	// A non-human, unreviewed origin makes the state quarantined so the
	// first review is a real provenance change (changed=true) and the
	// repeated review is an idempotent no-op at the same revision.
	written, err := fixture.app.SetState(ctx, app.StateInput{
		ProjectID:  "p",
		Goal:       &goal,
		Provenance: domain.Provenance{Origin: domain.OriginAgent, Trust: domain.TrustInferred},
	})
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	revision := app.StateRevision(written)
	if written.Provenance.ReviewedState() {
		t.Fatalf("seed state is unexpectedly reviewed: %+v", written.Provenance)
	}

	// A stale expected_revision is refused and writes nothing.
	var staleConflict *app.StateRevisionConflict
	if _, _, err := fixture.app.ReviewState(ctx, app.ReviewStateOptions{
		ProjectID: "p", Trust: domain.TrustVerified, ExpectedRevision: "none",
	}); !errors.As(err, &staleConflict) {
		t.Fatalf("stale state review = %v, want a revision conflict", err)
	} else if staleConflict.Current != revision {
		t.Fatalf("conflict revision = %q, want %q", staleConflict.Current, revision)
	}

	reviewed, changed, err := fixture.app.ReviewState(ctx, app.ReviewStateOptions{
		ProjectID: "p", Trust: domain.TrustVerified, ExpectedRevision: revision,
	})
	if err != nil || !changed {
		t.Fatalf("ReviewState = %+v, changed=%t, err=%v", reviewed, changed, err)
	}
	if !reviewed.Provenance.ReviewedState() || reviewed.Provenance.Trust != domain.TrustVerified {
		t.Fatalf("provenance = %+v", reviewed.Provenance)
	}
	if again := app.StateRevision(reviewed); again != revision {
		t.Fatalf("review moved the revision: %q != %q", again, revision)
	}

	// The review is provenance-only: the same expected_revision keeps passing.
	idempotent, changed, err := fixture.app.ReviewState(ctx, app.ReviewStateOptions{
		ProjectID: "p", ExpectedRevision: revision,
	})
	if err != nil || changed {
		t.Fatalf("repeated ReviewState changed the state: %+v changed=%t err=%v", idempotent, changed, err)
	}
	if _, _, err := fixture.app.ReviewState(ctx, app.ReviewStateOptions{
		ProjectID: "p", Trust: domain.Trust("guessed"),
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown trust = %v, want validation", err)
	}
}
