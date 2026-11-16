package app_test

import (
	"context"
	"slices"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestRecordSummariesAndEditExposeNoteTags(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	note, err := fixture.app.Note(ctx, app.NoteInput{
		ProjectID: "demo", Title: "Retry budget", Body: "Cap retries at three.",
		Tags: []string{"ops", "release"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ops", "release"}
	if !slices.Equal(note.Note.Tags, want) {
		t.Fatalf("stored tags = %#v, want %#v", note.Note.Tags, want)
	}

	summaries, err := fixture.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "demo", Kinds: []string{"note"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || !slices.Equal(summaries[0].Tags, want) {
		t.Fatalf("note summaries = %+v", summaries)
	}

	decision, err := fixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "demo", Title: "Adopt the retry budget", Context: "Bounded retries.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary := (app.Record{Decision: &decision.Decision}).Summary(); len(summary.Tags) != 0 {
		t.Fatalf("decision tags = %#v, want none", summary.Tags)
	}

	tags := []string{"release", "postmortem"}
	edited, err := fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "demo", ID: note.Note.ID, Tags: &tags,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(edited.Record.Note.Tags, tags) {
		t.Fatalf("edited tags = %#v, want %#v", edited.Record.Note.Tags, tags)
	}

	read, err := fixture.app.GetRecord(ctx, "demo", note.Note.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(read.Summary().Tags, tags) {
		t.Fatalf("read tags = %#v, want %#v", read.Summary().Tags, tags)
	}
}
