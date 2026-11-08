package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Link states of a resolved outgoing link.
const (
	LinkFound   = "found"
	LinkMissing = "missing"
	// LinkUnavailable marks a link into another project whose records could
	// not be read (for example no access in server mode).
	LinkUnavailable = "unavailable"
)

// ResolvedLink is one [[...]] reference of a record with its target looked up.
type ResolvedLink struct {
	// Field is the record field that holds the reference (body, context,
	// alternatives, consequences, evidence, hypothesis, result, reason,
	// summary, next_action).
	Field string
	// Ref is the reference as written, without brackets and display text.
	Ref string
	// Text is the optional display text.
	Text      string
	ProjectID string
	ID        string
	// State is LinkFound, LinkMissing or LinkUnavailable.
	State string
	// Detail explains a missing or unavailable target.
	Detail string
	// Target is set when State is LinkFound.
	Target RecordSummary
}

// Backlink is a record of the same project that references the record.
type Backlink struct {
	Field  string
	Source RecordSummary
}

// RecordLinks is a record with its outgoing links and backlinks.
type RecordLinks struct {
	Record    Record
	Links     []ResolvedLink
	Backlinks []Backlink
}

// BrokenLink is a reference to a record that does not exist.
type BrokenLink struct {
	Source RecordSummary
	Link   ResolvedLink
}

// recordField is one text field of a record that may hold links.
type recordField struct {
	name string
	text string
}

// linkFields lists the text fields of a record that are scanned for links.
func linkFields(record Record) []recordField {
	switch {
	case record.Note != nil:
		return []recordField{{"body", record.Note.Body}}
	case record.Decision != nil:
		d := record.Decision
		return []recordField{
			{"context", d.Context},
			{"alternatives", strings.Join(d.Alternatives, "\n")},
			{"consequences", strings.Join(d.Consequences, "\n")},
			{"evidence", strings.Join(d.Evidence, "\n")},
		}
	case record.Attempt != nil:
		a := record.Attempt
		return []recordField{{"hypothesis", a.Hypothesis}, {"result", a.Result}, {"reason", a.Reason}}
	case record.Handoff != nil:
		return []recordField{{"summary", record.Handoff.Summary}, {"next_action", record.Handoff.NextAction}}
	}
	return nil
}

// rawLink is a parsed link with the field and owner it came from.
type rawLink struct {
	field     string
	link      domain.RecordLink
	projectID string // resolved target project
}

func recordRawLinks(record Record) []rawLink {
	owner := record.Summary().ProjectID
	var out []rawLink
	for _, field := range linkFields(record) {
		for _, link := range domain.ParseRecordLinks(field.text) {
			project, _ := link.Target(owner)
			out = append(out, rawLink{field: field.name, link: link, projectID: project})
		}
	}
	return out
}

// linkResolver looks up link targets, loading each project's records at most
// once so resolving many links costs one listing per project.
type linkResolver struct {
	app      *App
	projects map[string]projectRecordSet
}

type projectRecordSet struct {
	byID map[string]RecordSummary
	// state is LinkMissing (no such project), LinkUnavailable (unreadable)
	// or "" when byID is loaded.
	state  string
	detail string
}

func newLinkResolver(a *App) *linkResolver {
	return &linkResolver{app: a, projects: map[string]projectRecordSet{}}
}

// seed registers already loaded records of a project.
func (r *linkResolver) seed(projectID string, records []Record) {
	set := projectRecordSet{byID: make(map[string]RecordSummary, len(records))}
	for _, record := range records {
		summary := record.Summary()
		set.byID[summary.ID] = summary
	}
	r.projects[projectID] = set
}

func (r *linkResolver) load(ctx context.Context, projectID string) projectRecordSet {
	if set, ok := r.projects[projectID]; ok {
		return set
	}
	set := r.fetch(ctx, projectID)
	r.projects[projectID] = set
	return set
}

func (r *linkResolver) fetch(ctx context.Context, projectID string) projectRecordSet {
	project, ok, err := r.app.LookupProject(ctx, projectID)
	if err != nil {
		return projectRecordSet{state: LinkUnavailable, detail: err.Error()}
	}
	if !ok {
		return projectRecordSet{state: LinkMissing, detail: fmt.Sprintf("project %s not found", projectID)}
	}
	records, err := r.app.projectRecords(ctx, project.ID)
	if err != nil {
		return projectRecordSet{state: LinkUnavailable, detail: err.Error()}
	}
	r.seed(project.ID, records)
	set := r.projects[project.ID]
	return set
}

func (r *linkResolver) resolve(ctx context.Context, raw rawLink) ResolvedLink {
	ref := raw.link.ID
	if raw.link.ProjectID != "" {
		ref = raw.link.ProjectID + "/" + raw.link.ID
	}
	out := ResolvedLink{Field: raw.field, Ref: ref, Text: raw.link.Text, ProjectID: raw.projectID, ID: raw.link.ID}
	set := r.load(ctx, raw.projectID)
	if set.state != "" {
		out.State, out.Detail = set.state, set.detail
		return out
	}
	target, ok := set.byID[raw.link.ID]
	if !ok {
		out.State, out.Detail = LinkMissing, fmt.Sprintf("record %s not found in project %s", raw.link.ID, raw.projectID)
		return out
	}
	out.State, out.Target = LinkFound, target
	return out
}

// RecordLinks returns a record with its resolved outgoing links and the
// records of the same project that link to it. Backlinks are computed on read
// from one listing of the project (no stored index); references from other
// projects are not scanned.
func (a *App) RecordLinks(ctx context.Context, projectID, id string) (RecordLinks, error) {
	if err := a.requireStore(); err != nil {
		return RecordLinks{}, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return RecordLinks{}, err
	}
	records, err := a.projectRecords(ctx, project.ID)
	if err != nil {
		return RecordLinks{}, err
	}
	id = strings.TrimSpace(id)
	var result RecordLinks
	found := false
	for _, record := range records {
		if record.Summary().ID == id {
			result.Record, found = record, true
			break
		}
	}
	if !found {
		return RecordLinks{}, fmt.Errorf("record %q in project %s: %w", id, project.ID, domain.ErrNotFound)
	}
	resolver := newLinkResolver(a)
	resolver.seed(project.ID, records)
	for _, raw := range recordRawLinks(result.Record) {
		result.Links = append(result.Links, resolver.resolve(ctx, raw))
	}
	result.Backlinks = backlinksTo(records, project.ID, id)
	return result, nil
}

// backlinksTo lists the records that reference projectID/id, one entry per
// source record and field, in listing order. Self-references are skipped.
func backlinksTo(records []Record, projectID, id string) []Backlink {
	var out []Backlink
	for _, record := range records {
		summary := record.Summary()
		if summary.ID == id && summary.ProjectID == projectID {
			continue
		}
		seen := map[string]bool{}
		for _, raw := range recordRawLinks(record) {
			if raw.projectID != projectID || raw.link.ID != id || seen[raw.field] {
				continue
			}
			seen[raw.field] = true
			out = append(out, Backlink{Field: raw.field, Source: summary})
		}
	}
	return out
}

// BrokenLinks lists references in a project's records whose target record or
// project does not exist. Links into projects that cannot be read are not
// reported as broken.
func (a *App) BrokenLinks(ctx context.Context, projectID string) ([]BrokenLink, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return nil, err
	}
	records, err := a.projectRecords(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	return brokenLinksIn(ctx, a, project.ID, records), nil
}

func brokenLinksIn(ctx context.Context, a *App, projectID string, records []Record) []BrokenLink {
	resolver := newLinkResolver(a)
	resolver.seed(projectID, records)
	var out []BrokenLink
	for _, record := range records {
		for _, raw := range recordRawLinks(record) {
			link := resolver.resolve(ctx, raw)
			if link.State == LinkMissing {
				out = append(out, BrokenLink{Source: record.Summary(), Link: link})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Source.CreatedAt.After(out[j].Source.CreatedAt) })
	return out
}
