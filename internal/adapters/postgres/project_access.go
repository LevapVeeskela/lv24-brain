package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// ProjectAccessRecord is the access configuration of one project. A project
// that is not Restricted is open to every authenticated user per their global
// role; Members is then always empty.
type ProjectAccessRecord struct {
	ProjectID  string
	Restricted bool
	Members    []domain.ProjectMember
}

// ProjectRestrictions returns every restricted project id mapped to userID's
// role in it, or "" when userID is not a member. Projects absent from the map
// are open. The result is small: one row per restricted project.
func (s *Store) ProjectRestrictions(ctx context.Context, userID string) (map[string]domain.ProjectRole, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.project_id, coalesce(m.role, '')
		FROM project_restrictions r
		LEFT JOIN project_members m ON m.project_id = r.project_id AND m.user_id = $1`, userID)
	if err != nil {
		return nil, mapPgError("list project restrictions", err)
	}
	defer rows.Close()
	out := make(map[string]domain.ProjectRole)
	for rows.Next() {
		var projectID, role string
		if err := rows.Scan(&projectID, &role); err != nil {
			return nil, mapPgError("scan project restriction", err)
		}
		out[projectID] = domain.ProjectRole(role)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPgError("list project restrictions", err)
	}
	return out, nil
}

// GetProjectAccess loads whether a project is restricted and its members
// ordered by login. A missing project is domain.ErrNotFound.
func (s *Store) GetProjectAccess(ctx context.Context, projectID string) (ProjectAccessRecord, error) {
	record := ProjectAccessRecord{ProjectID: projectID, Members: []domain.ProjectMember{}}
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM project_restrictions WHERE project_id = p.id)
		FROM projects p WHERE p.id = $1`, projectID).Scan(&record.Restricted)
	if err != nil {
		return ProjectAccessRecord{}, mapPgError("get project access", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT m.project_id, m.user_id, u.login, m.role, m.created_at, m.updated_at
		FROM project_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.project_id = $1
		ORDER BY lower(u.login), m.user_id`, projectID)
	if err != nil {
		return ProjectAccessRecord{}, mapPgError("list project members", err)
	}
	members, err := collectRows(rows, scanProjectMember)
	if err != nil {
		return ProjectAccessRecord{}, err
	}
	record.Members = append(record.Members, members...)
	return record, nil
}

// PutProjectMember adds or updates a membership and marks the project
// restricted in the same transaction. It returns the stored membership and the
// previous role ("" when the user was not a member). A missing project or user
// is domain.ErrNotFound.
func (s *Store) PutProjectMember(ctx context.Context, member domain.ProjectMember) (domain.ProjectMember, domain.ProjectRole, error) {
	if err := member.Validate(); err != nil {
		return domain.ProjectMember{}, "", err
	}
	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var stored domain.ProjectMember
	var previous domain.ProjectRole
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := requireRowTx(ctx, tx, "project", `SELECT 1 FROM projects WHERE id = $1`, member.ProjectID); err != nil {
			return err
		}
		if err := requireRowTx(ctx, tx, "user", `SELECT 1 FROM users WHERE id = $1`, member.UserID); err != nil {
			return err
		}
		var role string
		err := tx.QueryRow(ctx,
			`SELECT role FROM project_members WHERE project_id = $1 AND user_id = $2 FOR UPDATE`,
			member.ProjectID, member.UserID).Scan(&role)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return mapPgError("read project member", err)
		default:
			previous = domain.ProjectRole(role)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_restrictions (project_id, created_by, created_at)
			VALUES ($1, $2, $3) ON CONFLICT (project_id) DO NOTHING`,
			member.ProjectID, actor, at); err != nil {
			return mapPgError("restrict project", err)
		}
		row := tx.QueryRow(ctx, `
			WITH upserted AS (
				INSERT INTO project_members (project_id, user_id, role, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $4)
				ON CONFLICT (project_id, user_id) DO UPDATE SET role = EXCLUDED.role, updated_at = EXCLUDED.updated_at
				RETURNING project_id, user_id, role, created_at, updated_at
			)
			SELECT up.project_id, up.user_id, u.login, up.role, up.created_at, up.updated_at
			FROM upserted up JOIN users u ON u.id = up.user_id`,
			member.ProjectID, member.UserID, string(member.Role), at)
		record, err := scanProjectMember(row)
		if err != nil {
			return mapPgError("put project member", err)
		}
		stored = record
		return nil
	})
	if err != nil {
		return domain.ProjectMember{}, "", err
	}
	return stored, previous, nil
}

// DeleteProjectMember removes one membership and returns the removed role. The
// project stays restricted even when this was its last member.
func (s *Store) DeleteProjectMember(ctx context.Context, projectID, userID string) (domain.ProjectRole, error) {
	var role string
	err := s.pool.QueryRow(ctx,
		`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2 RETURNING role`,
		projectID, userID).Scan(&role)
	if err != nil {
		return "", mapPgError("delete project member", err)
	}
	return domain.ProjectRole(role), nil
}

// SetProjectRestricted restricts or reopens a project and reports whether the
// state changed. Reopening a project that still has members is
// domain.ErrConflict: remove the members first.
func (s *Store) SetProjectRestricted(ctx context.Context, projectID string, restricted bool) (bool, error) {
	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	changed := false
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := requireRowTx(ctx, tx, "project", `SELECT 1 FROM projects WHERE id = $1 FOR UPDATE`, projectID); err != nil {
			return err
		}
		if restricted {
			tag, err := tx.Exec(ctx, `
				INSERT INTO project_restrictions (project_id, created_by, created_at)
				VALUES ($1, $2, $3) ON CONFLICT (project_id) DO NOTHING`, projectID, actor, at)
			if err != nil {
				return mapPgError("restrict project", err)
			}
			changed = tag.RowsAffected() > 0
			return nil
		}
		var members int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM project_members WHERE project_id = $1`, projectID).Scan(&members); err != nil {
			return mapPgError("count project members", err)
		}
		if members > 0 {
			return fmt.Errorf("reopen project %q: it still has %d member(s): %w", projectID, members, domain.ErrConflict)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM project_restrictions WHERE project_id = $1`, projectID)
		if err != nil {
			return mapPgError("reopen project", err)
		}
		changed = tag.RowsAffected() > 0
		return nil
	})
	return changed, err
}

func requireRowTx(ctx context.Context, tx pgx.Tx, entity, query string, id string) error {
	var one int
	if err := tx.QueryRow(ctx, query, id).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s %q: %w", entity, strings.TrimSpace(id), domain.ErrNotFound)
		}
		return mapPgError("read "+entity, err)
	}
	return nil
}

func scanProjectMember(row rowScanner) (domain.ProjectMember, error) {
	var member domain.ProjectMember
	var role string
	if err := row.Scan(&member.ProjectID, &member.UserID, &member.Login, &role, &member.CreatedAt, &member.UpdatedAt); err != nil {
		return domain.ProjectMember{}, err
	}
	member.Role = domain.ProjectRole(role)
	member.CreatedAt = member.CreatedAt.UTC()
	member.UpdatedAt = member.UpdatedAt.UTC()
	return member, nil
}

// entityProjectTx returns the project of a knowledge row before it is deleted
// so the delete revision can carry project_id (event-stream filtering). A
// missing row returns "" and lets the delete report not-found itself.
func entityProjectTx(ctx context.Context, tx pgx.Tx, table, id string) (string, error) {
	var projectID string
	err := tx.QueryRow(ctx, `SELECT project_id FROM `+pgx.Identifier{table}.Sanitize()+` WHERE id = $1`, id).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", mapPgError("read "+table+" project", err)
	}
	return projectID, nil
}

// projectDetail adds project_id to a revision detail map when known.
func projectDetail(projectID string, detail map[string]any) map[string]any {
	if projectID == "" {
		return detail
	}
	out := make(map[string]any, len(detail)+1)
	for key, value := range detail {
		out[key] = value
	}
	out["project_id"] = projectID
	return out
}
