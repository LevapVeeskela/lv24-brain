package ports

import (
	"context"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// RelationLink is the indexed form of one typed relation. The Markdown
// frontmatter is the source of truth; the index keeps a derived copy so a
// listing or a backlink scan does not parse every record.
type RelationLink struct {
	// Target is the id of the related record of the same project.
	Target string
	// Relation is the relation verb; see domain.RelationKind.
	Relation string
	// Status is pending, accepted or rejected.
	Status string
	// Confidence is the optional 0..1 author or judge confidence.
	Confidence float64
	// Reason explains the link or the verdict.
	Reason string
	// JudgedAt is when the verdict was given; zero when unjudged.
	JudgedAt time.Time
	// VerdictBy names the judge: human or agent; empty when unjudged.
	VerdictBy string
}

// RelationRow is one stored relation as the index returns it, joined with the
// source document so a caller can render it without a second read.
type RelationRow struct {
	ProjectID   string
	SourceID    string
	SourceKind  string
	SourceTitle string
	TargetID    string
	Relation    string
	Status      string
	Confidence  float64
	Reason      string
	JudgedAt    time.Time
	VerdictBy   string
}

// RelationQuery filters a relation listing. Zero values match everything.
type RelationQuery struct {
	// ProjectID scopes the query; empty selects every project.
	ProjectID string
	// SourceID keeps relations of one source record of ProjectID.
	SourceID string
	// TargetID keeps relations that point at one record of ProjectID.
	TargetID string
	// Statuses keeps these statuses; empty keeps every status.
	Statuses []string
	// Relations keeps these verbs; empty keeps every verb.
	Relations []string
	// Limit bounds the result; <= 0 selects the adapter default.
	Limit int
}

// RelationIndex is implemented by search indexes that project the typed
// relations of every document. It is optional: a store without it reports an
// empty listing and callers fall back to reading records.
type RelationIndex interface {
	Relations(ctx context.Context, query RelationQuery) ([]RelationRow, error)
}

// RecordRelationSetter is implemented by stores that can rewrite the relation
// list of one stored record without touching its text. It is optional like the
// other single-record editors.
type RecordRelationSetter interface {
	// SetRecordRelations replaces the relations of one record and reports
	// whether anything changed. Frontmatter is the source of truth, so this
	// is the only write path for a relation list.
	SetRecordRelations(ctx context.Context, projectID, kind, id string, relations domain.RelationList) (changed bool, err error)
}
