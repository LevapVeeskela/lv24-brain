// Package serverstore adapts the PostgreSQL persistence layer to the contract
// required by internal/adapters/httpapi.
//
// The two packages intentionally define separate record types (for example
// httpapi.ProjectRecord and postgres.ProjectRecord) so the HTTP layer does not
// import the database driver. This adapter is the single bridge between them:
// it converts records, translates optimistic-concurrency errors into
// httpapi.ConflictError (so handlers can return the current revision and merge
// hint), and fills the few gaps the PostgreSQL store does not cover yet.
//
// Gaps covered here instead of in postgres.Store:
//   - Search: postgres.Store has no full-text/substring search.
//   - Ascending revision listings: postgres.RevisionFilter cannot express
//     ascending order, which SSE replay requires.
//
// API tokens and refresh-token rotations are persisted by postgres.Store
// (migration 0002_auth_tokens.sql) and only converted here.
//
// The revisions table has no action column, so RevisionRecord.Action is always
// empty for this store. Handlers and SSE clients must rely on EntityKind and
// the entity's own state instead of the action label.
//
// Connector-run passthroughs implement httpapi.Store and back the server-side
// connector runner; role passthroughs remain server-bootstrap helpers.
package serverstore

import (
	"context"
	"errors"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/postgres"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

var _ httpapi.Store = (*Store)(nil)

// Store implements httpapi.Store on top of a postgres.Store.
type Store struct {
	pg *postgres.Store
}

// New wraps a migrated postgres.Store. The caller owns the pool and must close
// the underlying store when the process exits.
func New(pg *postgres.Store) (*Store, error) {
	if pg == nil {
		return nil, errors.New("serverstore: postgres store is required")
	}
	if pg.Pool() == nil {
		return nil, errors.New("serverstore: postgres store has no connection pool")
	}
	return &Store{pg: pg}, nil
}

// Postgres exposes the wrapped store for health checks and connector runners.
func (s *Store) Postgres() *postgres.Store { return s.pg }

// Ping verifies database reachability.
func (s *Store) Ping(ctx context.Context) error {
	return mapError(s.pg.Ping(ctx))
}

// mapError converts a postgres revision conflict into the HTTP record type so
// httpapi.writeError can emit currentRevision and mergeHint on 409 responses.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var conflict *postgres.RevisionConflictError
	if errors.As(err, &conflict) {
		return &httpapi.ConflictError{
			Entity:   conflict.Entity,
			ID:       conflict.ID,
			Expected: conflict.Expected,
			Current:  conflict.Current,
		}
	}
	return err
}

func toUserRecord(record postgres.UserRecord) httpapi.UserRecord {
	return httpapi.UserRecord{
		User:         record.User,
		PasswordHash: record.PasswordHash,
		OIDCIssuer:   record.OIDCIssuer,
		OIDCSubject:  record.OIDCSubject,
		Revision:     record.Revision,
		CreatedAt:    record.CreatedAt,
		UpdatedAt:    record.UpdatedAt,
	}
}

func toProjectRecord(record postgres.ProjectRecord) httpapi.ProjectRecord {
	return httpapi.ProjectRecord{Project: record.Project, Revision: record.Revision}
}

func toStateRecord(record postgres.StateRecord) httpapi.StateRecord {
	return httpapi.StateRecord{State: record.State, Revision: record.Revision}
}

func toNoteRecord(record postgres.NoteRecord) httpapi.NoteRecord {
	return httpapi.NoteRecord{Note: record.Note, Revision: record.Revision, CreatedBy: record.CreatedBy}
}

func toDecisionRecord(record postgres.DecisionRecord) httpapi.DecisionRecord {
	return httpapi.DecisionRecord{Decision: record.Decision, Revision: record.Revision, CreatedBy: record.CreatedBy}
}

func toAttemptRecord(record postgres.AttemptRecord) httpapi.AttemptRecord {
	return httpapi.AttemptRecord{Attempt: record.Attempt, Revision: record.Revision, CreatedBy: record.CreatedBy}
}

func toHandoffRecord(record postgres.HandoffRecord) httpapi.HandoffRecord {
	return httpapi.HandoffRecord{Handoff: record.Handoff, Revision: record.Revision, CreatedBy: record.CreatedBy}
}

func toRevisionRecord(record postgres.RevisionRecord) httpapi.RevisionRecord {
	return httpapi.RevisionRecord{
		ID:         record.ID,
		Kind:       record.Kind,
		Actor:      record.Actor,
		EntityKind: record.EntityKind,
		EntityID:   record.EntityID,
		Detail:     record.Detail,
		CreatedAt:  record.CreatedAt,
	}
}

func listOptions(opts httpapi.ListOptions) postgres.ListOptions {
	return postgres.ListOptions{
		ProjectID: opts.ProjectID,
		Limit:     opts.Limit,
		Offset:    opts.Offset,
	}
}

// CreateUser inserts a user and returns its server revision.
func (s *Store) CreateUser(ctx context.Context, user domain.User, passwordHash string) (httpapi.UserRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.CreateUser(ctx, user, passwordHash)
	if err != nil {
		return httpapi.UserRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toUserRecord(record), revision, nil
}

// GetUser loads a user by ID.
func (s *Store) GetUser(ctx context.Context, id string) (httpapi.UserRecord, error) {
	record, err := s.pg.GetUser(ctx, id)
	if err != nil {
		return httpapi.UserRecord{}, mapError(err)
	}
	return toUserRecord(record), nil
}

// GetUserByLogin loads a user by case-insensitive login.
func (s *Store) GetUserByLogin(ctx context.Context, login string) (httpapi.UserRecord, error) {
	record, err := s.pg.GetUserByLogin(ctx, login)
	if err != nil {
		return httpapi.UserRecord{}, mapError(err)
	}
	return toUserRecord(record), nil
}

// GetUserByOIDC loads a user by issuer and subject.
func (s *Store) GetUserByOIDC(ctx context.Context, issuer, subject string) (httpapi.UserRecord, error) {
	record, err := s.pg.GetUserByOIDC(ctx, issuer, subject)
	if err != nil {
		return httpapi.UserRecord{}, mapError(err)
	}
	return toUserRecord(record), nil
}

// ListUsers returns every user ordered by login.
func (s *Store) ListUsers(ctx context.Context) ([]httpapi.UserRecord, error) {
	records, err := s.pg.ListUsers(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	users := make([]httpapi.UserRecord, 0, len(records))
	for _, record := range records {
		users = append(users, toUserRecord(record))
	}
	return users, nil
}

// UpdateUser updates display name, role and auth source.
func (s *Store) UpdateUser(ctx context.Context, user domain.User, expected int64) (httpapi.UserRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.UpdateUser(ctx, user, expected)
	if err != nil {
		return httpapi.UserRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toUserRecord(record), revision, nil
}

// SetUserOIDC links a user to an OIDC issuer and subject. The HTTP contract has
// no revision parameter, so it always applies without an optimistic check; the
// database row still receives a fresh revision.
func (s *Store) SetUserOIDC(ctx context.Context, id, issuer, subject string) (domain.SyncRevision, error) {
	revision, err := s.pg.SetUserOIDC(ctx, id, issuer, subject, 0)
	return revision, mapError(err)
}

// DeleteUser removes a user with an optional optimistic check.
func (s *Store) DeleteUser(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	revision, err := s.pg.DeleteUser(ctx, id, expected)
	return revision, mapError(err)
}

// CreateProject inserts a project.
func (s *Store) CreateProject(ctx context.Context, project domain.Project) (httpapi.ProjectRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.CreateProject(ctx, project)
	if err != nil {
		return httpapi.ProjectRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toProjectRecord(record), revision, nil
}

// GetProject loads a project with its revision.
func (s *Store) GetProject(ctx context.Context, id string) (httpapi.ProjectRecord, error) {
	record, err := s.pg.GetProjectRecord(ctx, id)
	if err != nil {
		return httpapi.ProjectRecord{}, mapError(err)
	}
	return toProjectRecord(record), nil
}

// ListProjects returns projects ordered by name.
func (s *Store) ListProjects(ctx context.Context) ([]httpapi.ProjectRecord, error) {
	records, err := s.pg.ListProjectRecords(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	projects := make([]httpapi.ProjectRecord, 0, len(records))
	for _, record := range records {
		projects = append(projects, toProjectRecord(record))
	}
	return projects, nil
}

// UpdateProject replaces mutable project fields.
func (s *Store) UpdateProject(ctx context.Context, project domain.Project, expected int64) (httpapi.ProjectRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.UpdateProject(ctx, project, expected)
	if err != nil {
		return httpapi.ProjectRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toProjectRecord(record), revision, nil
}

// DeleteProject removes a project and cascades knowledge rows.
func (s *Store) DeleteProject(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	revision, err := s.pg.DeleteProject(ctx, id, expected)
	return revision, mapError(err)
}

// GetState loads project state with its revision.
func (s *Store) GetState(ctx context.Context, projectID string) (httpapi.StateRecord, error) {
	record, err := s.pg.GetStateRecord(ctx, projectID)
	if err != nil {
		return httpapi.StateRecord{}, mapError(err)
	}
	return toStateRecord(record), nil
}

// PutState replaces project state with an optional optimistic check.
func (s *Store) PutState(ctx context.Context, state domain.State, expected int64) (httpapi.StateRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.PutStateAt(ctx, state, expected)
	if err != nil {
		return httpapi.StateRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toStateRecord(record), revision, nil
}

// CreateNote inserts a note.
func (s *Store) CreateNote(ctx context.Context, note domain.Note) (httpapi.NoteRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.CreateNote(ctx, note)
	if err != nil {
		return httpapi.NoteRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toNoteRecord(record), revision, nil
}

// GetNote loads a note with its revision.
func (s *Store) GetNote(ctx context.Context, id string) (httpapi.NoteRecord, error) {
	record, err := s.pg.GetNote(ctx, id)
	if err != nil {
		return httpapi.NoteRecord{}, mapError(err)
	}
	return toNoteRecord(record), nil
}

// ListNotes returns notes newest first.
func (s *Store) ListNotes(ctx context.Context, opts httpapi.ListOptions) ([]httpapi.NoteRecord, error) {
	records, err := s.pg.ListNotes(ctx, listOptions(opts))
	if err != nil {
		return nil, mapError(err)
	}
	notes := make([]httpapi.NoteRecord, 0, len(records))
	for _, record := range records {
		notes = append(notes, toNoteRecord(record))
	}
	return notes, nil
}

// UpdateNote replaces note content.
func (s *Store) UpdateNote(ctx context.Context, note domain.Note, expected int64) (httpapi.NoteRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.UpdateNote(ctx, note, expected)
	if err != nil {
		return httpapi.NoteRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toNoteRecord(record), revision, nil
}

// DeleteNote removes a note.
func (s *Store) DeleteNote(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	revision, err := s.pg.DeleteNote(ctx, id, expected)
	return revision, mapError(err)
}

// CreateDecision inserts a decision.
func (s *Store) CreateDecision(ctx context.Context, decision domain.Decision) (httpapi.DecisionRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.CreateDecision(ctx, decision)
	if err != nil {
		return httpapi.DecisionRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toDecisionRecord(record), revision, nil
}

// GetDecision loads a decision with its revision.
func (s *Store) GetDecision(ctx context.Context, id string) (httpapi.DecisionRecord, error) {
	record, err := s.pg.GetDecision(ctx, id)
	if err != nil {
		return httpapi.DecisionRecord{}, mapError(err)
	}
	return toDecisionRecord(record), nil
}

// ListDecisions returns decisions newest first.
func (s *Store) ListDecisions(ctx context.Context, opts httpapi.ListOptions) ([]httpapi.DecisionRecord, error) {
	records, err := s.pg.ListDecisions(ctx, listOptions(opts))
	if err != nil {
		return nil, mapError(err)
	}
	decisions := make([]httpapi.DecisionRecord, 0, len(records))
	for _, record := range records {
		decisions = append(decisions, toDecisionRecord(record))
	}
	return decisions, nil
}

// UpdateDecision replaces decision content.
func (s *Store) UpdateDecision(ctx context.Context, decision domain.Decision, expected int64) (httpapi.DecisionRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.UpdateDecision(ctx, decision, expected)
	if err != nil {
		return httpapi.DecisionRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toDecisionRecord(record), revision, nil
}

// DeleteDecision removes a decision.
func (s *Store) DeleteDecision(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	revision, err := s.pg.DeleteDecision(ctx, id, expected)
	return revision, mapError(err)
}

// CreateAttempt inserts an attempt.
func (s *Store) CreateAttempt(ctx context.Context, attempt domain.Attempt) (httpapi.AttemptRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.CreateAttempt(ctx, attempt)
	if err != nil {
		return httpapi.AttemptRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toAttemptRecord(record), revision, nil
}

// GetAttempt loads an attempt with its revision.
func (s *Store) GetAttempt(ctx context.Context, id string) (httpapi.AttemptRecord, error) {
	record, err := s.pg.GetAttempt(ctx, id)
	if err != nil {
		return httpapi.AttemptRecord{}, mapError(err)
	}
	return toAttemptRecord(record), nil
}

// ListAttempts returns attempts newest first.
func (s *Store) ListAttempts(ctx context.Context, opts httpapi.ListOptions) ([]httpapi.AttemptRecord, error) {
	records, err := s.pg.ListAttempts(ctx, listOptions(opts))
	if err != nil {
		return nil, mapError(err)
	}
	attempts := make([]httpapi.AttemptRecord, 0, len(records))
	for _, record := range records {
		attempts = append(attempts, toAttemptRecord(record))
	}
	return attempts, nil
}

// UpdateAttempt replaces attempt content.
func (s *Store) UpdateAttempt(ctx context.Context, attempt domain.Attempt, expected int64) (httpapi.AttemptRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.UpdateAttempt(ctx, attempt, expected)
	if err != nil {
		return httpapi.AttemptRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toAttemptRecord(record), revision, nil
}

// DeleteAttempt removes an attempt.
func (s *Store) DeleteAttempt(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	revision, err := s.pg.DeleteAttempt(ctx, id, expected)
	return revision, mapError(err)
}

// CreateHandoff inserts an immutable handoff.
func (s *Store) CreateHandoff(ctx context.Context, handoff domain.Handoff) (httpapi.HandoffRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.CreateHandoff(ctx, handoff)
	if err != nil {
		return httpapi.HandoffRecord{}, domain.SyncRevision{}, mapError(err)
	}
	return toHandoffRecord(record), revision, nil
}

// GetHandoff loads a handoff with its revision.
func (s *Store) GetHandoff(ctx context.Context, id string) (httpapi.HandoffRecord, error) {
	record, err := s.pg.GetHandoff(ctx, id)
	if err != nil {
		return httpapi.HandoffRecord{}, mapError(err)
	}
	return toHandoffRecord(record), nil
}

// ListHandoffs returns handoffs newest first.
func (s *Store) ListHandoffs(ctx context.Context, opts httpapi.ListOptions) ([]httpapi.HandoffRecord, error) {
	records, err := s.pg.ListHandoffs(ctx, listOptions(opts))
	if err != nil {
		return nil, mapError(err)
	}
	handoffs := make([]httpapi.HandoffRecord, 0, len(records))
	for _, record := range records {
		handoffs = append(handoffs, toHandoffRecord(record))
	}
	return handoffs, nil
}

// DeleteHandoff removes a handoff.
func (s *Store) DeleteHandoff(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	revision, err := s.pg.DeleteHandoff(ctx, id, expected)
	return revision, mapError(err)
}

// CurrentRevision returns the newest server revision.
func (s *Store) CurrentRevision(ctx context.Context) (domain.SyncRevision, error) {
	return s.pg.CurrentRevision(ctx)
}

// ListRoles returns the role reference rows. It is not part of httpapi.Store;
// it exists for bootstrap and admin wiring.
func (s *Store) ListRoles(ctx context.Context) ([]postgres.RoleRecord, error) {
	roles, err := s.pg.ListRoles(ctx)
	return roles, mapError(err)
}

// CreateConnectorRun starts a connector run record.
func (s *Store) CreateConnectorRun(ctx context.Context, run domain.ConnectorRun) (httpapi.ConnectorRunRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.CreateConnectorRun(ctx, run)
	if err != nil {
		return httpapi.ConnectorRunRecord{}, revision, mapError(err)
	}
	return connectorRunRecord(record), revision, nil
}

// FinishConnectorRun records the final state of a connector run.
func (s *Store) FinishConnectorRun(ctx context.Context, id string, status domain.ConnectorStatus, counts map[string]int, errorMessage string, expected int64) (httpapi.ConnectorRunRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.FinishConnectorRun(ctx, id, status, counts, errorMessage, expected)
	if err != nil {
		return httpapi.ConnectorRunRecord{}, revision, mapError(err)
	}
	return connectorRunRecord(record), revision, nil
}

// FinishConnectorRunStats records the final state of an item-level run,
// including its RunStats (nil keeps the stored stats).
func (s *Store) FinishConnectorRunStats(ctx context.Context, id string, status domain.ConnectorStatus, counts map[string]int, stats *domain.RunStats, errorMessage string, expected int64) (httpapi.ConnectorRunRecord, domain.SyncRevision, error) {
	record, revision, err := s.pg.FinishConnectorRunStats(ctx, id, status, counts, stats, errorMessage, expected)
	if err != nil {
		return httpapi.ConnectorRunRecord{}, revision, mapError(err)
	}
	return connectorRunRecord(record), revision, nil
}

// GetConnectorRun loads one connector run.
func (s *Store) GetConnectorRun(ctx context.Context, id string) (httpapi.ConnectorRunRecord, error) {
	record, err := s.pg.GetConnectorRun(ctx, id)
	if err != nil {
		return httpapi.ConnectorRunRecord{}, mapError(err)
	}
	return connectorRunRecord(record), nil
}

// ListConnectorRuns returns connector runs newest first.
func (s *Store) ListConnectorRuns(ctx context.Context, filter httpapi.ConnectorRunFilter) ([]httpapi.ConnectorRunRecord, error) {
	records, err := s.pg.ListConnectorRuns(ctx, postgres.ConnectorRunFilter{
		Source: filter.Source,
		Status: filter.Status,
		Limit:  filter.Limit,
		Offset: filter.Offset,
	})
	if err != nil {
		return nil, mapError(err)
	}
	converted := make([]httpapi.ConnectorRunRecord, 0, len(records))
	for _, record := range records {
		converted = append(converted, connectorRunRecord(record))
	}
	return converted, nil
}

// connectorRunRecord converts a postgres row into the httpapi record type.
func connectorRunRecord(record postgres.ConnectorRunRecord) httpapi.ConnectorRunRecord {
	return httpapi.ConnectorRunRecord{ConnectorRun: record.ConnectorRun, Revision: record.Revision}
}

// DeleteConnectorRun removes a connector run.
func (s *Store) DeleteConnectorRun(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	revision, err := s.pg.DeleteConnectorRun(ctx, id, expected)
	return revision, mapError(err)
}

// RecordSecurityEvent appends a security audit event without a revision.
func (s *Store) RecordSecurityEvent(ctx context.Context, event httpapi.SecurityEvent) error {
	_, err := s.pg.RecordSecurityEvent(ctx, event.Action, event.EntityKind, event.EntityID, event.Detail)
	return mapError(err)
}

// ListAuditEvents lists entity and security audit events newest first.
func (s *Store) ListAuditEvents(ctx context.Context, filter httpapi.AuditFilter) ([]httpapi.AuditEvent, error) {
	records, err := s.pg.ListAuditEvents(ctx, postgres.AuditFilter{
		Actor:      filter.Actor,
		Action:     filter.Action,
		EntityKind: filter.EntityKind,
		EntityID:   filter.EntityID,
		Since:      filter.Since,
		Until:      filter.Until,
		Limit:      filter.Limit,
		Offset:     filter.Offset,
	})
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]httpapi.AuditEvent, 0, len(records))
	for _, record := range records {
		out = append(out, httpapi.AuditEvent{
			ID:         record.ID,
			Revision:   record.Revision,
			Actor:      record.Actor,
			Action:     record.Action,
			EntityKind: record.EntityKind,
			EntityID:   record.EntityID,
			Detail:     record.Detail,
			CreatedAt:  record.CreatedAt,
		})
	}
	return out, nil
}

// ProjectRestrictions maps every restricted project to userID's role in it
// ("" when not a member). Projects absent from the map are open.
func (s *Store) ProjectRestrictions(ctx context.Context, userID string) (map[string]domain.ProjectRole, error) {
	out, err := s.pg.ProjectRestrictions(ctx, userID)
	return out, mapError(err)
}

// GetProjectAccess loads a project's restriction flag and members.
func (s *Store) GetProjectAccess(ctx context.Context, projectID string) (httpapi.ProjectAccessRecord, error) {
	record, err := s.pg.GetProjectAccess(ctx, projectID)
	if err != nil {
		return httpapi.ProjectAccessRecord{}, mapError(err)
	}
	return httpapi.ProjectAccessRecord{ProjectID: record.ProjectID, Restricted: record.Restricted, Members: record.Members}, nil
}

// PutProjectMember adds or updates a membership and restricts the project.
func (s *Store) PutProjectMember(ctx context.Context, member domain.ProjectMember) (domain.ProjectMember, domain.ProjectRole, error) {
	stored, previous, err := s.pg.PutProjectMember(ctx, member)
	return stored, previous, mapError(err)
}

// DeleteProjectMember removes a membership; the project stays restricted.
func (s *Store) DeleteProjectMember(ctx context.Context, projectID, userID string) (domain.ProjectRole, error) {
	role, err := s.pg.DeleteProjectMember(ctx, projectID, userID)
	return role, mapError(err)
}

// SetProjectRestricted restricts or reopens a project.
func (s *Store) SetProjectRestricted(ctx context.Context, projectID string, restricted bool) (bool, error) {
	changed, err := s.pg.SetProjectRestricted(ctx, projectID, restricted)
	return changed, mapError(err)
}
