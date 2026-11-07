package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// DefaultDigestWindow is the look-back of a digest when no time is given.
const DefaultDigestWindow = 24 * time.Hour

// digestLimit bounds each per-project list in a digest.
const digestLimit = 50

// DigestOptions selects the projects and the window start of a digest.
type DigestOptions struct {
	// ProjectID narrows the digest to one registered project; empty means all.
	ProjectID string
	// Since is the window start; zero means DefaultDigestWindow before now.
	Since time.Time
}

// DigestProject is one project's part of a digest. Activity is the window
// delta; the proposed decisions, open questions, blocked items and findings
// describe the current state and are not limited to the window.
type DigestProject struct {
	Project           domain.Project
	Activity          CatchupResult
	LatestHandoff     *domain.Handoff
	Commits           int
	ProposedDecisions []RecordSummary
	OpenQuestions     []string
	Blocked           []string
	Findings          map[FindingKind]int
}

// ActivityCount is the number of changes in the window (records, commits and
// other journal events).
func (p DigestProject) ActivityCount() int {
	return len(p.Activity.Notes) + len(p.Activity.Decisions) + len(p.Activity.Attempts) +
		len(p.Activity.Handoffs) + len(p.Activity.Journal)
}

// FindingCount is the total number of review findings of the project.
func (p DigestProject) FindingCount() int {
	total := 0
	for _, count := range p.Findings {
		total += count
	}
	return total
}

func (p DigestProject) hasReportableContent() bool {
	return p.ActivityCount() > 0 || len(p.ProposedDecisions) > 0 || len(p.OpenQuestions) > 0 ||
		len(p.Blocked) > 0 || p.FindingCount() > 0
}

// Digest is the morning summary across projects. Projects are ordered by
// activity, most active first; quiet projects with nothing to report are left
// out.
type Digest struct {
	GeneratedAt time.Time
	Since       time.Time
	Projects    []DigestProject
}

// Digest builds the owner's summary of what happened since opts.Since and what
// still needs a decision: proposed decisions, open questions, blocked items
// and review findings.
func (a *App) Digest(ctx context.Context, opts DigestOptions) (Digest, error) {
	if err := a.requireStore(); err != nil {
		return Digest{}, err
	}
	now := a.now()
	since := opts.Since
	if since.IsZero() {
		since = now.Add(-DefaultDigestWindow)
	}
	projects, err := a.digestProjects(ctx, opts.ProjectID)
	if err != nil {
		return Digest{}, err
	}
	digest := Digest{GeneratedAt: now.UTC(), Since: since.UTC()}
	for _, project := range projects {
		entry, err := a.digestProject(ctx, project, since)
		if err != nil {
			return Digest{}, err
		}
		if entry.hasReportableContent() {
			digest.Projects = append(digest.Projects, entry)
		}
	}
	sort.SliceStable(digest.Projects, func(i, j int) bool {
		ai, aj := digest.Projects[i].ActivityCount(), digest.Projects[j].ActivityCount()
		if ai != aj {
			return ai > aj
		}
		return digest.Projects[i].Project.ID < digest.Projects[j].Project.ID
	})
	return digest, nil
}

func (a *App) digestProjects(ctx context.Context, projectID string) ([]domain.Project, error) {
	selector := strings.TrimSpace(projectID)
	if selector == "" {
		return a.ListProjects(ctx)
	}
	project, ok, err := a.LookupProject(ctx, selector)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, domain.NewValidationError("project",
			fmt.Sprintf("%q is not a registered project id or alias", selector))
	}
	return []domain.Project{project}, nil
}

func (a *App) digestProject(ctx context.Context, project domain.Project, since time.Time) (DigestProject, error) {
	activity, err := a.Catchup(ctx, CatchupOptions{ProjectID: project.ID, Since: since, Limit: digestLimit})
	if err != nil {
		return DigestProject{}, err
	}
	entry := DigestProject{
		Project:       project,
		Activity:      activity,
		OpenQuestions: activity.State.OpenQuestions,
		Blocked:       activity.State.Blocked,
	}
	for _, item := range activity.Journal {
		if item.Event == JournalCommit {
			entry.Commits++
		}
	}
	_, _, handoffs, err := a.RecentRecords(ctx, project.ID, 1)
	if err != nil {
		return DigestProject{}, err
	}
	if len(handoffs) > 0 {
		latest := handoffs[0]
		entry.LatestHandoff = &latest
	}
	proposed, err := a.ListRecords(ctx, ListRecordsOptions{
		ProjectID: project.ID,
		Kinds:     []string{ports.RecordDecision},
		Status:    string(domain.DecisionProposed),
		Limit:     digestLimit,
	})
	if err != nil {
		return DigestProject{}, err
	}
	entry.ProposedDecisions = proposed
	findings, err := a.Findings(ctx, project.ID)
	if err != nil {
		return DigestProject{}, err
	}
	entry.Findings = countFindings(findings)
	return entry, nil
}

func countFindings(findings []Finding) map[FindingKind]int {
	counts := map[FindingKind]int{}
	for _, finding := range findings {
		counts[finding.Kind]++
	}
	return counts
}
