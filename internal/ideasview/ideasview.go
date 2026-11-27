package ideasview

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Provenance is the provenance block of the idea row.
type Provenance struct {
	Origin   string `json:"origin"`
	Reviewed bool   `json:"reviewed"`
	Trust    string `json:"trust"`
}

// Validity is the validity block of the idea row.
type Validity struct {
	ValidFrom    *string `json:"valid_from"`
	ValidTo      *string `json:"valid_to"`
	SupersededBy string  `json:"superseded_by"`
	Supersedes   string  `json:"supersedes"`
	Confidence   float64 `json:"confidence"`
}

// Review is the review block of the idea row.
type Review struct {
	ReviewAfter *string `json:"review_after"`
	ReviewedAt  *string `json:"reviewed_at"`
}

// Row is the snake_case idea row of contract §9.3/§11. Body is present only
// where the contract includes it (show and writes).
type Row struct {
	ID                  string             `json:"id"`
	ProjectID           string             `json:"project_id"`
	Title               string             `json:"title"`
	Body                *string            `json:"body,omitempty"`
	Status              string             `json:"status"`
	Priority            string             `json:"priority"`
	DueAt               *string            `json:"due_at"`
	DueDate             *string            `json:"due_date"`
	RemindBeforeMinutes *int               `json:"remind_before_minutes"`
	Tags                []string           `json:"tags"`
	Source              *domain.IdeaSource `json:"source"`
	CompletedAt         *string            `json:"completed_at"`
	CompletionNote      string             `json:"completion_note"`
	CreatedAt           *string            `json:"created_at"`
	UpdatedAt           *string            `json:"updated_at"`
	Path                string             `json:"path"`
	Provenance          Provenance         `json:"provenance"`
	Validity            Validity           `json:"validity"`
	Relations           []domain.Relation  `json:"relations"`
	Review              Review             `json:"review"`
}

// Problem is one unparsable or conflicted idea file.
type Problem struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// List is the GET /v1/ideas response and the CLI list data shape.
type List struct {
	Ideas         []Row     `json:"ideas"`
	Problems      []Problem `json:"problems"`
	Total         int       `json:"total"`
	ProjectSource string    `json:"project_source,omitempty"`
}

// Write is a create or edit response. The local API adds ProjectSource only
// when an edit moved the idea.
type Write struct {
	Idea          Row    `json:"idea"`
	ProjectSource string `json:"project_source,omitempty"`
}

// Show is the GET /v1/ideas/{id} and restore response.
type Show struct {
	Idea Row `json:"idea"`
}

// Trash is the DELETE /v1/ideas/{id} response.
type Trash struct {
	ID      string `json:"id"`
	Trashed bool   `json:"trashed"`
}

// Brief is the GET /v1/ideas/brief response and the CLI brief data shape.
type Brief struct {
	Text     string `json:"text"`
	Ideas    []Row  `json:"ideas"`
	Open     int    `json:"open"`
	DueToday int    `json:"due_today"`
	Muted    bool   `json:"muted"`
}

// RowOf maps one idea to the single row shape shared by the CLI and the local
// API. Timestamps use RFC3339: due_at keeps its stored offset, the others are
// rendered in UTC.
func RowOf(idea domain.Idea, withBody bool) Row {
	status := idea.Status
	if status == "" {
		status = domain.IdeaOpen
	}
	priority := idea.Priority
	if priority == "" {
		priority = domain.IdeaPriorityNormal
	}
	provenance := idea.Provenance.Normalized()
	relations := make([]domain.Relation, 0, len(idea.Relations))
	relations = append(relations, idea.Relations...)
	var body *string
	if withBody {
		body = &idea.Body
	}
	return Row{
		ID:                  idea.ID,
		ProjectID:           idea.ProjectID,
		Title:               idea.Title,
		Body:                body,
		Status:              string(status),
		Priority:            string(priority),
		DueAt:               DueAt(idea.DueAt),
		DueDate:             OptionalString(idea.DueDate),
		RemindBeforeMinutes: idea.RemindBeforeMinutes,
		Tags:                Strings(idea.Tags),
		Source:              idea.Source,
		CompletedAt:         timeValue(idea.CompletedAt),
		CompletionNote:      idea.CompletionNote,
		CreatedAt:           Time(idea.CreatedAt),
		UpdatedAt:           Time(idea.UpdatedAt),
		Path:                StoragePath(idea),
		Provenance: Provenance{
			Origin:   string(provenance.Origin),
			Reviewed: provenance.ReviewedState(),
			Trust:    string(provenance.Trust),
		},
		Validity: Validity{
			ValidFrom:    Time(idea.Validity.EffectiveFrom(idea.CreatedAt)),
			ValidTo:      Time(idea.Validity.ValidTo),
			SupersededBy: idea.Validity.SupersededBy,
			Supersedes:   idea.Validity.Supersedes,
			Confidence:   idea.Validity.Confidence,
		},
		Relations: relations,
		Review: Review{
			ReviewAfter: Time(idea.ReviewSchedule.ReviewAfter),
			ReviewedAt:  Time(idea.ReviewSchedule.ReviewedAt),
		},
	}
}

// Rows maps a list; body never enters a list or brief row.
func Rows(ideas []domain.Idea, withBody bool) []Row {
	rows := make([]Row, 0, len(ideas))
	for _, idea := range ideas {
		rows = append(rows, RowOf(idea, withBody))
	}
	return rows
}

// Problems renders problem rows with data-dir-relative paths when the file is
// inside the brain.
func Problems(dataDir string, problems []ports.RecordProblem) []Problem {
	out := make([]Problem, 0, len(problems))
	for _, problem := range problems {
		out = append(out, Problem{
			Path:   RelativePath(dataDir, problem.Path),
			Kind:   problem.Kind,
			Reason: problem.Reason,
		})
	}
	return out
}

// RelativePath renders a path inside the data directory with forward slashes,
// so output is stable across machines. A path outside it stays absolute. The
// ..foo guard checks a real parent segment, not any name starting with "..".
func RelativePath(dataDir, path string) string {
	if strings.TrimSpace(dataDir) == "" {
		return path
	}
	rel, err := filepath.Rel(dataDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.ToSlash(rel)
}

// StoragePath is the stable logical storage path of an idea, independent of
// the data directory and of the machine's separator.
func StoragePath(idea domain.Idea) string {
	return "projects/" + idea.ProjectID + "/ideas/" + idea.ID + ".md"
}

// DueAt keeps the stored offset of a timed due instead of normalizing it to
// UTC, the same way the frozen row has always done.
func DueAt(value *time.Time) *string {
	if value == nil || value.IsZero() {
		return nil
	}
	formatted := value.Format(time.RFC3339)
	return &formatted
}

// Time renders a timestamp as RFC3339 (UTC) or null.
func Time(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

// OptionalString renders an optional string field as null when unset.
func OptionalString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

// Strings keeps an empty tag list as [] rather than null.
func Strings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func timeValue(value *time.Time) *string {
	if value == nil {
		return nil
	}
	return Time(*value)
}
