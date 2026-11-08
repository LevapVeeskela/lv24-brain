package httpapi

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// allows reports whether a hit from projectID passes the scope.
func (s ProjectScope) allows(projectID string) bool {
	if len(s.Only) > 0 && !slices.Contains(s.Only, projectID) {
		return false
	}
	return !slices.Contains(s.Exclude, projectID)
}

// ProjectRestrictions maps restricted projects to userID's role in them.
func (m *MemoryStore) ProjectRestrictions(_ context.Context, userID string) (map[string]domain.ProjectRole, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]domain.ProjectRole, len(m.restricted))
	for projectID := range m.restricted {
		out[projectID] = m.members[projectID][userID].Role
	}
	return out, nil
}

// GetProjectAccess loads a project's restriction flag and members.
func (m *MemoryStore) GetProjectAccess(_ context.Context, projectID string) (ProjectAccessRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.projects[projectID]; !ok {
		return ProjectAccessRecord{}, domain.ErrNotFound
	}
	_, restricted := m.restricted[projectID]
	record := ProjectAccessRecord{ProjectID: projectID, Restricted: restricted, Members: []domain.ProjectMember{}}
	for userID, member := range m.members[projectID] {
		member.Login = m.users[userID].Login
		record.Members = append(record.Members, member)
	}
	sort.Slice(record.Members, func(i, j int) bool {
		left, right := strings.ToLower(record.Members[i].Login), strings.ToLower(record.Members[j].Login)
		if left == right {
			return record.Members[i].UserID < record.Members[j].UserID
		}
		return left < right
	})
	return record, nil
}

// PutProjectMember adds or updates a membership and restricts the project.
func (m *MemoryStore) PutProjectMember(_ context.Context, member domain.ProjectMember) (domain.ProjectMember, domain.ProjectRole, error) {
	if err := member.Validate(); err != nil {
		return domain.ProjectMember{}, "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[member.ProjectID]; !ok {
		return domain.ProjectMember{}, "", domain.ErrNotFound
	}
	user, ok := m.users[member.UserID]
	if !ok {
		return domain.ProjectMember{}, "", domain.ErrNotFound
	}
	at := m.nowUTC()
	if _, ok := m.restricted[member.ProjectID]; !ok {
		m.restricted[member.ProjectID] = at
	}
	byUser := m.members[member.ProjectID]
	if byUser == nil {
		byUser = make(map[string]domain.ProjectMember)
		m.members[member.ProjectID] = byUser
	}
	previous, existed := byUser[member.UserID]
	stored := domain.ProjectMember{
		ProjectID: member.ProjectID, UserID: member.UserID, Role: member.Role,
		CreatedAt: at, UpdatedAt: at,
	}
	if existed {
		stored.CreatedAt = previous.CreatedAt
	}
	byUser[member.UserID] = stored
	stored.Login = user.Login
	return stored, previous.Role, nil
}

// DeleteProjectMember removes a membership; the project stays restricted.
func (m *MemoryStore) DeleteProjectMember(_ context.Context, projectID, userID string) (domain.ProjectRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	member, ok := m.members[projectID][userID]
	if !ok {
		return "", domain.ErrNotFound
	}
	delete(m.members[projectID], userID)
	return member.Role, nil
}

// SetProjectRestricted restricts or reopens a project.
func (m *MemoryStore) SetProjectRestricted(_ context.Context, projectID string, restricted bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[projectID]; !ok {
		return false, domain.ErrNotFound
	}
	_, current := m.restricted[projectID]
	if restricted {
		if current {
			return false, nil
		}
		m.restricted[projectID] = m.nowUTC()
		return true, nil
	}
	if count := len(m.members[projectID]); count > 0 {
		return false, fmt.Errorf("reopen project %q: it still has %d member(s): %w", projectID, count, domain.ErrConflict)
	}
	delete(m.restricted, projectID)
	return current, nil
}

// projectDetail adds project_id to a revision detail map when known, so the
// revision trail and event stream can be filtered by project access.
func projectDetail(projectID string, detail map[string]any) map[string]any {
	if projectID == "" {
		return detail
	}
	out := make(map[string]any, len(detail)+1)
	for key, value := range detail {
		out[key] = value
	}
	out[revisionProjectKey] = projectID
	return out
}

const revisionProjectKey = "project_id"
