package agentcontrol

import (
	"context"
	"errors"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/localapi"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestMemoryServiceReviewRecordByIDClearsQuarantine(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	created := fixture.note(t, "Connector note", "external text", domain.Provenance{Origin: domain.OriginConnector})

	before, err := fixture.service.ListRecords(ctx, localapi.RecordQuery{ProjectID: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !before[0].Provenance.Quarantined {
		t.Fatalf("unreviewed connector note is not quarantined: %+v", before[0].Provenance)
	}

	view, err := fixture.service.ReviewRecord(ctx, localapi.RecordReviewRequest{
		ProjectID: "demo", ID: created.ID, Reviewed: true, Trust: "stated",
	})
	if err != nil {
		t.Fatalf("ReviewRecord: %v", err)
	}
	if view.ID != created.ID {
		t.Fatalf("view = %+v", view)
	}
	if !view.Provenance.Reviewed || view.Provenance.Trust != "stated" || view.Provenance.Quarantined {
		t.Fatalf("provenance after review = %+v", view.Provenance)
	}
	if view.UpdatedAt != created.UpdatedAt {
		t.Fatalf("review moved updated_at: %s != %s", view.UpdatedAt, created.UpdatedAt)
	}

	// A repeated review is an idempotent no-op that keeps the record row.
	again, err := fixture.service.ReviewRecord(ctx, localapi.RecordReviewRequest{ProjectID: "demo", ID: created.ID})
	if err != nil {
		t.Fatalf("repeated ReviewRecord: %v", err)
	}
	if again.Provenance.Trust != "stated" || again.UpdatedAt != view.UpdatedAt {
		t.Fatalf("repeated review changed the record: %+v", again)
	}

	// An unknown id is a typed not-found.
	if _, err := fixture.service.ReviewRecord(ctx, localapi.RecordReviewRequest{ProjectID: "demo", ID: "note-missing"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing id = %v, want not found", err)
	}
}

func TestMemoryServiceReviewRecordCAS(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	created := fixture.note(t, "Agent note", "first text", domain.Provenance{Origin: domain.OriginAgent})

	body := "second text"
	edited, err := fixture.app.EditRecord(ctx, app.RecordEdit{ProjectID: "demo", ID: created.ID, Body: &body})
	if err != nil {
		t.Fatalf("EditRecord: %v", err)
	}
	fresh := edited.Record.Summary()

	if _, err := fixture.service.ReviewRecord(ctx, localapi.RecordReviewRequest{
		ProjectID: "demo", ID: created.ID, ExpectedUpdatedAt: created.UpdatedAt,
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale review = %v, want a conflict", err)
	}

	view, err := fixture.service.ReviewRecord(ctx, localapi.RecordReviewRequest{
		ProjectID: "demo", ID: created.ID, ExpectedUpdatedAt: fresh.UpdatedAt,
	})
	if err != nil {
		t.Fatalf("fresh review: %v", err)
	}
	if !view.Provenance.Reviewed {
		t.Fatalf("review did not mark reviewed: %+v", view.Provenance)
	}
}

func TestMemoryServiceReviewStateReturnsRevisionAndProvenance(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	goal := "ship"
	if _, err := fixture.app.SetState(ctx, app.StateInput{ProjectID: "demo", Goal: &goal}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	before, err := fixture.service.State(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}

	view, err := fixture.service.ReviewState(ctx, localapi.StateReviewRequest{
		ProjectID: "demo", Reviewed: true, Trust: "verified", ExpectedRevision: before.Revision,
	})
	if err != nil {
		t.Fatalf("ReviewState: %v", err)
	}
	if view.Revision != before.Revision {
		t.Fatalf("review moved the revision: %q != %q", view.Revision, before.Revision)
	}
	if !view.Provenance.Reviewed || view.Provenance.Trust != "verified" || view.Provenance.Origin == "" {
		t.Fatalf("state provenance = %+v", view.Provenance)
	}

	// A stale expected_revision is the documented 409 conflict.
	if _, err := fixture.service.ReviewState(ctx, localapi.StateReviewRequest{
		ProjectID: "demo", ExpectedRevision: "none",
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale state review = %v, want a conflict", err)
	}

	// A repeated review is an idempotent no-op with the same revision.
	again, err := fixture.service.ReviewState(ctx, localapi.StateReviewRequest{
		ProjectID: "demo", ExpectedRevision: before.Revision,
	})
	if err != nil {
		t.Fatalf("repeated ReviewState: %v", err)
	}
	if again.Revision != before.Revision || !again.Provenance.Reviewed {
		t.Fatalf("repeated review = %+v", again)
	}
}
