package app_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// linkedFixture builds the link graph of the golden test:
//
//	rootA (decision, matches the query) -> nA, nB, p2/nCross, missing
//	rootB (decision, matches the query) -> nA, nQuarantined, nSuperseded, rootA
//	rootC (attempt, matches the query)  -> nA
//	nB (note)                           -> nA   (nB is never selected, so its
//	                                              links must not be followed)
//
// nA is therefore referenced by three selected records, nB by one. rootA and
// rootB are selected by the query layer, so they must not come back as linked
// records of each other.
type linkedFixture struct {
	f            contextFixture
	nA           string
	nB           string
	nQuarantined string
	nSuperseded  string
	superseding  string
	nCross       string
	rootA, rootB string
	attemptRoot  string
}

func newLinkedFixture(t *testing.T) linkedFixture {
	t.Helper()
	ctx := context.Background()
	f := newContextFixture(t, true)
	out := linkedFixture{f: f}

	// Other project: a link into it must never widen the answer.
	other, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p2", Body: "cross project target", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	out.nCross = other.Note.ID

	out.nA = f.note(t, "target A unique text")
	out.nB = f.note(t, "target B links: [["+out.nA+"]]")
	out.nQuarantined = quarantinedNote(t, f, "target quarantined")
	out.nSuperseded = f.note(t, "target superseded")
	out.superseding = f.note(t, "target superseding")
	if _, err := f.app.Supersede(ctx, "p", out.nSuperseded, out.superseding); err != nil {
		t.Fatal(err)
	}

	out.rootA = f.decision(t, "Golden root A goldenmarker",
		"goldenmarker links [["+out.nA+"]] [["+out.nB+"]] [[p2/"+out.nCross+"]] [[golden-missing-id]]")
	out.rootB = f.decision(t, "Golden root B goldenmarker",
		"goldenmarker links [["+out.nA+"]] [["+out.nQuarantined+"]] [["+out.nSuperseded+"]] [["+out.rootA+"]]")
	out.attemptRoot = f.attempt(t, "Golden attempt goldenmarker", "goldenmarker links [["+out.nA+"]]")
	return out
}

func quarantinedNote(t *testing.T, f contextFixture, body string) string {
	t.Helper()
	f.advance(time.Minute)
	result, err := f.app.Note(context.Background(), app.NoteInput{
		ProjectID: "p", Body: body, CreateProject: true,
		Provenance: domain.Provenance{Origin: domain.OriginAgent},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Note.ID
}

func linkedSection(t *testing.T, pack app.ContextPack) app.ContextSection {
	t.Helper()
	for _, section := range pack.Sections {
		if section.Name == app.ContextLayerLinked {
			return section
		}
	}
	return app.ContextSection{}
}

func linkedScores(section app.ContextSection) []string {
	out := make([]string, 0, len(section.Records))
	for _, record := range section.Records {
		out = append(out, fmt.Sprintf("%s=%.0f", record.ID, record.Score))
	}
	return out
}

// TestContextPackLinkedGolden pins the exact linked records, their order and
// their reference scores. It is the equivalence guard for the batch link lookup:
// the layer must not change when the links are resolved from one project listing
// instead of one listing per selected record.
func TestContextPackLinkedGolden(t *testing.T) {
	// Arrange
	ctx := context.Background()
	fx := newLinkedFixture(t)

	// Act
	result, err := fx.f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "goldenmarker", TokenBudget: domain.MaxContextBudgetTokens,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	section := linkedSection(t, result.Pack)
	want := []string{fx.nA + "=3", fx.nB + "=1"}
	if got := linkedScores(section); !reflect.DeepEqual(got, want) {
		t.Fatalf("linked = %v, want %v", got, want)
	}
	for _, id := range []string{fx.nQuarantined, fx.nSuperseded, fx.nCross, fx.rootA, fx.rootB, fx.attemptRoot} {
		for _, record := range section.Records {
			if record.ID == id {
				t.Fatalf("linked = %v includes %s", linkedScores(section), id)
			}
		}
	}
	for _, record := range section.Records {
		if record.Title == "" || record.Path == "" || record.Tokens <= 0 {
			t.Fatalf("linked record = %+v, want title, path and tokens", record)
		}
		if !strings.Contains(result.Summary, record.ID) {
			t.Fatalf("summary lacks linked record %s:\n%s", record.ID, result.Summary)
		}
	}
}

func TestContextPackLinkedGoldenIncludeUnreviewed(t *testing.T) {
	// Arrange
	ctx := context.Background()
	fx := newLinkedFixture(t)

	// Act
	result, err := fx.f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "goldenmarker", TokenBudget: domain.MaxContextBudgetTokens,
		IncludeUnreviewed: true,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	// nQuarantined ties with nB on the reference count; the deterministic
	// tie-break keeps the older record first.
	want := []string{fx.nA + "=3", fx.nB + "=1", fx.nQuarantined + "=1"}
	if got := linkedScores(linkedSection(t, result.Pack)); !reflect.DeepEqual(got, want) {
		t.Fatalf("linked = %v, want %v", got, want)
	}
}

func TestContextPackLinkedGoldenIncludeSuperseded(t *testing.T) {
	// Arrange
	ctx := context.Background()
	fx := newLinkedFixture(t)

	// Act
	result, err := fx.f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "goldenmarker", TokenBudget: domain.MaxContextBudgetTokens,
		IncludeSuperseded: true,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	// The superseded note is older than nB, so the score tie keeps nB first.
	want := []string{fx.nA + "=3", fx.nB + "=1", fx.nSuperseded + "=1"}
	if got := linkedScores(linkedSection(t, result.Pack)); !reflect.DeepEqual(got, want) {
		t.Fatalf("linked = %v, want %v", got, want)
	}
}

// TestContextPackLinkedKindsFilter checks that the linked layer keeps only the
// kinds the caller asked for. With notes alone, the two decision roots are not
// selected, so their links are not followed; the attempt that matches the query
// is still reported as a failed attempt and its link to the A note counts.
func TestContextPackLinkedKindsFilter(t *testing.T) {
	// Arrange
	ctx := context.Background()
	fx := newLinkedFixture(t)

	// Act: only notes, so the decisions are neither packed nor used as link
	// sources.
	result, err := fx.f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "goldenmarker", Kinds: []string{"note"},
		TokenBudget: domain.MaxContextBudgetTokens,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	want := []string{fx.nA + "=1"}
	if got := linkedScores(linkedSection(t, result.Pack)); !reflect.DeepEqual(got, want) {
		t.Fatalf("linked = %v, want %v", got, want)
	}
	for _, id := range []string{fx.nB, fx.nQuarantined, fx.nSuperseded} {
		for _, record := range linkedSection(t, result.Pack).Records {
			if record.ID == id {
				t.Fatalf("linked = %v includes %s", linkedScores(linkedSection(t, result.Pack)), id)
			}
		}
	}
}

// TestContextPackLinkedNeedsRoom keeps the guard that a tight budget skips the
// link lookup completely.
func TestContextPackLinkedNeedsRoom(t *testing.T) {
	// Arrange
	ctx := context.Background()
	fx := newLinkedFixture(t)

	// Act
	result, err := fx.f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "goldenmarker", TokenBudget: 200,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if section := linkedSection(t, result.Pack); len(section.Records) != 0 {
		t.Fatalf("a 200-token answer linked %v", linkedScores(section))
	}
}
