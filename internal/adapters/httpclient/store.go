package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var (
	_ ports.Store           = (*Client)(nil)
	_ ports.DecisionUpdater = (*Client)(nil)
)

const (
	// listPageSize is the page size requested per list call; the server
	// clamps larger values to its own maximum, and a short page ends the scan.
	listPageSize = 500
	// maxListedRecords bounds how many records one List call reads. Reaching
	// it is an error, never a silently truncated listing.
	maxListedRecords = 10000
)

type projectRecord struct {
	domain.Project
	Revision int64 `json:"revision"`
}

type stateRecord struct {
	domain.State
	Revision int64 `json:"revision"`
}

type noteRecord struct {
	domain.Note
	Revision int64 `json:"revision"`
}

type decisionRecord struct {
	domain.Decision
	Revision int64 `json:"revision"`
}

type attemptRecord struct {
	domain.Attempt
	Revision int64 `json:"revision"`
}

type handoffRecord struct {
	domain.Handoff
	Revision int64 `json:"revision"`
}

type revisionRecord struct {
	ID        int64           `json:"id"`
	Kind      domain.SyncKind `json:"kind"`
	Actor     string          `json:"actor,omitempty"`
	Detail    map[string]any  `json:"detail,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
}

type updateProjectRequest struct {
	Name          *string             `json:"name,omitempty"`
	Paths         *[]string           `json:"paths,omitempty"`
	Aliases       *[]string           `json:"aliases,omitempty"`
	Kind          *domain.ProjectKind `json:"kind,omitempty"`
	RepoURL       *string             `json:"repoUrl,omitempty"`
	DefaultBranch *string             `json:"defaultBranch,omitempty"`
}

type putStateRequest struct {
	Goal          string     `json:"goal"`
	Active        []string   `json:"active"`
	Blocked       []string   `json:"blocked"`
	NextAction    string     `json:"nextAction"`
	OpenQuestions []string   `json:"openQuestions"`
	FreshUntil    *time.Time `json:"freshUntil"`
}

type noteRequest struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Tags      []string `json:"tags"`
	Permalink string   `json:"permalink"`
}

type decisionRequest struct {
	Title        string                `json:"title"`
	Status       domain.DecisionStatus `json:"status"`
	Context      string                `json:"context"`
	Alternatives []string              `json:"alternatives"`
	Consequences []string              `json:"consequences"`
	Evidence     []string              `json:"evidence"`
	Permalink    string                `json:"permalink"`
	// Revision is the record revision the update was based on; zero makes
	// the server write unconditionally. Create requests leave it out.
	Revision int64 `json:"revision,omitempty"`
}

type attemptRequest struct {
	Title      string `json:"title"`
	Hypothesis string `json:"hypothesis"`
	Result     string `json:"result"`
	Reason     string `json:"reason"`
	Permalink  string `json:"permalink"`
}

type handoffRequest struct {
	Summary    string `json:"summary"`
	GitContext string `json:"gitContext"`
	NextAction string `json:"nextAction"`
	Permalink  string `json:"permalink"`
}

// GetProject loads a project by ID.
func (c *Client) GetProject(ctx context.Context, id string) (domain.Project, error) {
	if err := requireValue("project.id", id); err != nil {
		return domain.Project{}, err
	}
	var record projectRecord
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(id), nil, nil, &record); err != nil {
		return domain.Project{}, err
	}
	return record.Project, nil
}

// ListProjects returns every project ordered by name.
func (c *Client) ListProjects(ctx context.Context) ([]domain.Project, error) {
	var payload struct {
		Items []projectRecord `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects", nil, nil, &payload); err != nil {
		return nil, err
	}
	projects := make([]domain.Project, 0, len(payload.Items))
	for _, record := range payload.Items {
		projects = append(projects, record.Project)
	}
	return projects, nil
}

// PutProject creates the project when it is missing and otherwise replaces its
// mutable fields. The server request deliberately omits a revision so the
// write is unconditional, matching the ports.Store contract.
//
// The lookup and write are two requests: if the project is deleted between
// them, the PATCH fails with domain.ErrNotFound. The server has no single
// unconditional project upsert endpoint to call instead.
func (c *Client) PutProject(ctx context.Context, project domain.Project) error {
	if err := project.Validate(); err != nil {
		return err
	}
	_, err := c.GetProject(ctx, project.ID)
	switch {
	case err == nil:
		body := updateProjectRequest{
			Name:          &project.Name,
			Paths:         &project.Paths,
			Aliases:       &project.Aliases,
			RepoURL:       &project.RepoURL,
			DefaultBranch: &project.DefaultBranch,
		}
		if project.Kind != "" {
			body.Kind = &project.Kind
		}
		return c.do(ctx, http.MethodPatch, "/v1/projects/"+url.PathEscape(project.ID), nil, body, nil)
	case errors.Is(err, domain.ErrNotFound):
		return c.do(ctx, http.MethodPost, "/v1/projects", nil, project, nil)
	default:
		return err
	}
}

// GetState loads project state.
func (c *Client) GetState(ctx context.Context, projectID string) (domain.State, error) {
	if err := requireValue("state.projectId", projectID); err != nil {
		return domain.State{}, err
	}
	var record stateRecord
	if err := c.do(ctx, http.MethodGet, projectStatePath(projectID), nil, nil, &record); err != nil {
		return domain.State{}, err
	}
	return record.State, nil
}

// PutState replaces project state and returns the new server revision.
func (c *Client) PutState(ctx context.Context, state domain.State) (domain.SyncRevision, error) {
	if err := state.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	body := putStateRequest{
		Goal:          state.Goal,
		Active:        state.Active,
		Blocked:       state.Blocked,
		NextAction:    state.NextAction,
		OpenQuestions: state.OpenQuestions,
	}
	if !state.FreshUntil.IsZero() {
		freshUntil := state.FreshUntil
		body.FreshUntil = &freshUntil
	}
	var record stateRecord
	if err := c.do(ctx, http.MethodPut, projectStatePath(state.ProjectID), nil, body, &record); err != nil {
		return domain.SyncRevision{}, err
	}
	return revisionFromNumber(record.Revision, record.UpdatedAt), nil
}

// AppendNote creates a note and returns the new server revision.
func (c *Client) AppendNote(ctx context.Context, note domain.Note) (domain.SyncRevision, error) {
	if err := note.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	var record noteRecord
	body := noteRequest{
		Title:     note.Title,
		Body:      note.Body,
		Tags:      note.Tags,
		Permalink: note.Permalink,
	}
	if err := c.do(ctx, http.MethodPost, projectPath(note.ProjectID, "notes"), nil, body, &record); err != nil {
		return domain.SyncRevision{}, err
	}
	return revisionFromNumber(record.Revision, record.CreatedAt), nil
}

// AppendDecision creates a decision and returns the new server revision.
func (c *Client) AppendDecision(ctx context.Context, decision domain.Decision) (domain.SyncRevision, error) {
	if err := decision.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	var record decisionRecord
	body := decisionRequest{
		Title:        decision.Title,
		Status:       decision.Status,
		Context:      decision.Context,
		Alternatives: decision.Alternatives,
		Consequences: decision.Consequences,
		Evidence:     decision.Evidence,
		Permalink:    decision.Permalink,
	}
	if err := c.do(ctx, http.MethodPost, projectPath(decision.ProjectID, "decisions"), nil, body, &record); err != nil {
		return domain.SyncRevision{}, err
	}
	return revisionFromNumber(record.Revision, record.CreatedAt), nil
}

// AppendAttempt creates an attempt and returns the new server revision.
func (c *Client) AppendAttempt(ctx context.Context, attempt domain.Attempt) (domain.SyncRevision, error) {
	if err := attempt.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	var record attemptRecord
	body := attemptRequest{
		Title:      attempt.Title,
		Hypothesis: attempt.Hypothesis,
		Result:     attempt.Result,
		Reason:     attempt.Reason,
		Permalink:  attempt.Permalink,
	}
	if err := c.do(ctx, http.MethodPost, projectPath(attempt.ProjectID, "attempts"), nil, body, &record); err != nil {
		return domain.SyncRevision{}, err
	}
	return revisionFromNumber(record.Revision, record.CreatedAt), nil
}

// AppendHandoff creates a handoff and returns the new server revision.
func (c *Client) AppendHandoff(ctx context.Context, handoff domain.Handoff) (domain.SyncRevision, error) {
	if err := handoff.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	var record handoffRecord
	body := handoffRequest{
		Summary:    handoff.Summary,
		GitContext: handoff.GitContext,
		NextAction: handoff.NextAction,
		Permalink:  handoff.Permalink,
	}
	if err := c.do(ctx, http.MethodPost, projectPath(handoff.ProjectID, "handoffs"), nil, body, &record); err != nil {
		return domain.SyncRevision{}, err
	}
	return revisionFromNumber(record.Revision, record.CreatedAt), nil
}

// ListNotes returns every note of a project, newest first as the server orders them.
func (c *Client) ListNotes(ctx context.Context, projectID string) ([]domain.Note, error) {
	records, err := listAll[noteRecord](ctx, c, projectPath(projectID, "notes"))
	if err != nil {
		return nil, err
	}
	notes := make([]domain.Note, 0, len(records))
	for _, record := range records {
		notes = append(notes, record.Note)
	}
	return notes, nil
}

// ListDecisions returns every decision of a project.
func (c *Client) ListDecisions(ctx context.Context, projectID string) ([]domain.Decision, error) {
	records, err := listAll[decisionRecord](ctx, c, projectPath(projectID, "decisions"))
	if err != nil {
		return nil, err
	}
	decisions := make([]domain.Decision, 0, len(records))
	for _, record := range records {
		decisions = append(decisions, record.Decision)
	}
	return decisions, nil
}

// ListAttempts returns every attempt of a project.
func (c *Client) ListAttempts(ctx context.Context, projectID string) ([]domain.Attempt, error) {
	records, err := listAll[attemptRecord](ctx, c, projectPath(projectID, "attempts"))
	if err != nil {
		return nil, err
	}
	attempts := make([]domain.Attempt, 0, len(records))
	for _, record := range records {
		attempts = append(attempts, record.Attempt)
	}
	return attempts, nil
}

// ListHandoffs returns every handoff of a project.
func (c *Client) ListHandoffs(ctx context.Context, projectID string) ([]domain.Handoff, error) {
	records, err := listAll[handoffRecord](ctx, c, projectPath(projectID, "handoffs"))
	if err != nil {
		return nil, err
	}
	handoffs := make([]domain.Handoff, 0, len(records))
	for _, record := range records {
		handoffs = append(handoffs, record.Handoff)
	}
	return handoffs, nil
}

// UpdateDecision replaces a decision's content. It reads the current record
// first and sends its revision back, so a concurrent edit fails with
// domain.ErrConflict instead of being overwritten. ErrNotFound when the
// decision is missing or belongs to another project.
func (c *Client) UpdateDecision(ctx context.Context, decision domain.Decision) error {
	if err := decision.Validate(); err != nil {
		return err
	}
	if err := requireValue("decision.id", decision.ID); err != nil {
		return err
	}
	var current decisionRecord
	if err := c.do(ctx, http.MethodGet, "/v1/decisions/"+url.PathEscape(decision.ID), nil, nil, &current); err != nil {
		return err
	}
	if current.ProjectID != decision.ProjectID {
		return fmt.Errorf("decision %s in project %s: %w", decision.ID, decision.ProjectID, domain.ErrNotFound)
	}
	body := decisionRequest{
		Title:        decision.Title,
		Status:       decision.Status,
		Context:      decision.Context,
		Alternatives: decision.Alternatives,
		Consequences: decision.Consequences,
		Evidence:     decision.Evidence,
		Permalink:    decision.Permalink,
		Revision:     current.Revision,
	}
	return c.do(ctx, http.MethodPatch, "/v1/decisions/"+url.PathEscape(decision.ID), nil, body, nil)
}

// listAll reads every page of a project collection. The server returns
// {"items": [...]} per page; a page shorter than listPageSize is the last one.
func listAll[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var all []T
	for offset := 0; ; offset += listPageSize {
		if offset >= maxListedRecords {
			return nil, fmt.Errorf("httpclient: %s holds more than %d records; the listing was not read in full", path, maxListedRecords)
		}
		query := url.Values{}
		query.Set("limit", strconv.Itoa(listPageSize))
		query.Set("offset", strconv.Itoa(offset))
		var payload struct {
			Items []T `json:"items"`
		}
		if err := c.do(ctx, http.MethodGet, path, query, nil, &payload); err != nil {
			return nil, err
		}
		all = append(all, payload.Items...)
		if len(payload.Items) < listPageSize {
			return all, nil
		}
	}
}

// CurrentRevision returns the newest server revision. An empty server reports
// revision "0", matching the server-side store.
func (c *Client) CurrentRevision(ctx context.Context) (domain.SyncRevision, error) {
	var payload struct {
		Items []revisionRecord `json:"items"`
	}
	query := url.Values{}
	query.Set("limit", "1")
	if err := c.do(ctx, http.MethodGet, "/v1/revisions", query, nil, &payload); err != nil {
		return domain.SyncRevision{}, err
	}
	if len(payload.Items) == 0 {
		return domain.SyncRevision{Kind: domain.SyncKindServer, Value: "0"}, nil
	}
	record := payload.Items[0]
	return domain.SyncRevision{
		Kind:  domain.SyncKindServer,
		Value: strconv.FormatInt(record.ID, 10),
		At:    record.CreatedAt.UTC(),
	}, nil
}

func projectPath(projectID, collection string) string {
	return "/v1/projects/" + url.PathEscape(projectID) + "/" + collection
}

func projectStatePath(projectID string) string {
	return "/v1/projects/" + url.PathEscape(projectID) + "/state"
}

func revisionFromNumber(value int64, at time.Time) domain.SyncRevision {
	return domain.SyncRevision{
		Kind:  domain.SyncKindServer,
		Value: strconv.FormatInt(value, 10),
		At:    at.UTC(),
	}
}

func requireValue(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return domain.NewValidationError(field, "is required")
	}
	return nil
}
