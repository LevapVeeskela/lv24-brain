package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
)

// pastRetryWindow moves the clock beyond the window in which an identical
// write counts as a retry, so the second write creates a new record.
func (f reviewFixture) pastRetryWindow() { *f.clock = f.clock.Add(time.Hour) }

func nearDuplicates(findings []app.Finding) []app.Finding {
	var out []app.Finding
	for _, finding := range findings {
		if finding.Kind == app.FindingNearDuplicate {
			out = append(out, finding)
		}
	}
	return out
}

func TestReviewReportsNearDuplicateNotesAndFixLeavesThem(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newReviewFixture(t)
	body := "Индекс поиска пересобирается при каждой синхронизации, старый индекс удаляется после успешной сборки."
	first, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Пересборка индекса", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	f.tick()
	second, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "**Пересборка индекса**", Body: "- " + strings.ReplaceAll(body, "ё", "е") + "!"})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	fixed, fixErr := f.app.FixFindings(ctx, "p", nil)
	after, afterErr := f.app.Findings(ctx, "p")

	// Assert
	dups := nearDuplicates(findings)
	if len(dups) != 1 {
		t.Fatalf("near-duplicate findings = %+v", findings)
	}
	wantPair := first.Note.ID + " and " + second.Note.ID
	if !strings.Contains(dups[0].Message, wantPair) || dups[0].Fixable {
		t.Fatalf("finding = %+v, want %q and not fixable", dups[0], wantPair)
	}
	if fixErr != nil || len(fixed.Applied) != 0 {
		t.Fatalf("FixFindings applied %+v (err %v), want nothing", fixed.Applied, fixErr)
	}
	if afterErr != nil || len(nearDuplicates(after)) != 1 {
		t.Fatalf("after --fix: %+v (err %v), want the duplicate still reported", after, afterErr)
	}
}

func TestReviewIgnoresTrashedDuplicate(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newReviewFixture(t)
	input := app.AttemptInput{ProjectID: "p", Title: "Cache the index in memory", Hypothesis: "keeping the whole index in memory makes search faster on large projects"}
	if _, err := f.app.Attempt(ctx, input); err != nil {
		t.Fatal(err)
	}
	f.pastRetryWindow()
	copied, err := f.app.Attempt(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.app.TrashRecord(ctx, "p", copied.Attempt.ID); err != nil {
		t.Fatal(err)
	}

	// Act
	after, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	if len(nearDuplicates(before)) != 1 || len(nearDuplicates(after)) != 0 {
		t.Fatalf("before trash %+v, after trash %+v", nearDuplicates(before), nearDuplicates(after))
	}
}

func TestDoctorWarnsAboutNearDuplicates(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newReviewFixture(t)
	input := app.DecisionInput{ProjectID: "p", Title: "Index engine: SQLite FTS5", Context: "it ships with the driver and needs no extra service"}
	if _, err := f.app.Decision(ctx, input); err != nil {
		t.Fatal(err)
	}
	f.pastRetryWindow()
	if _, err := f.app.Decision(ctx, input); err != nil {
		t.Fatal(err)
	}

	// Act
	report, err := f.app.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	for _, check := range report.Checks {
		if check.Name == "duplicates" {
			if check.Status != "warn" || !strings.Contains(check.Message, "p 1") {
				t.Fatalf("duplicates check = %+v", check)
			}
			return
		}
	}
	t.Fatalf("no duplicates check in %+v", report.Checks)
}
