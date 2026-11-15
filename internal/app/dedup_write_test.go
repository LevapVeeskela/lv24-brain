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

// duplicateWriter writes one record of a kind and reports its id.
type duplicateWriter struct {
	write func(t *testing.T, app *app.App, title, body string, allowDuplicate bool) (string, error)
}

func duplicateWriters() []struct {
	name   string
	writer duplicateWriter
} {
	return []struct {
		name   string
		writer duplicateWriter
	}{
		{"note", duplicateWriter{write: func(t *testing.T, a *app.App, title, body string, allow bool) (string, error) {
			result, err := a.Note(context.Background(), app.NoteInput{ProjectID: "p", Title: title, Body: body, AllowDuplicate: allow})
			return result.Note.ID, err
		}}},
		{"decision", duplicateWriter{write: func(t *testing.T, a *app.App, title, body string, allow bool) (string, error) {
			result, err := a.Decision(context.Background(), app.DecisionInput{ProjectID: "p", Title: title, Context: body, AllowDuplicate: allow})
			return result.Decision.ID, err
		}}},
		{"attempt", duplicateWriter{write: func(t *testing.T, a *app.App, title, body string, allow bool) (string, error) {
			result, err := a.Attempt(context.Background(), app.AttemptInput{ProjectID: "p", Title: title, Hypothesis: body, AllowDuplicate: allow})
			return result.Attempt.ID, err
		}}},
		{"handoff", duplicateWriter{write: func(t *testing.T, a *app.App, title, body string, allow bool) (string, error) {
			result, err := a.Handoff(context.Background(), app.HandoffInput{ProjectID: "p", Summary: title, NextAction: body, AllowDuplicate: allow})
			return result.Handoff.ID, err
		}}},
	}
}

func TestExactDuplicateIsRefusedAndNamed(t *testing.T) {
	for _, test := range duplicateWriters() {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			f := newReviewFixture(t)
			const title = "Cache invalidation on sync"
			const body = "the cache is invalidated after a successful synchronization, never before"
			first, err := test.writer.write(t, f.app, title, body, false)
			if err != nil || first == "" {
				t.Fatalf("first write = %q, %v", first, err)
			}
			f.pastRetryWindow()

			// Act: same text with different punctuation and casing.
			second, err := test.writer.write(t, f.app, strings.ToUpper(title)+"!", body+".", false)

			// Assert
			var duplicate *domain.DuplicateError
			if !errors.As(err, &duplicate) || !errors.Is(err, domain.ErrDuplicate) {
				t.Fatalf("second write error = %v, want a duplicate error", err)
			}
			if duplicate.ID != first || duplicate.Kind != test.name {
				t.Fatalf("duplicate error = %+v, want kind %s id %s", duplicate, test.name, first)
			}
			if second != "" {
				t.Fatalf("second write returned id %q despite the refusal", second)
			}
			if !strings.Contains(err.Error(), first) {
				t.Fatalf("error %q does not name the existing record", err)
			}
		})
	}
}

func TestExactDuplicateOverrideWritesTheCopy(t *testing.T) {
	for _, test := range duplicateWriters() {
		t.Run(test.name, func(t *testing.T) {
			f := newReviewFixture(t)
			const title = "Cache invalidation on sync"
			const body = "the cache is invalidated after a successful synchronization, never before"
			first, err := test.writer.write(t, f.app, title, body, false)
			if err != nil {
				t.Fatal(err)
			}
			f.pastRetryWindow()

			second, err := test.writer.write(t, f.app, title, body, true)
			if err != nil || second == "" || second == first {
				t.Fatalf("override write = %q, %v, want a new record", second, err)
			}
		})
	}
}

func TestExactDuplicateCheckKeepsSupersedeWorking(t *testing.T) {
	// Arrange: the same text written as a deliberate replacement is history,
	// not a duplicate.
	ctx := context.Background()
	f := newReviewFixture(t)
	first, err := f.app.Decision(ctx, app.DecisionInput{
		ProjectID: "p", Title: "Index engine: SQLite FTS5", Context: "ships with the driver",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.pastRetryWindow()

	// Act
	second, err := f.app.Decision(ctx, app.DecisionInput{
		ProjectID: "p", Title: "Index engine: SQLite FTS5", Context: "ships with the driver",
		Supersedes: first.Decision.ID,
	})

	// Assert
	if err != nil {
		t.Fatalf("supersede write error = %v", err)
	}
	old, err := f.app.GetRecord(ctx, "p", first.Decision.ID)
	if err != nil || old.Validity().SupersededBy != second.Decision.ID {
		t.Fatalf("old decision superseded_by = %q, %v", old.Validity().SupersededBy, err)
	}
}

func clusterWith(clusters []app.DuplicateCluster, ids ...string) (app.DuplicateCluster, bool) {
	for _, cluster := range clusters {
		found := 0
		for _, member := range cluster.Members {
			for _, id := range ids {
				if member.ID == id {
					found++
				}
			}
		}
		if found == len(ids) {
			return cluster, true
		}
	}
	return app.DuplicateCluster{}, false
}

func TestDuplicatesReportsExactAndNearClusters(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newReviewFixture(t)
	exact, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Release checklist", Body: "tag the release, publish the notes, then push the mirror"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Release checklist ", Body: "Tag the release; publish the notes; then push the mirror!", AllowDuplicate: true}); err != nil {
		t.Fatal(err)
	}
	f.pastRetryWindow()
	nearA, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Index rebuild order", Body: "build the new index write it to disk swap the pointer remove the old index rebuild the schema"})
	if err != nil {
		t.Fatal(err)
	}
	f.pastRetryWindow()
	nearB, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Index rebuild order", Body: "remove the old index rebuild the schema build the new index write it to disk swap the pointer"})
	if err != nil {
		t.Fatal(err)
	}
	f.pastRetryWindow()
	unrelatedA, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Publishing", Body: "publish the npm package through trusted publishing with oidc tokens and provenance"})
	if err != nil {
		t.Fatal(err)
	}
	f.pastRetryWindow()
	unrelatedB, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Search", Body: "rebuild the sqlite search index when the schema version changes on disk again"})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	report, err := f.app.Duplicates(ctx, app.DuplicateOptions{ProjectID: "p"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	exactCluster, ok := clusterWith(report.Clusters, exact.Note.ID)
	if !ok || exactCluster.Reason != app.DuplicateReasonExact || len(exactCluster.Members) != 2 {
		t.Fatalf("exact cluster = %+v (ok %v), want both copies", exactCluster, ok)
	}
	nearCluster, ok := clusterWith(report.Clusters, nearA.Note.ID, nearB.Note.ID)
	if !ok || nearCluster.Reason != app.DuplicateReasonSimhash {
		t.Fatalf("near cluster = %+v (ok %v), want a simhash match", nearCluster, ok)
	}
	if _, ok := clusterWith(report.Clusters, unrelatedA.Note.ID, unrelatedB.Note.ID); ok {
		t.Fatalf("unrelated notes clustered: %+v", report.Clusters)
	}
	if report.Exact != 1 || report.Near < 1 {
		t.Fatalf("report counts exact=%d near=%d", report.Exact, report.Near)
	}
}

func TestDuplicatesNeverMergesOrTrashes(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	first, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "One thing", Body: "the same statement written twice on purpose"})
	if err != nil {
		t.Fatal(err)
	}
	f.pastRetryWindow()
	second, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "One thing", Body: "the same statement written twice on purpose", AllowDuplicate: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Duplicates(ctx, app.DuplicateOptions{ProjectID: "p"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.Note.ID, second.Note.ID} {
		if _, err := f.app.GetRecord(ctx, "p", id); err != nil {
			t.Fatalf("record %s disappeared after listing duplicates: %v", id, err)
		}
	}
}

func TestDoctorCountsExactAndNearDuplicates(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	if _, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Same note", Body: "identical body for the doctor count"}); err != nil {
		t.Fatal(err)
	}
	f.pastRetryWindow()
	if _, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Same note", Body: "identical body for the doctor count", AllowDuplicate: true}); err != nil {
		t.Fatal(err)
	}
	report, err := f.app.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range report.Checks {
		if check.Name != "duplicates" {
			continue
		}
		if check.Status != "warn" || !strings.Contains(check.Message, "2 exact") || !strings.Contains(check.Message, "p 1") {
			t.Fatalf("duplicates check = %+v", check)
		}
		return
	}
	t.Fatalf("no duplicates check in %+v", report.Checks)
}

func TestDuplicateRefusalSurvivesTimeTravel(t *testing.T) {
	// The retry window is about a repeated call, not duplicates: a copy months
	// later is still refused, and the error still names the original.
	ctx := context.Background()
	f := newReviewFixture(t)
	if _, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "session summary", NextAction: "ship"}); err != nil {
		t.Fatal(err)
	}
	*f.clock = f.clock.Add(365 * 24 * time.Hour)
	_, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "session summary", NextAction: "ship"})
	if !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("late duplicate error = %v", err)
	}
}
