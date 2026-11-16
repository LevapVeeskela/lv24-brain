package agentcontrol

import (
	"context"
	"errors"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/localapi"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestMemoryServiceEditRecordParity(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	note := fixture.note(t, "Cache", "old cache text", domain.Provenance{})

	title, body := "Cache fixed", "zebracache new text"
	detail, err := fixture.service.EditRecord(ctx, localapi.RecordEditRequest{
		ProjectID: "demo", ID: note.ID, Title: &title, Body: &body,
	})
	if err != nil {
		t.Fatalf("EditRecord: %v", err)
	}
	if detail.ID != note.ID || detail.Kind != "note" || detail.Title != title || detail.Body != body {
		t.Fatalf("detail after edit = %+v", detail)
	}
	if detail.Provenance.Reviewed != true {
		t.Fatalf("human edit lost provenance: %+v", detail.Provenance)
	}

	search, err := fixture.service.Search(ctx, localapi.SearchQuery{Text: "zebracache", ProjectID: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Hits) != 1 || search.Hits[0].ID != note.ID || search.Hits[0].Title != title {
		t.Fatalf("search after edit = %+v", search.Hits)
	}

	// The service returns the same detail the GET route would.
	again, err := fixture.service.RecordDetail(ctx, localapi.RecordDetailQuery{
		ProjectID: "demo", ID: note.ID, MaxTokens: localapi.RecordBodyDefaultTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Title != detail.Title || again.Body != detail.Body || !again.UpdatedAt.Equal(detail.UpdatedAt) {
		t.Fatalf("detail parity = %+v vs %+v", again, detail)
	}

	stale := detail.UpdatedAt
	if stale.IsZero() {
		t.Fatalf("detail has no updated_at: %+v", detail)
	}
	nextBody := "second edit"
	if _, err := fixture.service.EditRecord(ctx, localapi.RecordEditRequest{
		ProjectID: "demo", ID: note.ID, Body: &nextBody, ExpectedUpdatedAt: stale,
	}); err != nil {
		t.Fatalf("CAS edit: %v", err)
	}
	_, err = fixture.service.EditRecord(ctx, localapi.RecordEditRequest{
		ProjectID: "demo", ID: note.ID, Body: &nextBody, ExpectedUpdatedAt: stale,
	})
	var conflict *domain.RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale service edit = %v, want RevisionConflictError", err)
	}
}
