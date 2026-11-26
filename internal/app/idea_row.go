package app

import (
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// IdeaRow is the snake_case row shape of the contract (§9.3). The CLI `--json`
// output, the MCP `brain_idea` tool and the local API all render one idea
// through it, so the surfaces cannot drift.
type IdeaRow struct {
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
	Provenance          IdeaRowProvenance  `json:"provenance"`
	Validity            IdeaRowValidity    `json:"validity"`
	Relations           []domain.Relation  `json:"relations"`
	Review              IdeaRowReview      `json:"review"`
}

// IdeaRowProvenance is the provenance block of an idea row.
type IdeaRowProvenance struct {
	Origin   string `json:"origin"`
	Reviewed bool   `json:"reviewed"`
	Trust    string `json:"trust"`
}

// IdeaRowValidity is the validity block of an idea row.
type IdeaRowValidity struct {
	ValidFrom    *string `json:"valid_from"`
	ValidTo      *string `json:"valid_to"`
	SupersededBy string  `json:"superseded_by"`
	Supersedes   string  `json:"supersedes"`
	Confidence   float64 `json:"confidence"`
}

// IdeaRowReview is the review block of an idea row.
type IdeaRowReview struct {
	ReviewAfter *string `json:"review_after"`
	ReviewedAt  *string `json:"reviewed_at"`
}

// NewIdeaRow maps one idea to the contract row. The body is included only where
// the contract includes it (show and writes).
func NewIdeaRow(idea domain.Idea, withBody bool) IdeaRow {
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
	return IdeaRow{
		ID:                  idea.ID,
		ProjectID:           idea.ProjectID,
		Title:               idea.Title,
		Body:                body,
		Status:              string(status),
		Priority:            string(priority),
		DueAt:               ideaRowDueAt(idea.DueAt),
		DueDate:             ideaRowOptionalString(idea.DueDate),
		RemindBeforeMinutes: idea.RemindBeforeMinutes,
		Tags:                ideaRowStrings(idea.Tags),
		Source:              idea.Source,
		CompletedAt:         ideaRowTime(idea.CompletedAt),
		CompletionNote:      idea.CompletionNote,
		CreatedAt:           ideaRowTime(&idea.CreatedAt),
		UpdatedAt:           ideaRowTime(&idea.UpdatedAt),
		Path:                IdeaStoragePath(idea),
		Provenance: IdeaRowProvenance{
			Origin:   string(provenance.Origin),
			Reviewed: provenance.ReviewedState(),
			Trust:    string(provenance.Trust),
		},
		Validity: IdeaRowValidity{
			ValidFrom:    ideaRowTimeValue(idea.Validity.EffectiveFrom(idea.CreatedAt)),
			ValidTo:      ideaRowTimeValue(idea.Validity.ValidTo),
			SupersededBy: idea.Validity.SupersededBy,
			Supersedes:   idea.Validity.Supersedes,
			Confidence:   idea.Validity.Confidence,
		},
		Relations: relations,
		Review: IdeaRowReview{
			ReviewAfter: ideaRowTimeValue(idea.ReviewSchedule.ReviewAfter),
			ReviewedAt:  ideaRowTimeValue(idea.ReviewSchedule.ReviewedAt),
		},
	}
}

// IdeaStoragePath is the stable storage path of an idea, independent of the
// data directory.
func IdeaStoragePath(idea domain.Idea) string {
	return recordDataPath(idea.ProjectID, "idea", idea.ID+".md")
}

// ideaRowDueAt keeps the stored offset of a timed due instead of normalizing it
// to UTC: the row reports the value the writer set.
func ideaRowDueAt(at *time.Time) *string {
	if at == nil || at.IsZero() {
		return nil
	}
	formatted := at.Format(time.RFC3339)
	return &formatted
}

// ideaRowTime renders an optional instant in UTC, or nil when unset.
func ideaRowTime(value *time.Time) *string {
	if value == nil || value.IsZero() {
		return nil
	}
	return ideaRowTimeValue(*value)
}

// ideaRowTimeValue renders an instant in UTC, or nil when it is zero.
func ideaRowTimeValue(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

func ideaRowOptionalString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

// ideaRowStrings keeps an empty list as [] rather than null in JSON.
func ideaRowStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
