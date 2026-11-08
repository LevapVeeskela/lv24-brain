package httpapi

import (
	"context"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	defaultListLimit = 50
	maxListLimit     = 500
	defaultSearchCap = 20
	maxSearchCap     = 100
)

// MemoryStore is a concurrency-safe in-memory Store for single-node
// deployments and tests. Data is lost when the process exits.
type MemoryStore struct {
	mu        sync.RWMutex
	now       func() time.Time
	revision  int64
	revisions []RevisionRecord

	users      map[string]UserRecord
	userLogins map[string]string
	userOIDC   map[string]string
	apiTokens  map[string]APITokenRecord

	projects  map[string]ProjectRecord
	states    map[string]StateRecord
	notes     map[string]NoteRecord
	decisions map[string]DecisionRecord
	attempts  map[string]AttemptRecord
	handoffs  map[string]HandoffRecord

	connectorRuns map[string]ConnectorRunRecord

	// restricted marks projects closed to non-members; members holds
	// project id -> user id -> membership.
	restricted map[string]time.Time
	members    map[string]map[string]domain.ProjectMember

	// audit holds security events only (bounded); the in-memory store is a
	// development fallback and does not mirror entity audit rows.
	audit       []AuditEvent
	auditNextID int64

	refresh map[string]auth.RefreshRecord
	// revokedSessions and revokedFamilies mirror the durable PostgreSQL
	// auth_revocations tombstones so a logout racing an in-flight refresh
	// cannot leave a usable token behind in the no-database dev server.
	revokedSessions map[string]struct{}
	revokedFamilies map[string]struct{}
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		now:             time.Now,
		users:           make(map[string]UserRecord),
		userLogins:      make(map[string]string),
		userOIDC:        make(map[string]string),
		apiTokens:       make(map[string]APITokenRecord),
		projects:        make(map[string]ProjectRecord),
		states:          make(map[string]StateRecord),
		notes:           make(map[string]NoteRecord),
		decisions:       make(map[string]DecisionRecord),
		attempts:        make(map[string]AttemptRecord),
		handoffs:        make(map[string]HandoffRecord),
		connectorRuns:   make(map[string]ConnectorRunRecord),
		restricted:      make(map[string]time.Time),
		members:         make(map[string]map[string]domain.ProjectMember),
		refresh:         make(map[string]auth.RefreshRecord),
		revokedSessions: make(map[string]struct{}),
		revokedFamilies: make(map[string]struct{}),
	}
}

var _ Store = (*MemoryStore)(nil)

// Ping always reports readiness for the in-memory store.
func (m *MemoryStore) Ping(context.Context) error { return nil }

func (m *MemoryStore) nowUTC() time.Time {
	return m.now().UTC().Truncate(time.Microsecond)
}

func (m *MemoryStore) nextRevisionLocked(ctx context.Context, entityKind, entityID, action string, detail map[string]any) domain.SyncRevision {
	m.revision++
	at := m.nowUTC()
	m.revisions = append(m.revisions, RevisionRecord{
		ID:         m.revision,
		Kind:       domain.SyncKindServer,
		Actor:      actorFromContext(ctx),
		EntityKind: entityKind,
		EntityID:   entityID,
		Action:     action,
		Detail:     detail,
		CreatedAt:  at,
	})
	return domain.SyncRevision{
		Kind:  domain.SyncKindServer,
		Value: strconv.FormatInt(m.revision, 10),
		At:    at,
	}
}

// CreateUser inserts a user. An empty role defaults to member, an empty auth
// source defaults to local, and an empty ID is generated.
func (m *MemoryStore) CreateUser(ctx context.Context, user domain.User, passwordHash string) (UserRecord, domain.SyncRevision, error) {
	user.Login = strings.TrimSpace(user.Login)
	if user.ID == "" {
		id, err := auth.RandomID("user")
		if err != nil {
			return UserRecord{}, domain.SyncRevision{}, err
		}
		user.ID = id
	}
	if user.Role == "" {
		user.Role = domain.RoleMember
	}
	if strings.TrimSpace(user.AuthSource) == "" {
		user.AuthSource = "local"
	}
	if err := user.Validate(); err != nil {
		return UserRecord{}, domain.SyncRevision{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.users[user.ID]; exists {
		return UserRecord{}, domain.SyncRevision{}, domain.ErrConflict
	}
	loginKey := strings.ToLower(user.Login)
	if _, exists := m.userLogins[loginKey]; exists {
		return UserRecord{}, domain.SyncRevision{}, domain.ErrConflict
	}
	at := m.nowUTC()
	revision := m.nextRevisionLocked(ctx, "user", user.ID, "create", map[string]any{"role": string(user.Role)})
	record := UserRecord{
		User:      user,
		Revision:  revisionValue(revision),
		CreatedAt: at,
		UpdatedAt: at,
	}
	record.PasswordHash = passwordHash
	m.users[user.ID] = record
	m.userLogins[loginKey] = user.ID
	return record, revision, nil
}

// GetUser loads a user by ID.
func (m *MemoryStore) GetUser(_ context.Context, id string) (UserRecord, error) {
	if strings.TrimSpace(id) == "" {
		return UserRecord{}, domain.NewValidationError("user.id", "is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.users[id]
	if !ok {
		return UserRecord{}, domain.ErrNotFound
	}
	return record, nil
}

// GetUserByLogin loads a user by case-insensitive login.
func (m *MemoryStore) GetUserByLogin(_ context.Context, login string) (UserRecord, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return UserRecord{}, domain.NewValidationError("user.login", "is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.userLogins[strings.ToLower(login)]
	if !ok {
		return UserRecord{}, domain.ErrNotFound
	}
	return m.users[id], nil
}

// GetUserByOIDC loads a user by issuer and subject.
func (m *MemoryStore) GetUserByOIDC(_ context.Context, issuer, subject string) (UserRecord, error) {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		return UserRecord{}, domain.ErrNotFound
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.userOIDC[issuer+"\x00"+subject]
	if !ok {
		return UserRecord{}, domain.ErrNotFound
	}
	return m.users[id], nil
}

// ListUsers returns users ordered by login.
func (m *MemoryStore) ListUsers(context.Context) ([]UserRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	users := make([]UserRecord, 0, len(m.users))
	for _, record := range m.users {
		users = append(users, record)
	}
	sort.Slice(users, func(i, j int) bool {
		left := strings.ToLower(users[i].Login)
		right := strings.ToLower(users[j].Login)
		if left == right {
			return users[i].ID < users[j].ID
		}
		return left < right
	})
	return users, nil
}

// UpdateUser updates display name, role and auth source.
func (m *MemoryStore) UpdateUser(ctx context.Context, user domain.User, expected int64) (UserRecord, domain.SyncRevision, error) {
	if strings.TrimSpace(user.ID) == "" {
		return UserRecord{}, domain.SyncRevision{}, domain.NewValidationError("user.id", "is required")
	}
	if user.Role == "" {
		return UserRecord{}, domain.SyncRevision{}, domain.NewValidationError("user.role", "is required")
	}
	if strings.TrimSpace(user.AuthSource) == "" {
		return UserRecord{}, domain.SyncRevision{}, domain.NewValidationError("user.authSource", "is required")
	}
	if err := user.Validate(); err != nil {
		return UserRecord{}, domain.SyncRevision{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.users[user.ID]
	if !ok {
		return UserRecord{}, domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return UserRecord{}, domain.SyncRevision{}, &ConflictError{
			Entity: "user", ID: user.ID, Expected: expected, Current: current.Revision,
		}
	}
	if current.Role == domain.RoleAdmin && user.Role != domain.RoleAdmin && !m.otherAdminLocked(user.ID) {
		return UserRecord{}, domain.SyncRevision{}, domain.ErrLastAdmin
	}
	revision := m.nextRevisionLocked(ctx, "user", user.ID, "update", map[string]any{"role": string(user.Role)})
	current.User.DisplayName = user.DisplayName
	current.User.Role = user.Role
	current.User.AuthSource = user.AuthSource
	current.Revision = revisionValue(revision)
	current.UpdatedAt = revision.At
	m.users[user.ID] = current
	return current, revision, nil
}

// SetUserOIDC links a user to an issuer and subject.
func (m *MemoryStore) SetUserOIDC(ctx context.Context, id, issuer, subject string) (domain.SyncRevision, error) {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		return domain.SyncRevision{}, domain.NewValidationError("user.oidcSubject", "issuer and subject are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.users[id]
	if !ok {
		return domain.SyncRevision{}, domain.ErrNotFound
	}
	key := issuer + "\x00" + subject
	if owner, exists := m.userOIDC[key]; exists && owner != id {
		return domain.SyncRevision{}, domain.ErrConflict
	}
	if current.OIDCIssuer != "" || current.OIDCSubject != "" {
		delete(m.userOIDC, current.OIDCIssuer+"\x00"+current.OIDCSubject)
	}
	current.OIDCIssuer = issuer
	current.OIDCSubject = subject
	revision := m.nextRevisionLocked(ctx, "user", id, "oidc", nil)
	current.Revision = revisionValue(revision)
	current.UpdatedAt = revision.At
	m.users[id] = current
	m.userOIDC[key] = id
	return revision, nil
}

// DeleteUser removes a user.
func (m *MemoryStore) DeleteUser(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("user.id", "is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.users[id]
	if !ok {
		return domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return domain.SyncRevision{}, &ConflictError{
			Entity: "user", ID: id, Expected: expected, Current: current.Revision,
		}
	}
	if current.Role == domain.RoleAdmin && !m.otherAdminLocked(id) {
		return domain.SyncRevision{}, domain.ErrLastAdmin
	}
	revision := m.nextRevisionLocked(ctx, "user", id, "delete", nil)
	delete(m.users, id)
	for _, byUser := range m.members {
		delete(byUser, id)
	}
	delete(m.userLogins, strings.ToLower(current.Login))
	if current.OIDCIssuer != "" || current.OIDCSubject != "" {
		delete(m.userOIDC, current.OIDCIssuer+"\x00"+current.OIDCSubject)
	}
	// A deleted user cannot refresh, so drop its tokens and the tombstones
	// attributable through them. Tombstones for scopes that never stored a row
	// cannot be attributed and are kept; their ids are random and never reused.
	for hash, record := range m.refresh {
		if record.UserID != id {
			continue
		}
		delete(m.refresh, hash)
		delete(m.revokedSessions, record.SessionID)
		delete(m.revokedFamilies, record.FamilyID)
	}
	return revision, nil
}

// otherAdminLocked reports whether any admin other than id exists. The caller
// must hold m.mu.
func (m *MemoryStore) otherAdminLocked(id string) bool {
	for otherID, record := range m.users {
		if otherID != id && record.Role == domain.RoleAdmin {
			return true
		}
	}
	return false
}

// UpdateUserPasswordHash replaces the stored hash of a user.
func (m *MemoryStore) UpdateUserPasswordHash(ctx context.Context, id, passwordHash string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("user.id", "is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.users[id]
	if !ok {
		return domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return domain.SyncRevision{}, &ConflictError{
			Entity: "user", ID: id, Expected: expected, Current: current.Revision,
		}
	}
	revision := m.nextRevisionLocked(ctx, "user", id, "password", nil)
	current.PasswordHash = passwordHash
	current.Revision = revisionValue(revision)
	current.UpdatedAt = revision.At
	m.users[id] = current
	return revision, nil
}

// RevokeUserSessions revokes every refresh-token family of a user except
// exceptFamilyID and records a family tombstone for each one.
func (m *MemoryStore) RevokeUserSessions(_ context.Context, userID, exceptFamilyID string, now time.Time) (int, error) {
	userID = strings.TrimSpace(userID)
	exceptFamilyID = strings.TrimSpace(exceptFamilyID)
	if userID == "" {
		return 0, domain.NewValidationError("user.id", "is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	families := make(map[string]struct{})
	for _, record := range m.refresh {
		if record.UserID != userID || (exceptFamilyID != "" && record.FamilyID == exceptFamilyID) {
			continue
		}
		families[record.FamilyID] = struct{}{}
	}
	at := now.UTC()
	for family := range families {
		m.revokedFamilies[family] = struct{}{}
	}
	for hash, record := range m.refresh {
		if _, ok := families[record.FamilyID]; ok && record.RevokedAt == nil {
			record.RevokedAt = &at
			m.refresh[hash] = record
		}
	}
	return len(families), nil
}

// CreateAPIToken stores a hashed API token.
func (m *MemoryStore) CreateAPIToken(_ context.Context, token APITokenRecord) (APITokenRecord, error) {
	if strings.TrimSpace(token.UserID) == "" {
		return APITokenRecord{}, domain.NewValidationError("token.userId", "is required")
	}
	if strings.TrimSpace(token.Name) == "" {
		return APITokenRecord{}, domain.NewValidationError("token.name", "is required")
	}
	if !token.Role.Valid() {
		return APITokenRecord{}, domain.NewValidationError("token.role", "unknown role")
	}
	if token.TokenHash == "" {
		return APITokenRecord{}, domain.NewValidationError("token.hash", "is required")
	}
	if token.ID == "" {
		id, err := auth.RandomID("token")
		if err != nil {
			return APITokenRecord{}, err
		}
		token.ID = id
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[token.UserID]; !ok {
		return APITokenRecord{}, domain.ErrNotFound
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = m.nowUTC()
	}
	m.apiTokens[token.ID] = token
	return token, nil
}

// GetAPIToken loads one API token record.
func (m *MemoryStore) GetAPIToken(_ context.Context, id string) (APITokenRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.apiTokens[id]
	if !ok {
		return APITokenRecord{}, domain.ErrNotFound
	}
	return record, nil
}

// ListAPITokens returns tokens for one user, or every token when userID is
// empty.
func (m *MemoryStore) ListAPITokens(_ context.Context, userID string) ([]APITokenRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tokens := make([]APITokenRecord, 0, len(m.apiTokens))
	for _, record := range m.apiTokens {
		if userID != "" && record.UserID != userID {
			continue
		}
		tokens = append(tokens, record)
	}
	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].CreatedAt.Equal(tokens[j].CreatedAt) {
			return tokens[i].ID < tokens[j].ID
		}
		return tokens[i].CreatedAt.After(tokens[j].CreatedAt)
	})
	return tokens, nil
}

// RevokeAPIToken marks a token unusable. Repeated revocation is a no-op.
func (m *MemoryStore) RevokeAPIToken(_ context.Context, id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.apiTokens[id]
	if !ok {
		return domain.ErrNotFound
	}
	if record.RevokedAt == nil {
		at := now.UTC()
		record.RevokedAt = &at
		m.apiTokens[id] = record
	}
	return nil
}

// FindAPITokenByHash resolves an active token and records its last use. A
// revoked or expired token (ExpiresAt <= now) is reported as not found.
func (m *MemoryStore) FindAPITokenByHash(_ context.Context, hash string, now time.Time) (APITokenRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, record := range m.apiTokens {
		if record.TokenHash != hash || record.RevokedAt != nil {
			continue
		}
		if record.ExpiresAt != nil && !record.ExpiresAt.After(now) {
			continue
		}
		at := now.UTC()
		record.LastUsedAt = &at
		m.apiTokens[id] = record
		return record, nil
	}
	return APITokenRecord{}, domain.ErrNotFound
}

// CreateProject inserts a project.
func (m *MemoryStore) CreateProject(ctx context.Context, project domain.Project) (ProjectRecord, domain.SyncRevision, error) {
	if project.ID == "" {
		id, err := auth.RandomID("project")
		if err != nil {
			return ProjectRecord{}, domain.SyncRevision{}, err
		}
		project.ID = id
	}
	project, err := normalizeProject(project)
	if err != nil {
		return ProjectRecord{}, domain.SyncRevision{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.projects[project.ID]; exists {
		return ProjectRecord{}, domain.SyncRevision{}, domain.ErrConflict
	}
	at := m.nowUTC()
	if project.CreatedAt.IsZero() {
		project.CreatedAt = at
	}
	project.UpdatedAt = at
	revision := m.nextRevisionLocked(ctx, "project", project.ID, "create", map[string]any{"kind": string(project.Kind)})
	record := ProjectRecord{Project: project, Revision: revisionValue(revision)}
	m.projects[project.ID] = record
	return record, revision, nil
}

// GetProject loads a project with its revision.
func (m *MemoryStore) GetProject(_ context.Context, id string) (ProjectRecord, error) {
	if strings.TrimSpace(id) == "" {
		return ProjectRecord{}, domain.NewValidationError("project.id", "is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.projects[id]
	if !ok {
		return ProjectRecord{}, domain.ErrNotFound
	}
	return record, nil
}

// ListProjects returns projects ordered by name.
func (m *MemoryStore) ListProjects(context.Context) ([]ProjectRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	projects := make([]ProjectRecord, 0, len(m.projects))
	for _, record := range m.projects {
		projects = append(projects, record)
	}
	sort.Slice(projects, func(i, j int) bool {
		left := strings.ToLower(projects[i].Name)
		right := strings.ToLower(projects[j].Name)
		if left == right {
			return projects[i].ID < projects[j].ID
		}
		return left < right
	})
	return projects, nil
}

// UpdateProject replaces mutable project fields.
func (m *MemoryStore) UpdateProject(ctx context.Context, project domain.Project, expected int64) (ProjectRecord, domain.SyncRevision, error) {
	project, err := normalizeProject(project)
	if err != nil {
		return ProjectRecord{}, domain.SyncRevision{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.projects[project.ID]
	if !ok {
		return ProjectRecord{}, domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return ProjectRecord{}, domain.SyncRevision{}, &ConflictError{
			Entity: "project", ID: project.ID, Expected: expected, Current: current.Revision,
		}
	}
	revision := m.nextRevisionLocked(ctx, "project", project.ID, "update", map[string]any{"kind": string(project.Kind)})
	project.CreatedAt = current.CreatedAt
	project.UpdatedAt = revision.At
	record := ProjectRecord{Project: project, Revision: revisionValue(revision)}
	m.projects[project.ID] = record
	return record, revision, nil
}

// DeleteProject removes a project and its project-scoped knowledge.
func (m *MemoryStore) DeleteProject(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("project.id", "is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.projects[id]
	if !ok {
		return domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return domain.SyncRevision{}, &ConflictError{
			Entity: "project", ID: id, Expected: expected, Current: current.Revision,
		}
	}
	revision := m.nextRevisionLocked(ctx, "project", id, "delete", nil)
	delete(m.projects, id)
	delete(m.states, id)
	delete(m.restricted, id)
	delete(m.members, id)
	for noteID, record := range m.notes {
		if record.ProjectID == id {
			delete(m.notes, noteID)
		}
	}
	for decisionID, record := range m.decisions {
		if record.ProjectID == id {
			delete(m.decisions, decisionID)
		}
	}
	for attemptID, record := range m.attempts {
		if record.ProjectID == id {
			delete(m.attempts, attemptID)
		}
	}
	for handoffID, record := range m.handoffs {
		if record.ProjectID == id {
			delete(m.handoffs, handoffID)
		}
	}
	return revision, nil
}

// GetState loads project state.
func (m *MemoryStore) GetState(_ context.Context, projectID string) (StateRecord, error) {
	if strings.TrimSpace(projectID) == "" {
		return StateRecord{}, domain.NewValidationError("state.projectId", "is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.states[projectID]
	if !ok {
		return StateRecord{}, domain.ErrNotFound
	}
	return record, nil
}

// PutState creates or replaces project state with an optional optimistic
// revision check.
func (m *MemoryStore) PutState(ctx context.Context, state domain.State, expected int64) (StateRecord, domain.SyncRevision, error) {
	if err := state.Validate(); err != nil {
		return StateRecord{}, domain.SyncRevision{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[state.ProjectID]; !ok {
		return StateRecord{}, domain.SyncRevision{}, domain.ErrNotFound
	}
	current, exists := m.states[state.ProjectID]
	if expected > 0 {
		if !exists {
			return StateRecord{}, domain.SyncRevision{}, domain.ErrNotFound
		}
		if current.Revision != expected {
			return StateRecord{}, domain.SyncRevision{}, &ConflictError{
				Entity: "state", ID: state.ProjectID, Expected: expected, Current: current.Revision,
			}
		}
	}
	revision := m.nextRevisionLocked(ctx, "project_state", state.ProjectID, "put", nil)
	state.Active = nonNilStrings(state.Active)
	state.Blocked = nonNilStrings(state.Blocked)
	state.OpenQuestions = nonNilStrings(state.OpenQuestions)
	state.UpdatedAt = revision.At
	record := StateRecord{State: state, Revision: revisionValue(revision)}
	m.states[state.ProjectID] = record
	return record, revision, nil
}

// CreateNote inserts a note.
func (m *MemoryStore) CreateNote(ctx context.Context, note domain.Note) (NoteRecord, domain.SyncRevision, error) {
	if note.ID == "" {
		id, err := auth.RandomID("note")
		if err != nil {
			return NoteRecord{}, domain.SyncRevision{}, err
		}
		note.ID = id
	}
	if err := note.Validate(); err != nil {
		return NoteRecord{}, domain.SyncRevision{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[note.ProjectID]; !ok {
		return NoteRecord{}, domain.SyncRevision{}, domain.ErrNotFound
	}
	if _, exists := m.notes[note.ID]; exists {
		return NoteRecord{}, domain.SyncRevision{}, domain.ErrConflict
	}
	at := m.nowUTC()
	note.Tags = nonNilStrings(note.Tags)
	note.CreatedAt = at
	note.UpdatedAt = at
	revision := m.nextRevisionLocked(ctx, "note", note.ID, "create", projectDetail(note.ProjectID, nil))
	record := NoteRecord{Note: note, Revision: revisionValue(revision), CreatedBy: ports.ActorIDFromContext(ctx)}
	m.notes[note.ID] = record
	return record, revision, nil
}

// GetNote loads a note.
func (m *MemoryStore) GetNote(_ context.Context, id string) (NoteRecord, error) {
	if strings.TrimSpace(id) == "" {
		return NoteRecord{}, domain.NewValidationError("note.id", "is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.notes[id]
	if !ok {
		return NoteRecord{}, domain.ErrNotFound
	}
	return record, nil
}

// ListNotes returns notes newest first.
func (m *MemoryStore) ListNotes(_ context.Context, opts ListOptions) ([]NoteRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	records := make([]NoteRecord, 0, len(m.notes))
	for _, record := range m.notes {
		if opts.ProjectID != "" && record.ProjectID != opts.ProjectID {
			continue
		}
		records = append(records, record)
	}
	sortNotes(records)
	return page(records, opts.Limit, opts.Offset), nil
}

// UpdateNote replaces note content.
func (m *MemoryStore) UpdateNote(ctx context.Context, note domain.Note, expected int64) (NoteRecord, domain.SyncRevision, error) {
	if strings.TrimSpace(note.ID) == "" {
		return NoteRecord{}, domain.SyncRevision{}, domain.NewValidationError("note.id", "is required")
	}
	if strings.TrimSpace(note.Body) == "" {
		return NoteRecord{}, domain.SyncRevision{}, domain.NewValidationError("note.body", "is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.notes[note.ID]
	if !ok {
		return NoteRecord{}, domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return NoteRecord{}, domain.SyncRevision{}, &ConflictError{
			Entity: "note", ID: note.ID, Expected: expected, Current: current.Revision,
		}
	}
	revision := m.nextRevisionLocked(ctx, "note", note.ID, "update", projectDetail(current.ProjectID, nil))
	current.Title = note.Title
	current.Body = note.Body
	current.Tags = nonNilStrings(note.Tags)
	current.Permalink = note.Permalink
	current.UpdatedAt = revision.At
	current.Revision = revisionValue(revision)
	m.notes[note.ID] = current
	return current, revision, nil
}

// DeleteNote removes a note.
func (m *MemoryStore) DeleteNote(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("note.id", "is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.notes[id]
	if !ok {
		return domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return domain.SyncRevision{}, &ConflictError{
			Entity: "note", ID: id, Expected: expected, Current: current.Revision,
		}
	}
	revision := m.nextRevisionLocked(ctx, "note", id, "delete", projectDetail(current.ProjectID, nil))
	delete(m.notes, id)
	return revision, nil
}

// CreateDecision inserts a decision.
func (m *MemoryStore) CreateDecision(ctx context.Context, decision domain.Decision) (DecisionRecord, domain.SyncRevision, error) {
	if decision.ID == "" {
		id, err := auth.RandomID("decision")
		if err != nil {
			return DecisionRecord{}, domain.SyncRevision{}, err
		}
		decision.ID = id
	}
	if decision.Status == "" {
		decision.Status = domain.DecisionProposed
	}
	if err := decision.Validate(); err != nil {
		return DecisionRecord{}, domain.SyncRevision{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[decision.ProjectID]; !ok {
		return DecisionRecord{}, domain.SyncRevision{}, domain.ErrNotFound
	}
	if _, exists := m.decisions[decision.ID]; exists {
		return DecisionRecord{}, domain.SyncRevision{}, domain.ErrConflict
	}
	at := m.nowUTC()
	decision.Alternatives = nonNilStrings(decision.Alternatives)
	decision.Consequences = nonNilStrings(decision.Consequences)
	decision.Evidence = nonNilStrings(decision.Evidence)
	decision.CreatedAt = at
	decision.UpdatedAt = at
	revision := m.nextRevisionLocked(ctx, "decision", decision.ID, "create", projectDetail(decision.ProjectID, map[string]any{"status": string(decision.Status)}))
	record := DecisionRecord{Decision: decision, Revision: revisionValue(revision), CreatedBy: ports.ActorIDFromContext(ctx)}
	m.decisions[decision.ID] = record
	return record, revision, nil
}

// GetDecision loads a decision.
func (m *MemoryStore) GetDecision(_ context.Context, id string) (DecisionRecord, error) {
	if strings.TrimSpace(id) == "" {
		return DecisionRecord{}, domain.NewValidationError("decision.id", "is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.decisions[id]
	if !ok {
		return DecisionRecord{}, domain.ErrNotFound
	}
	return record, nil
}

// ListDecisions returns decisions newest first.
func (m *MemoryStore) ListDecisions(_ context.Context, opts ListOptions) ([]DecisionRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	records := make([]DecisionRecord, 0, len(m.decisions))
	for _, record := range m.decisions {
		if opts.ProjectID != "" && record.ProjectID != opts.ProjectID {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].UpdatedAt.Equal(records[j].UpdatedAt) {
			return records[i].ID > records[j].ID
		}
		return records[i].UpdatedAt.After(records[j].UpdatedAt)
	})
	return page(records, opts.Limit, opts.Offset), nil
}

// UpdateDecision replaces decision content.
func (m *MemoryStore) UpdateDecision(ctx context.Context, decision domain.Decision, expected int64) (DecisionRecord, domain.SyncRevision, error) {
	if strings.TrimSpace(decision.ID) == "" {
		return DecisionRecord{}, domain.SyncRevision{}, domain.NewValidationError("decision.id", "is required")
	}
	if decision.Status == "" {
		decision.Status = domain.DecisionProposed
	}
	if err := decision.Validate(); err != nil {
		return DecisionRecord{}, domain.SyncRevision{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.decisions[decision.ID]
	if !ok {
		return DecisionRecord{}, domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return DecisionRecord{}, domain.SyncRevision{}, &ConflictError{
			Entity: "decision", ID: decision.ID, Expected: expected, Current: current.Revision,
		}
	}
	revision := m.nextRevisionLocked(ctx, "decision", decision.ID, "update", projectDetail(current.ProjectID, map[string]any{"status": string(decision.Status)}))
	current.Title = decision.Title
	current.Status = decision.Status
	current.Context = decision.Context
	current.Alternatives = nonNilStrings(decision.Alternatives)
	current.Consequences = nonNilStrings(decision.Consequences)
	current.Evidence = nonNilStrings(decision.Evidence)
	current.Permalink = decision.Permalink
	current.UpdatedAt = revision.At
	current.Revision = revisionValue(revision)
	m.decisions[decision.ID] = current
	return current, revision, nil
}

// DeleteDecision removes a decision.
func (m *MemoryStore) DeleteDecision(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("decision.id", "is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.decisions[id]
	if !ok {
		return domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return domain.SyncRevision{}, &ConflictError{
			Entity: "decision", ID: id, Expected: expected, Current: current.Revision,
		}
	}
	revision := m.nextRevisionLocked(ctx, "decision", id, "delete", projectDetail(current.ProjectID, nil))
	delete(m.decisions, id)
	return revision, nil
}

// CreateAttempt inserts an attempt.
func (m *MemoryStore) CreateAttempt(ctx context.Context, attempt domain.Attempt) (AttemptRecord, domain.SyncRevision, error) {
	if attempt.ID == "" {
		id, err := auth.RandomID("attempt")
		if err != nil {
			return AttemptRecord{}, domain.SyncRevision{}, err
		}
		attempt.ID = id
	}
	if err := attempt.Validate(); err != nil {
		return AttemptRecord{}, domain.SyncRevision{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[attempt.ProjectID]; !ok {
		return AttemptRecord{}, domain.SyncRevision{}, domain.ErrNotFound
	}
	if _, exists := m.attempts[attempt.ID]; exists {
		return AttemptRecord{}, domain.SyncRevision{}, domain.ErrConflict
	}
	at := m.nowUTC()
	attempt.CreatedAt = at
	attempt.UpdatedAt = at
	revision := m.nextRevisionLocked(ctx, "attempt", attempt.ID, "create", projectDetail(attempt.ProjectID, nil))
	record := AttemptRecord{Attempt: attempt, Revision: revisionValue(revision), CreatedBy: ports.ActorIDFromContext(ctx)}
	m.attempts[attempt.ID] = record
	return record, revision, nil
}

// GetAttempt loads an attempt.
func (m *MemoryStore) GetAttempt(_ context.Context, id string) (AttemptRecord, error) {
	if strings.TrimSpace(id) == "" {
		return AttemptRecord{}, domain.NewValidationError("attempt.id", "is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.attempts[id]
	if !ok {
		return AttemptRecord{}, domain.ErrNotFound
	}
	return record, nil
}

// ListAttempts returns attempts newest first.
func (m *MemoryStore) ListAttempts(_ context.Context, opts ListOptions) ([]AttemptRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	records := make([]AttemptRecord, 0, len(m.attempts))
	for _, record := range m.attempts {
		if opts.ProjectID != "" && record.ProjectID != opts.ProjectID {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].UpdatedAt.Equal(records[j].UpdatedAt) {
			return records[i].ID > records[j].ID
		}
		return records[i].UpdatedAt.After(records[j].UpdatedAt)
	})
	return page(records, opts.Limit, opts.Offset), nil
}

// UpdateAttempt replaces attempt content.
func (m *MemoryStore) UpdateAttempt(ctx context.Context, attempt domain.Attempt, expected int64) (AttemptRecord, domain.SyncRevision, error) {
	if strings.TrimSpace(attempt.ID) == "" {
		return AttemptRecord{}, domain.SyncRevision{}, domain.NewValidationError("attempt.id", "is required")
	}
	if err := attempt.Validate(); err != nil {
		return AttemptRecord{}, domain.SyncRevision{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.attempts[attempt.ID]
	if !ok {
		return AttemptRecord{}, domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return AttemptRecord{}, domain.SyncRevision{}, &ConflictError{
			Entity: "attempt", ID: attempt.ID, Expected: expected, Current: current.Revision,
		}
	}
	revision := m.nextRevisionLocked(ctx, "attempt", attempt.ID, "update", projectDetail(current.ProjectID, nil))
	current.Title = attempt.Title
	current.Hypothesis = attempt.Hypothesis
	current.Result = attempt.Result
	current.Reason = attempt.Reason
	current.Permalink = attempt.Permalink
	current.UpdatedAt = revision.At
	current.Revision = revisionValue(revision)
	m.attempts[attempt.ID] = current
	return current, revision, nil
}

// DeleteAttempt removes an attempt.
func (m *MemoryStore) DeleteAttempt(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("attempt.id", "is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.attempts[id]
	if !ok {
		return domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return domain.SyncRevision{}, &ConflictError{
			Entity: "attempt", ID: id, Expected: expected, Current: current.Revision,
		}
	}
	revision := m.nextRevisionLocked(ctx, "attempt", id, "delete", projectDetail(current.ProjectID, nil))
	delete(m.attempts, id)
	return revision, nil
}

// CreateHandoff inserts an immutable handoff.
func (m *MemoryStore) CreateHandoff(ctx context.Context, handoff domain.Handoff) (HandoffRecord, domain.SyncRevision, error) {
	if handoff.ID == "" {
		id, err := auth.RandomID("handoff")
		if err != nil {
			return HandoffRecord{}, domain.SyncRevision{}, err
		}
		handoff.ID = id
	}
	if err := handoff.Validate(); err != nil {
		return HandoffRecord{}, domain.SyncRevision{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[handoff.ProjectID]; !ok {
		return HandoffRecord{}, domain.SyncRevision{}, domain.ErrNotFound
	}
	if _, exists := m.handoffs[handoff.ID]; exists {
		return HandoffRecord{}, domain.SyncRevision{}, domain.ErrConflict
	}
	handoff.CreatedAt = m.nowUTC()
	revision := m.nextRevisionLocked(ctx, "handoff", handoff.ID, "create", projectDetail(handoff.ProjectID, nil))
	record := HandoffRecord{Handoff: handoff, Revision: revisionValue(revision), CreatedBy: ports.ActorIDFromContext(ctx)}
	m.handoffs[handoff.ID] = record
	return record, revision, nil
}

// GetHandoff loads a handoff.
func (m *MemoryStore) GetHandoff(_ context.Context, id string) (HandoffRecord, error) {
	if strings.TrimSpace(id) == "" {
		return HandoffRecord{}, domain.NewValidationError("handoff.id", "is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.handoffs[id]
	if !ok {
		return HandoffRecord{}, domain.ErrNotFound
	}
	return record, nil
}

// ListHandoffs returns handoffs newest first.
func (m *MemoryStore) ListHandoffs(_ context.Context, opts ListOptions) ([]HandoffRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	records := make([]HandoffRecord, 0, len(m.handoffs))
	for _, record := range m.handoffs {
		if opts.ProjectID != "" && record.ProjectID != opts.ProjectID {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID > records[j].ID
		}
		return records[i].CreatedAt.After(records[j].CreatedAt)
	})
	return page(records, opts.Limit, opts.Offset), nil
}

// DeleteHandoff removes a handoff.
func (m *MemoryStore) DeleteHandoff(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("handoff.id", "is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.handoffs[id]
	if !ok {
		return domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return domain.SyncRevision{}, &ConflictError{
			Entity: "handoff", ID: id, Expected: expected, Current: current.Revision,
		}
	}
	revision := m.nextRevisionLocked(ctx, "handoff", id, "delete", projectDetail(current.ProjectID, nil))
	delete(m.handoffs, id)
	return revision, nil
}

// CreateConnectorRun starts a connector run record. An empty ID is generated,
// an empty status defaults to running, and an empty start time defaults to now.
func (m *MemoryStore) CreateConnectorRun(ctx context.Context, run domain.ConnectorRun) (ConnectorRunRecord, domain.SyncRevision, error) {
	if run.ID == "" {
		id, err := auth.RandomID("run")
		if err != nil {
			return ConnectorRunRecord{}, domain.SyncRevision{}, err
		}
		run.ID = id
	}
	if run.Status == "" {
		run.Status = domain.ConnectorRunning
	}
	if err := run.Validate(); err != nil {
		return ConnectorRunRecord{}, domain.SyncRevision{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.connectorRuns[run.ID]; exists {
		return ConnectorRunRecord{}, domain.SyncRevision{}, domain.ErrConflict
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = m.nowUTC()
	} else {
		run.StartedAt = run.StartedAt.UTC()
	}
	if run.Counts == nil {
		run.Counts = map[string]int{}
	}
	revision := m.nextRevisionLocked(ctx, "connector_run", run.ID, "start", map[string]any{"status": string(run.Status)})
	record := cloneConnectorRun(ConnectorRunRecord{ConnectorRun: run, Revision: revisionValue(revision)})
	m.connectorRuns[run.ID] = record
	return cloneConnectorRun(record), revision, nil
}

// FinishConnectorRun records the final status, counts and error of a run.
// expected is the caller's last seen revision; zero skips the check.
func (m *MemoryStore) FinishConnectorRun(ctx context.Context, id string, status domain.ConnectorStatus, counts map[string]int, errorMessage string, expected int64) (ConnectorRunRecord, domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return ConnectorRunRecord{}, domain.SyncRevision{}, domain.NewValidationError("connectorRun.id", "is required")
	}
	if !status.Valid() {
		return ConnectorRunRecord{}, domain.SyncRevision{}, domain.NewValidationError("connectorRun.status", "unknown status")
	}
	if counts == nil {
		counts = map[string]int{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.connectorRuns[id]
	if !ok {
		return ConnectorRunRecord{}, domain.SyncRevision{}, domain.ErrNotFound
	}
	if expected > 0 && current.Revision != expected {
		return ConnectorRunRecord{}, domain.SyncRevision{}, &ConflictError{
			Entity: "connector_run", ID: id, Expected: expected, Current: current.Revision,
		}
	}
	finished := m.nowUTC()
	current.Status = status
	current.Counts = maps.Clone(counts)
	current.Error = errorMessage
	current.FinishedAt = &finished
	revision := m.nextRevisionLocked(ctx, "connector_run", id, "finish", map[string]any{"status": string(status)})
	current.Revision = revisionValue(revision)
	m.connectorRuns[id] = current
	return cloneConnectorRun(current), revision, nil
}

// GetConnectorRun loads one connector run.
func (m *MemoryStore) GetConnectorRun(_ context.Context, id string) (ConnectorRunRecord, error) {
	if strings.TrimSpace(id) == "" {
		return ConnectorRunRecord{}, domain.NewValidationError("connectorRun.id", "is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.connectorRuns[id]
	if !ok {
		return ConnectorRunRecord{}, domain.ErrNotFound
	}
	return cloneConnectorRun(record), nil
}

// ListConnectorRuns returns runs newest first with optional source and status
// filters.
func (m *MemoryStore) ListConnectorRuns(_ context.Context, filter ConnectorRunFilter) ([]ConnectorRunRecord, error) {
	if filter.Status != "" && !filter.Status.Valid() {
		return nil, domain.NewValidationError("connectorRun.status", "unknown status")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	records := make([]ConnectorRunRecord, 0, len(m.connectorRuns))
	for _, record := range m.connectorRuns {
		if filter.Source != "" && record.Source != filter.Source {
			continue
		}
		if filter.Status != "" && record.Status != filter.Status {
			continue
		}
		records = append(records, cloneConnectorRun(record))
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].StartedAt.Equal(records[j].StartedAt) {
			return records[i].ID < records[j].ID
		}
		return records[i].StartedAt.After(records[j].StartedAt)
	})
	return page(records, filter.Limit, filter.Offset), nil
}

func cloneConnectorRun(record ConnectorRunRecord) ConnectorRunRecord {
	record.Counts = maps.Clone(record.Counts)
	if record.FinishedAt != nil {
		finished := *record.FinishedAt
		record.FinishedAt = &finished
	}
	return record
}

// Search performs a case-insensitive substring search across knowledge.
func (m *MemoryStore) Search(ctx context.Context, query ports.Query) ([]ports.Hit, error) {
	return m.SearchScoped(ctx, query, ProjectScope{})
}

// SearchScoped is Search restricted to readable projects before the limit.
func (m *MemoryStore) SearchScoped(_ context.Context, query ports.Query, scope ProjectScope) ([]ports.Hit, error) {
	text := strings.ToLower(strings.TrimSpace(query.Text))
	if text == "" {
		return nil, domain.NewValidationError("search.text", "is required")
	}
	limit := query.Limit
	if limit <= 0 {
		limit = defaultSearchCap
	}
	if limit > maxSearchCap {
		limit = maxSearchCap
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	hits := make([]ports.Hit, 0)
	appendHit := func(id, projectID, kind string, updatedAt time.Time, title, body string, content ...string) {
		if query.ProjectID != "" && projectID != query.ProjectID {
			return
		}
		if !query.AllowsSince(updatedAt) {
			return
		}
		if !scope.allows(projectID) {
			return
		}
		if !query.AllowsKind(kind) {
			return
		}
		combined := strings.ToLower(title + "\n" + body + "\n" + strings.Join(content, "\n"))
		if !strings.Contains(combined, text) {
			return
		}
		score := 1.0
		if strings.Contains(strings.ToLower(title), text) {
			score = 2.0
		}
		hits = append(hits, ports.Hit{
			ID:        id,
			ProjectID: projectID,
			Kind:      kind,
			Title:     title,
			Snippet:   snippet(body, text, 160),
			Score:     score,
		})
	}
	for _, record := range m.notes {
		appendHit(record.ID, record.ProjectID, "note", record.UpdatedAt, record.Title, record.Body)
	}
	for _, record := range m.decisions {
		appendHit(record.ID, record.ProjectID, "decision", record.UpdatedAt, record.Title, record.Context,
			strings.Join(record.Alternatives, "\n"), strings.Join(record.Consequences, "\n"), strings.Join(record.Evidence, "\n"))
	}
	for _, record := range m.attempts {
		appendHit(record.ID, record.ProjectID, "attempt", record.UpdatedAt, record.Title,
			record.Hypothesis+"\n"+record.Result+"\n"+record.Reason)
	}
	for _, record := range m.handoffs {
		appendHit(record.ID, record.ProjectID, "handoff", record.CreatedAt, record.Summary,
			record.GitContext+"\n"+record.NextAction)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// CurrentRevision returns the newest server revision.
func (m *MemoryStore) CurrentRevision(context.Context) (domain.SyncRevision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.revisions) == 0 {
		return domain.SyncRevision{Kind: domain.SyncKindServer, Value: "0"}, nil
	}
	last := m.revisions[len(m.revisions)-1]
	return domain.SyncRevision{
		Kind:  domain.SyncKindServer,
		Value: strconv.FormatInt(last.ID, 10),
		At:    last.CreatedAt,
	}, nil
}

// ListRevisions returns revisions with optional filters.
func (m *MemoryStore) ListRevisions(_ context.Context, filter RevisionFilter) ([]RevisionRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	records := make([]RevisionRecord, 0, len(m.revisions))
	for _, record := range m.revisions {
		if record.ID <= filter.AfterID {
			continue
		}
		if filter.EntityKind != "" && record.EntityKind != filter.EntityKind {
			continue
		}
		if filter.EntityID != "" && record.EntityID != filter.EntityID {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if filter.Ascending {
			return records[i].ID < records[j].ID
		}
		return records[i].ID > records[j].ID
	})
	return page(records, filter.Limit, filter.Offset), nil
}

// CreateRefresh stores a refresh token hash. A token for a tombstoned session
// or family is rejected just like the PostgreSQL store rejects it.
func (m *MemoryStore) CreateRefresh(_ context.Context, record auth.RefreshRecord) error {
	if record.TokenHash == "" || record.SessionID == "" || record.UserID == "" {
		return domain.NewValidationError("refresh", "session, user and hash are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refreshScopeRevokedLocked(record.FamilyID, record.SessionID) {
		return auth.ErrInvalidRefresh
	}
	now := m.nowUTC()
	for hash, existing := range m.refresh {
		if existing.ExpiresAt.Before(now) {
			delete(m.refresh, hash)
		}
	}
	if _, exists := m.refresh[record.TokenHash]; exists {
		return domain.ErrConflict
	}
	m.refresh[record.TokenHash] = record
	return nil
}

// ConsumeRefresh atomically marks an unused refresh token as used.
func (m *MemoryStore) ConsumeRefresh(_ context.Context, tokenHash string, now time.Time) (auth.RefreshRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.refresh[tokenHash]
	if !ok {
		return auth.RefreshRecord{}, auth.ErrInvalidRefresh
	}
	if m.refreshScopeRevokedLocked(record.FamilyID, record.SessionID) ||
		record.RevokedAt != nil || !record.ExpiresAt.After(now) {
		return record, auth.ErrInvalidRefresh
	}
	if record.UsedAt != nil {
		return record, auth.ErrRefreshReused
	}
	at := now.UTC()
	record.UsedAt = &at
	m.refresh[tokenHash] = record
	return record, nil
}

// RevokeSession revokes every token issued for one session and records a
// session tombstone.
func (m *MemoryStore) RevokeSession(_ context.Context, sessionID string, now time.Time) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokedSessions[sessionID] = struct{}{}
	at := now.UTC()
	for hash, record := range m.refresh {
		if record.SessionID == sessionID && record.RevokedAt == nil {
			record.RevokedAt = &at
			m.refresh[hash] = record
		}
	}
	return nil
}

// RevokeFamily revokes every rotated token of one login family and records a
// family tombstone.
func (m *MemoryStore) RevokeFamily(_ context.Context, familyID string, now time.Time) error {
	familyID = strings.TrimSpace(familyID)
	if familyID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokedFamilies[familyID] = struct{}{}
	at := now.UTC()
	for hash, record := range m.refresh {
		if record.FamilyID == familyID && record.RevokedAt == nil {
			record.RevokedAt = &at
			m.refresh[hash] = record
		}
	}
	return nil
}

// refreshScopeRevokedLocked reports whether a session or family tombstone
// exists. The caller must hold m.mu.
func (m *MemoryStore) refreshScopeRevokedLocked(familyID, sessionID string) bool {
	if _, ok := m.revokedFamilies[familyID]; ok {
		return true
	}
	if _, ok := m.revokedSessions[sessionID]; ok {
		return true
	}
	return false
}

func normalizeProject(project domain.Project) (domain.Project, error) {
	project.ID = strings.TrimSpace(project.ID)
	project.Name = strings.TrimSpace(project.Name)
	if project.ID == "" {
		return domain.Project{}, domain.NewValidationError("project.id", "is required")
	}
	if project.Name == "" {
		return domain.Project{}, domain.NewValidationError("project.name", "is required")
	}
	if project.Kind == "" {
		project.Kind = domain.ProjectKindOther
	}
	project.Paths = nonNilStrings(project.Paths)
	project.Aliases = nonNilStrings(project.Aliases)
	if err := project.Validate(); err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

func sortNotes(records []NoteRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].UpdatedAt.Equal(records[j].UpdatedAt) {
			return records[i].ID > records[j].ID
		}
		return records[i].UpdatedAt.After(records[j].UpdatedAt)
	})
}

func page[T any](items []T, limit, offset int) []T {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(items) {
		return []T{}
	}
	items = items[offset:]
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func snippet(body, needle string, max int) string {
	if body == "" {
		return ""
	}
	lower := strings.ToLower(body)
	index := strings.Index(lower, needle)
	if index < 0 {
		index = 0
	}
	start := index - max/3
	if start < 0 {
		start = 0
	}
	end := start + max
	if end > len(body) {
		end = len(body)
		start = end - max
		if start < 0 {
			start = 0
		}
	}
	return strings.TrimSpace(body[start:end])
}

func actorFromContext(ctx context.Context) string {
	return ports.ActorFromContext(ctx)
}

// RecordSecurityEvent appends a security audit event (bounded ring).
func (m *MemoryStore) RecordSecurityEvent(ctx context.Context, event SecurityEvent) error {
	if strings.TrimSpace(event.Action) == "" {
		return domain.NewValidationError("audit.action", "is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditNextID++
	m.audit = append(m.audit, AuditEvent{
		ID:         m.auditNextID,
		Actor:      actorFromContext(ctx),
		Action:     event.Action,
		EntityKind: event.EntityKind,
		EntityID:   event.EntityID,
		Detail:     event.Detail,
		CreatedAt:  m.now().UTC(),
	})
	if overflow := len(m.audit) - maxMemoryAuditEvents; overflow > 0 {
		m.audit = append([]AuditEvent(nil), m.audit[overflow:]...)
	}
	return nil
}

// ListAuditEvents lists recorded security events newest first.
func (m *MemoryStore) ListAuditEvents(_ context.Context, filter AuditFilter) ([]AuditEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AuditEvent, 0)
	for i := len(m.audit) - 1; i >= 0; i-- {
		event := m.audit[i]
		switch {
		case filter.Actor != "" && event.Actor != filter.Actor,
			filter.Action != "" && event.Action != filter.Action,
			filter.EntityKind != "" && event.EntityKind != filter.EntityKind,
			filter.EntityID != "" && event.EntityID != filter.EntityID,
			filter.Since != nil && event.CreatedAt.Before(*filter.Since),
			filter.Until != nil && !event.CreatedAt.Before(*filter.Until):
			continue
		}
		out = append(out, event)
	}
	offset := max(filter.Offset, 0)
	if offset >= len(out) {
		return []AuditEvent{}, nil
	}
	out = out[offset:]
	if limit := clampAuditLimit(filter.Limit); len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// IsRevoked reports whether an access token's family or session was revoked.
func (m *MemoryStore) IsRevoked(_ context.Context, familyID, sessionID string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.revokedFamilies[strings.TrimSpace(familyID)]; ok && strings.TrimSpace(familyID) != "" {
		return true, nil
	}
	if _, ok := m.revokedSessions[strings.TrimSpace(sessionID)]; ok && strings.TrimSpace(sessionID) != "" {
		return true, nil
	}
	return false, nil
}
