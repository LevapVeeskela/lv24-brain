package ideasview

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func sampleIdea() domain.Idea {
	due := time.Date(2026, 12, 1, 18, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	remind := 30
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	return domain.Idea{
		ID:                  "idea-1",
		ProjectID:           "brain",
		Title:               "Dark theme",
		Body:                "Ship the dark calendar",
		Status:              domain.IdeaOpen,
		Priority:            domain.IdeaPriorityHigh,
		DueAt:               &due,
		RemindBeforeMinutes: &remind,
		Tags:                []string{"ui"},
		CreatedAt:           created,
		UpdatedAt:           created,
	}
}

func TestRowOfMatchesTheContractJSON(t *testing.T) {
	idea := sampleIdea()
	row := RowOf(idea, true)

	if row.Body == nil || *row.Body != "Ship the dark calendar" {
		t.Fatalf("body = %v, want the write row body", row.Body)
	}
	if row.DueAt == nil || *row.DueAt != "2026-12-01T18:00:00+03:00" {
		t.Fatalf("due_at = %v, want the stored offset", row.DueAt)
	}
	if row.UpdatedAt == nil || *row.UpdatedAt != "2026-11-26T10:00:00Z" {
		t.Fatalf("updated_at = %v, want UTC RFC3339", row.UpdatedAt)
	}
	if row.CreatedAt == nil || *row.CreatedAt != "2026-11-26T10:00:00Z" {
		t.Fatalf("created_at = %v, want UTC RFC3339", row.CreatedAt)
	}
	if row.Path != "projects/brain/ideas/idea-1.md" {
		t.Fatalf("path = %q", row.Path)
	}
	if row.Provenance.Origin != string(domain.OriginHuman) || !row.Provenance.Reviewed ||
		row.Provenance.Trust != string(domain.TrustVerified) {
		t.Fatalf("provenance = %+v", row.Provenance)
	}
	if row.Validity.ValidFrom == nil || *row.Validity.ValidFrom != "2026-11-26T10:00:00Z" {
		t.Fatalf("valid_from = %v", row.Validity.ValidFrom)
	}
	if row.Relations == nil {
		t.Fatal("relations must be [] rather than null")
	}
	if row.Tags == nil || len(row.Tags) != 1 || row.Tags[0] != "ui" {
		t.Fatalf("tags = %v", row.Tags)
	}
	if row.RemindBeforeMinutes == nil || *row.RemindBeforeMinutes != 30 {
		t.Fatalf("remind_before_minutes = %v", row.RemindBeforeMinutes)
	}

	listRow := RowOf(idea, false)
	if listRow.Body != nil {
		t.Fatalf("a list row must not carry body: %v", listRow.Body)
	}
}

func TestProblemRowsUseTheDataDirectoryAndKeepDotDotNames(t *testing.T) {
	dataDir := t.TempDir()
	inside := filepath.Join(dataDir, "projects", "brain", "ideas", "idea-9.md")
	dotDotName := filepath.Join(dataDir, "..foo", "idea-9.md")
	outside := filepath.Join(dataDir, "..", "other.md")

	problems := Problems(dataDir, []ports.RecordProblem{
		{Path: inside, Kind: ports.RecordIdea, Reason: "git conflict markers"},
		{Path: dotDotName, Kind: ports.RecordIdea, Reason: "bad frontmatter"},
		{Path: outside, Kind: ports.RecordIdea, Reason: "outside"},
	})
	if len(problems) != 3 {
		t.Fatalf("problems = %+v", problems)
	}
	if problems[0].Path != "projects/brain/ideas/idea-9.md" {
		t.Fatalf("inside path = %q", problems[0].Path)
	}
	if problems[1].Path != "..foo/idea-9.md" {
		t.Fatalf("..foo path must stay relative, got %q", problems[1].Path)
	}
	if problems[2].Path != outside {
		t.Fatalf("outside path = %q, want %q", problems[2].Path, outside)
	}
}

func TestStoragePathIsIndependentOfTheDataDirectory(t *testing.T) {
	if got := StoragePath(sampleIdea()); got != "projects/brain/ideas/idea-1.md" {
		t.Fatalf("storage path = %q", got)
	}
}
