package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

const engramNotesPerFile = 200

// engramNoteFileStamp names a notes file after the exact updated_at of the
// records it holds. Grouping by the timestamp instead of only the created date
// keeps one note's updated_at from being applied to every other note that was
// created the same day, because the store reads a file's frontmatter timestamp
// for every entry in it.
func engramNoteFileStamp(at time.Time) string {
	if at.IsZero() {
		return "000000"
	}
	return at.UTC().Format("150405")
}

type engramNoteFileFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	Date                 string `yaml:"date,omitempty"`
}

type engramProvenanceOut struct {
	Origin   string `json:"origin,omitempty" yaml:"origin,omitempty"`
	Reviewed *bool  `json:"reviewed,omitempty" yaml:"reviewed,omitempty"`
	Trust    string `json:"trust,omitempty" yaml:"trust,omitempty"`
}

type engramRelationOut struct {
	Target     string  `json:"target" yaml:"target"`
	Relation   string  `json:"relation" yaml:"relation"`
	Reason     string  `json:"reason,omitempty" yaml:"reason,omitempty"`
	Confidence float64 `json:"confidence,omitempty" yaml:"confidence,omitempty"`
	Status     string  `json:"status,omitempty" yaml:"status,omitempty"`
	JudgedAt   string  `json:"judgedAt,omitempty" yaml:"judged_at,omitempty"`
	VerdictBy  string  `json:"verdictBy,omitempty" yaml:"verdict_by,omitempty"`
}

type engramRelationsOut struct {
	Relations []engramRelationOut `json:"relations,omitempty" yaml:"relations,omitempty"`
}

type engramReviewOut struct {
	ReviewAfter string `json:"review_after,omitempty" yaml:"review_after,omitempty"`
}

type engramTaxonomyOut struct {
	Subtype     string `json:"subtype,omitempty" yaml:"subtype,omitempty"`
	Scope       string `json:"scope,omitempty" yaml:"scope,omitempty"`
	TopicKey    string `json:"topic_key,omitempty" yaml:"topic_key,omitempty"`
	Occurrences int    `json:"occurrences,omitempty" yaml:"occurrences,omitempty"`
}

type engramSourceOut struct {
	ExternalID string `json:"externalId,omitempty" yaml:"external_id,omitempty"`
	Session    string `json:"session,omitempty" yaml:"session,omitempty"`
}

type engramNoteMarker struct {
	engramProvenanceOut
	engramRelationsOut
	engramReviewOut
	engramTaxonomyOut
	ID        string           `json:"id"`
	Title     string           `json:"title"`
	Source    *engramSourceOut `json:"source,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	// UpdatedAt is the source updated_at of this note. The daily file
	// frontmatter holds one timestamp per file, so without this field every
	// note that shares a file would be read back with the file's maximum.
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

type engramDecisionFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	engramProvenanceOut  `yaml:",inline"`
	engramRelationsOut   `yaml:",inline"`
	engramReviewOut      `yaml:",inline"`
	engramTaxonomyOut    `yaml:",inline"`
	ID                   string           `yaml:"id,omitempty"`
	Title                string           `yaml:"title,omitempty"`
	Status               string           `yaml:"status,omitempty"`
	Source               *engramSourceOut `yaml:"source,omitempty"`
	CreatedAt            time.Time        `yaml:"created_at,omitempty"`
}

type engramPromptFileFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	Visibility           string `yaml:"visibility"`
}

func renderEngramFiles(ctx context.Context, plan *engramPlan, result *Result) error {
	notes := map[string][]*engramRecord{}
	noteOrder := []string{}
	decisions := make([]*engramRecord, 0, len(plan.records))
	for _, record := range plan.records {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch record.kind {
		case engramRecordDecision:
			decisions = append(decisions, record)
		default:
			date := record.createdAt.UTC().Format("2006-01-02")
			stamp := engramNoteFileStamp(record.updatedAt)
			key := record.project + "\x00" + date + "\x00" + stamp
			if _, ok := notes[key]; !ok {
				noteOrder = append(noteOrder, key)
			}
			notes[key] = append(notes[key], record)
		}
	}
	sort.Strings(noteOrder)
	for _, key := range noteOrder {
		group := notes[key]
		sort.SliceStable(group, func(i, j int) bool {
			if !group[i].createdAt.Equal(group[j].createdAt) {
				return group[i].createdAt.Before(group[j].createdAt)
			}
			return group[i].externalID < group[j].externalID
		})
		parts := strings.SplitN(key, "\x00", 3)
		project, date, stamp := parts[0], parts[1], parts[2]
		for index := 0; index < len(group); index += engramNotesPerFile {
			end := index + engramNotesPerFile
			if end > len(group) {
				end = len(group)
			}
			file := index / engramNotesPerFile
			name := fmt.Sprintf("engram-%s-%s.md", date, stamp)
			if file > 0 {
				name = fmt.Sprintf("engram-%s-%s-%d.md", date, stamp, file+1)
			}
			content, err := renderEngramNoteFile(project, date, group[index:end])
			if err != nil {
				return err
			}
			plan.files = append(plan.files, engramPlannedFile{
				rel: path.Join(projectsDir, project, notesDir, name), content: content,
				kind: "note", project: project,
				source: "engram:observations/" + strings.TrimPrefix(group[index].externalID, EngramSourceSystem+":"),
			})
		}
	}
	sort.SliceStable(decisions, func(i, j int) bool {
		if decisions[i].project != decisions[j].project {
			return decisions[i].project < decisions[j].project
		}
		return decisions[i].id < decisions[j].id
	})
	for _, record := range decisions {
		content, err := renderEngramDecisionFile(record)
		if err != nil {
			return err
		}
		plan.files = append(plan.files, engramPlannedFile{
			rel: path.Join(projectsDir, record.project, decisionsDir, record.id+".md"), content: content,
			kind: "decision", project: record.project, source: "engram:observations/" + record.syncID,
		})
	}
	if len(plan.prompts) > 0 {
		promptFiles, err := renderEngramPromptFiles(plan.prompts)
		if err != nil {
			return err
		}
		plan.files = append(plan.files, promptFiles...)
	}
	sort.SliceStable(plan.files, func(i, j int) bool { return plan.files[i].rel < plan.files[j].rel })
	return nil
}

func renderEngramNoteFile(project, date string, records []*engramRecord) ([]byte, error) {
	updatedAt := records[0].updatedAt
	for _, record := range records[1:] {
		if record.updatedAt.After(updatedAt) {
			updatedAt = record.updatedAt
		}
	}
	frontmatter := engramNoteFileFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID: project, Kind: "note", UpdatedAt: updatedAt.UTC(),
			SchemaVersion: SchemaVersion,
		},
		Date: date,
	}
	var body strings.Builder
	for _, record := range records {
		marker := engramNoteMarker{
			engramProvenanceOut: provenanceOut(record),
			engramRelationsOut:  relationsOut(record.relations),
			engramReviewOut:     reviewOut(record.reviewAfter),
			engramTaxonomyOut:   taxonomyOut(record),
			ID:                  record.id,
			Title:               record.title,
			Source:              sourceOut(record),
			CreatedAt:           record.createdAt.UTC(),
			UpdatedAt:           record.updatedAt.UTC(),
		}
		raw, err := json.Marshal(marker)
		if err != nil {
			return nil, fmt.Errorf("migrate: encode note marker: %w", err)
		}
		body.WriteString(noteMarkerPrefix)
		body.Write(raw)
		body.WriteString(noteMarkerSuffix)
		body.WriteByte('\n')
		body.WriteString("## ")
		body.WriteString(record.title)
		body.WriteString("\n\n")
		body.WriteString(strings.TrimSpace(record.body))
		body.WriteString("\n\n")
		if history := renderEngramHistory(record.history); history != "" {
			body.WriteString(history)
			body.WriteString("\n\n")
		}
	}
	return markdown.Render(frontmatter, strings.TrimSpace(body.String())+"\n")
}

func renderEngramDecisionFile(record *engramRecord) ([]byte, error) {
	frontmatter := engramDecisionFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID: record.project, Kind: "decision", UpdatedAt: record.updatedAt.UTC(),
			SchemaVersion: SchemaVersion,
		},
		engramProvenanceOut: provenanceOut(record),
		engramRelationsOut:  relationsOut(record.relations),
		engramReviewOut:     reviewOut(record.reviewAfter),
		engramTaxonomyOut:   taxonomyOut(record),
		ID:                  record.id,
		Title:               record.title,
		Status:              string(domain.DecisionAccepted),
		Source:              sourceOut(record),
		CreatedAt:           record.createdAt.UTC(),
	}
	var body strings.Builder
	fmt.Fprintf(&body, "# %s\n\n", record.title)
	body.WriteString("Status: accepted\n\n## Context\n\n")
	body.WriteString(strings.TrimSpace(record.context))
	body.WriteString("\n\n## Alternatives\n\n")
	writeMarkdownList(&body, record.alternatives)
	body.WriteString("\n## Consequences\n\n")
	writeMarkdownList(&body, record.consequences)
	body.WriteString("\n## Evidence\n\n")
	writeMarkdownList(&body, record.evidence)
	if history := renderEngramHistory(record.history); history != "" {
		body.WriteString("\n")
		body.WriteString(history)
		body.WriteString("\n")
	}
	return markdown.Render(frontmatter, body.String())
}

func renderEngramPromptFiles(entries []engramPromptEntry) ([]engramPlannedFile, error) {
	groups := map[string][]engramPromptEntry{}
	order := []string{}
	for _, entry := range entries {
		date := entry.at.UTC().Format("2006-01-02")
		key := entry.project + "\x00" + date
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], entry)
	}
	sort.Strings(order)
	files := make([]engramPlannedFile, 0, len(order)+2)
	projectsWithPrompts := map[string]bool{}
	for _, key := range order {
		group := groups[key]
		sort.SliceStable(group, func(i, j int) bool {
			if !group[i].at.Equal(group[j].at) {
				return group[i].at.Before(group[j].at)
			}
			if group[i].text != group[j].text {
				return group[i].text < group[j].text
			}
			return group[i].session < group[j].session
		})
		parts := strings.SplitN(key, "\x00", 2)
		project, date := parts[0], parts[1]
		projectsWithPrompts[project] = true
		updatedAt := group[len(group)-1].at
		content, err := renderEngramPromptFile(project, date, updatedAt, group)
		if err != nil {
			return nil, err
		}
		files = append(files, engramPlannedFile{
			rel: path.Join(projectsDir, project, promptsDir, date+"-engram.md"), content: content,
			kind: "prompt", project: project, source: "engram:prompts/" + date,
		})
	}
	projects := make([]string, 0, len(projectsWithPrompts))
	for project := range projectsWithPrompts {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	for _, project := range projects {
		files = append(files, engramPlannedFile{
			rel: path.Join(projectsDir, project, promptsDir, ".gitignore"), content: []byte("*\n"),
			kind: "prompt", project: project, source: "engram:prompts/gitignore",
		})
	}
	return files, nil
}

func renderEngramPromptFile(project, date string, updatedAt time.Time, entries []engramPromptEntry) ([]byte, error) {
	frontmatter := engramPromptFileFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID: project, Kind: "prompt", UpdatedAt: updatedAt.UTC(),
			SchemaVersion: SchemaVersion,
		},
		Visibility: "local",
	}
	var body strings.Builder
	fmt.Fprintf(&body, "# Prompts — %s\n", date)
	for _, entry := range entries {
		body.WriteString("\n## ")
		body.WriteString(entry.at.UTC().Format(time.RFC3339))
		body.WriteString("\n\n")
		if entry.session != "" {
			body.WriteString("session: ")
			body.WriteString(entry.session)
			body.WriteString("\n\n")
		}
		body.WriteString(strings.TrimSpace(entry.text))
		body.WriteString("\n")
	}
	return markdown.Render(frontmatter, body.String())
}

func renderEngramHistory(history []domain.HistoryEntry) string {
	history = domain.BoundedHistory(history)
	if len(history) == 0 {
		return ""
	}
	var body strings.Builder
	body.WriteString("## History\n")
	for _, entry := range history {
		body.WriteString("\n")
		if at := strings.TrimSpace(entry.At); at != "" {
			body.WriteString("### ")
			body.WriteString(at)
			body.WriteString("\n\n")
		}
		body.WriteString(strings.TrimSpace(entry.Body))
		body.WriteString("\n")
	}
	return strings.TrimSpace(body.String())
}

func provenanceOut(record *engramRecord) engramProvenanceOut {
	out := engramProvenanceOut{Origin: string(domain.OriginImport), Trust: string(domain.TrustStated)}
	if record.quarantine {
		reviewed := false
		out.Reviewed = &reviewed
	}
	return out
}

func taxonomyOut(record *engramRecord) engramTaxonomyOut {
	occurrences := record.occurrences
	if occurrences <= 1 {
		occurrences = 0
	}
	return engramTaxonomyOut{
		Subtype: record.subtype, Scope: record.scope,
		TopicKey: record.topicKey, Occurrences: occurrences,
	}
}

func reviewOut(after time.Time) engramReviewOut {
	if after.IsZero() {
		return engramReviewOut{}
	}
	return engramReviewOut{ReviewAfter: after.UTC().Format(time.RFC3339)}
}

func sourceOut(record *engramRecord) *engramSourceOut {
	if record.externalID == "" && record.session == "" {
		return nil
	}
	return &engramSourceOut{ExternalID: record.externalID, Session: record.session}
}

func relationsOut(relations domain.RelationList) engramRelationsOut {
	normalized := relations.Normalize()
	if len(normalized) == 0 {
		return engramRelationsOut{}
	}
	out := engramRelationsOut{Relations: make([]engramRelationOut, 0, len(normalized))}
	for _, relation := range normalized {
		entry := engramRelationOut{
			Target: relation.Target, Relation: string(relation.Relation),
			Reason: relation.Reason, Confidence: relation.Confidence,
			Status: string(relation.Status), VerdictBy: string(relation.VerdictBy),
		}
		if !relation.JudgedAt.IsZero() {
			entry.JudgedAt = relation.JudgedAt.UTC().Format(time.RFC3339)
		}
		out.Relations = append(out.Relations, entry)
	}
	return out
}
