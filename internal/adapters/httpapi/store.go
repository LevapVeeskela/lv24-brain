package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ListOptions narrows a project-scoped knowledge listing.
type ListOptions struct {
	ProjectID string
	Limit     int
	Offset    int
}

// RevisionFilter narrows the server revision trail.
type RevisionFilter struct {
	AfterID    int64
	EntityKind string
	EntityID   string
	Ascending  bool
	Limit      int
	Offset     int
}

// RevisionRecord is one row of the global server revision trail.
type RevisionRecord struct {
	ID         int64           `json:"id"`
	Kind       domain.SyncKind `json:"kind"`
	Actor      string          `json:"actor,omitempty"`
	EntityKind string          `json:"entityKind,omitempty"`
	EntityID   string          `json:"entityId,omitempty"`
	Action     string          `json:"action,omitempty"`
	Detail     map[string]any  `json:"detail,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// UserRecord is the persisted principal. Auth fields never appear in JSON.
type UserRecord struct {
	domain.User
	PasswordHash string    `json:"-"`
	OIDCIssuer   string    `json:"-"`
	OIDCSubject  string    `json:"-"`
	Revision     int64     `json:"revision"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// APITokenRecord is one scoped API token. TokenHash is never serialized.
type APITokenRecord struct {
	ID         string      `json:"id"`
	UserID     string      `json:"userId"`
	Name       string      `json:"name"`
	Role       domain.Role `json:"role"`
	Prefix     string      `json:"prefix"`
	TokenHash  string      `json:"-"`
	CreatedAt  time.Time   `json:"createdAt"`
	LastUsedAt *time.Time  `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time  `json:"revokedAt,omitempty"`
}

// ProjectRecord is a project plus its current server revision.
type ProjectRecord struct {
	domain.Project
	Revision int64 `json:"revision"`
}

// StateRecord is project state plus its current server revision.
type StateRecord struct {
	domain.State
	Revision int64 `json:"revision"`
}

// NoteRecord is a note plus its current server revision.
type NoteRecord struct {
	domain.Note
	Revision int64 `json:"revision"`
}

// DecisionRecord is a decision plus its current server revision.
type DecisionRecord struct {
	domain.Decision
	Revision int64 `json:"revision"`
}

// AttemptRecord is an attempt plus its current server revision.
type AttemptRecord struct {
	domain.Attempt
	Revision int64 `json:"revision"`
}

// HandoffRecord is a handoff plus its server revision.
type HandoffRecord struct {
	domain.Handoff
	Revision int64 `json:"revision"`
}

// ConflictError reports an optimistic concurrency failure and unwraps to
// domain.ErrConflict.
type ConflictError struct {
	Entity   string
	ID       string
	Expected int64
	Current  int64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s %q: revision conflict: expected %d, current %d", e.Entity, e.ID, e.Expected, e.Current)
}

// Unwrap makes errors.Is(err, domain.ErrConflict) true.
func (e *ConflictError) Unwrap() error { return domain.ErrConflict }

// Store is the persistence contract required by the HTTP API. The in-memory
// implementation serves single-node deployments; a Postgres adapter can
// implement the same contract later without touching handlers.
type Store interface {
	auth.SessionStore

	Ping(ctx context.Context) error

	CreateUser(ctx context.Context, user domain.User, passwordHash string) (UserRecord, domain.SyncRevision, error)
	GetUser(ctx context.Context, id string) (UserRecord, error)
	GetUserByLogin(ctx context.Context, login string) (UserRecord, error)
	GetUserByOIDC(ctx context.Context, issuer, subject string) (UserRecord, error)
	ListUsers(ctx context.Context) ([]UserRecord, error)
	UpdateUser(ctx context.Context, user domain.User, expected int64) (UserRecord, domain.SyncRevision, error)
	SetUserOIDC(ctx context.Context, id, issuer, subject string) (domain.SyncRevision, error)
	DeleteUser(ctx context.Context, id string, expected int64) (domain.SyncRevision, error)

	CreateAPIToken(ctx context.Context, token APITokenRecord) (APITokenRecord, error)
	GetAPIToken(ctx context.Context, id string) (APITokenRecord, error)
	ListAPITokens(ctx context.Context, userID string) ([]APITokenRecord, error)
	RevokeAPIToken(ctx context.Context, id string, now time.Time) error
	FindAPITokenByHash(ctx context.Context, hash string, now time.Time) (APITokenRecord, error)

	CreateProject(ctx context.Context, project domain.Project) (ProjectRecord, domain.SyncRevision, error)
	GetProject(ctx context.Context, id string) (ProjectRecord, error)
	ListProjects(ctx context.Context) ([]ProjectRecord, error)
	UpdateProject(ctx context.Context, project domain.Project, expected int64) (ProjectRecord, domain.SyncRevision, error)
	DeleteProject(ctx context.Context, id string, expected int64) (domain.SyncRevision, error)

	GetState(ctx context.Context, projectID string) (StateRecord, error)
	PutState(ctx context.Context, state domain.State, expected int64) (StateRecord, domain.SyncRevision, error)

	CreateNote(ctx context.Context, note domain.Note) (NoteRecord, domain.SyncRevision, error)
	GetNote(ctx context.Context, id string) (NoteRecord, error)
	ListNotes(ctx context.Context, opts ListOptions) ([]NoteRecord, error)
	UpdateNote(ctx context.Context, note domain.Note, expected int64) (NoteRecord, domain.SyncRevision, error)
	DeleteNote(ctx context.Context, id string, expected int64) (domain.SyncRevision, error)

	CreateDecision(ctx context.Context, decision domain.Decision) (DecisionRecord, domain.SyncRevision, error)
	GetDecision(ctx context.Context, id string) (DecisionRecord, error)
	ListDecisions(ctx context.Context, opts ListOptions) ([]DecisionRecord, error)
	UpdateDecision(ctx context.Context, decision domain.Decision, expected int64) (DecisionRecord, domain.SyncRevision, error)
	DeleteDecision(ctx context.Context, id string, expected int64) (domain.SyncRevision, error)

	CreateAttempt(ctx context.Context, attempt domain.Attempt) (AttemptRecord, domain.SyncRevision, error)
	GetAttempt(ctx context.Context, id string) (AttemptRecord, error)
	ListAttempts(ctx context.Context, opts ListOptions) ([]AttemptRecord, error)
	UpdateAttempt(ctx context.Context, attempt domain.Attempt, expected int64) (AttemptRecord, domain.SyncRevision, error)
	DeleteAttempt(ctx context.Context, id string, expected int64) (domain.SyncRevision, error)

	CreateHandoff(ctx context.Context, handoff domain.Handoff) (HandoffRecord, domain.SyncRevision, error)
	GetHandoff(ctx context.Context, id string) (HandoffRecord, error)
	ListHandoffs(ctx context.Context, opts ListOptions) ([]HandoffRecord, error)
	DeleteHandoff(ctx context.Context, id string, expected int64) (domain.SyncRevision, error)

	Search(ctx context.Context, query ports.Query) ([]ports.Hit, error)
	CurrentRevision(ctx context.Context) (domain.SyncRevision, error)
	ListRevisions(ctx context.Context, filter RevisionFilter) ([]RevisionRecord, error)
}

// revisionValue converts a server revision into its numeric id.
func revisionValue(revision domain.SyncRevision) int64 {
	value, err := strconv.ParseInt(revision.Value, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// authBridge adapts Store to the auth service's narrow interfaces.
type authBridge struct {
	store Store
	now   func() time.Time
}

func (b authBridge) LookupUser(ctx context.Context, login string) (domain.User, string, error) {
	record, err := b.store.GetUserByLogin(ctx, login)
	if err != nil {
		return domain.User{}, "", err
	}
	return record.User, record.PasswordHash, nil
}

func (b authBridge) FindUser(ctx context.Context, id string) (domain.User, error) {
	record, err := b.store.GetUser(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	return record.User, nil
}

func (b authBridge) AuthenticateAPIToken(ctx context.Context, rawToken string) (domain.User, error) {
	now := time.Now
	if b.now != nil {
		now = b.now
	}
	record, err := b.store.FindAPITokenByHash(ctx, auth.HashToken(rawToken), now().UTC())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.User{}, auth.ErrInvalidToken
		}
		return domain.User{}, err
	}
	user, err := b.store.GetUser(ctx, record.UserID)
	if err != nil {
		return domain.User{}, auth.ErrInvalidToken
	}
	principal := user.User
	// A token can narrow but never raise its owner's current privilege.
	if roleRank(record.Role) < roleRank(principal.Role) {
		principal.Role = record.Role
	}
	principal.AuthSource = "api_token"
	return principal, nil
}
