package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const projectColumns = `id, name, paths, aliases, kind, repo_url, default_branch, revision, created_at, updated_at`

// ProjectRecord is a project plus its current server revision.
type ProjectRecord struct {
	domain.Project
	Revision int64 `json:"revision"`
}

// CreateProject inserts a new project.
func (s *Store) CreateProject(ctx context.Context, project domain.Project) (ProjectRecord, domain.SyncRevision, error) {
	project, err := normalizeProject(project)
	if err != nil {
		return ProjectRecord{}, domain.SyncRevision{}, err
	}

	at := s.nowUTC()
	if project.CreatedAt.IsZero() {
		project.CreatedAt = at
	} else {
		project.CreatedAt = databaseTime(project.CreatedAt)
	}
	project.UpdatedAt = at
	actor := ActorFromContext(ctx)
	record := ProjectRecord{Project: project}
	var revision domain.SyncRevision

	err = s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "project", project.ID, map[string]any{"kind": string(project.Kind)}, at)
		if err != nil {
			return err
		}
		record.Revision = revisionID
		if _, err := tx.Exec(
			ctx,
			`INSERT INTO projects (id, name, paths, aliases, kind, repo_url, default_branch, revision, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			project.ID, project.Name, project.Paths, project.Aliases, string(project.Kind),
			project.RepoURL, project.DefaultBranch, revisionID, project.CreatedAt, project.UpdatedAt,
		); err != nil {
			return mapPgError("create project", err)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "project.create",
			EntityKind: "project", EntityID: project.ID,
			Detail: map[string]any{"kind": string(project.Kind)}, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return ProjectRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

// GetProject loads a project for the ports.Store contract.
func (s *Store) GetProject(ctx context.Context, id string) (domain.Project, error) {
	record, err := s.GetProjectRecord(ctx, id)
	if err != nil {
		return domain.Project{}, err
	}
	return record.Project, nil
}

// GetProjectRecord loads a project together with its revision.
func (s *Store) GetProjectRecord(ctx context.Context, id string) (ProjectRecord, error) {
	if strings.TrimSpace(id) == "" {
		return ProjectRecord{}, domain.NewValidationError("project.id", "is required")
	}
	return getProject(ctx, s.pool, id)
}

// ListProjects returns all projects for the ports.Store contract.
func (s *Store) ListProjects(ctx context.Context) ([]domain.Project, error) {
	records, err := s.ListProjectRecords(ctx)
	if err != nil {
		return nil, err
	}
	projects := make([]domain.Project, 0, len(records))
	for _, record := range records {
		projects = append(projects, record.Project)
	}
	return projects, nil
}

// ListProjectRecords returns all projects ordered by name.
func (s *Store) ListProjectRecords(ctx context.Context) ([]ProjectRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY lower(name), id`)
	if err != nil {
		return nil, mapPgError("list projects", err)
	}
	return collectRows(rows, scanProject)
}

// UpdateProject replaces mutable project fields. expected is the caller's last
// seen revision; zero skips the optimistic check.
func (s *Store) UpdateProject(ctx context.Context, project domain.Project, expected int64) (ProjectRecord, domain.SyncRevision, error) {
	project, err := normalizeProject(project)
	if err != nil {
		return ProjectRecord{}, domain.SyncRevision{}, err
	}

	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var record ProjectRecord
	var revision domain.SyncRevision

	err = s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "project", project.ID, map[string]any{"kind": string(project.Kind)}, at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(
			ctx,
			`UPDATE projects
			 SET name = $1, paths = $2, aliases = $3, kind = $4, repo_url = $5, default_branch = $6,
			     revision = $7, updated_at = $8
			 WHERE id = $9 AND ($10 = 0 OR revision = $10)`,
			project.Name, project.Paths, project.Aliases, string(project.Kind), project.RepoURL,
			project.DefaultBranch, revisionID, at, project.ID, expected,
		)
		if err != nil {
			return mapPgError("update project", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "projects", project.ID, expected)
		}
		updated, err := getProject(ctx, tx, project.ID)
		if err != nil {
			return err
		}
		record = updated
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "project.update",
			EntityKind: "project", EntityID: project.ID,
			Detail: map[string]any{"kind": string(project.Kind)}, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return ProjectRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

// PutProject creates or replaces a project for the ports.Store contract. It is
// an unconditional upsert; use UpdateProject when an optimistic check is
// required.
func (s *Store) PutProject(ctx context.Context, project domain.Project) error {
	project, err := normalizeProject(project)
	if err != nil {
		return err
	}

	at := s.nowUTC()
	if project.CreatedAt.IsZero() {
		project.CreatedAt = at
	} else {
		project.CreatedAt = databaseTime(project.CreatedAt)
	}
	project.UpdatedAt = at
	actor := ActorFromContext(ctx)

	return s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "project", project.ID, map[string]any{"kind": string(project.Kind)}, at)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			ctx,
			`INSERT INTO projects (id, name, paths, aliases, kind, repo_url, default_branch, revision, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			 ON CONFLICT (id) DO UPDATE SET
			     name = EXCLUDED.name,
			     paths = EXCLUDED.paths,
			     aliases = EXCLUDED.aliases,
			     kind = EXCLUDED.kind,
			     repo_url = EXCLUDED.repo_url,
			     default_branch = EXCLUDED.default_branch,
			     revision = EXCLUDED.revision,
			     updated_at = EXCLUDED.updated_at`,
			project.ID, project.Name, project.Paths, project.Aliases, string(project.Kind),
			project.RepoURL, project.DefaultBranch, revisionID, project.CreatedAt, project.UpdatedAt,
		); err != nil {
			return mapPgError("put project", err)
		}
		return insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "project.put",
			EntityKind: "project", EntityID: project.ID,
			Detail: map[string]any{"kind": string(project.Kind)}, At: at,
		})
	})
}

// DeleteProject removes a project and cascades its knowledge rows.
func (s *Store) DeleteProject(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("project.id", "is required")
	}
	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "project", id, nil, at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM projects WHERE id = $1 AND ($2 = 0 OR revision = $2)`, id, expected)
		if err != nil {
			return mapPgError("delete project", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "projects", id, expected)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "project.delete",
			EntityKind: "project", EntityID: id, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return domain.SyncRevision{}, err
	}
	return revision, nil
}

func normalizeProject(project domain.Project) (domain.Project, error) {
	if strings.TrimSpace(project.ID) == "" {
		return domain.Project{}, domain.NewValidationError("project.id", "is required")
	}
	if strings.TrimSpace(project.Name) == "" {
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

func getProject(ctx context.Context, q dbQuerier, id string) (ProjectRecord, error) {
	record, err := scanProject(q.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, id))
	if err != nil {
		return ProjectRecord{}, mapPgError("get project", err)
	}
	return record, nil
}

func scanProject(row rowScanner) (ProjectRecord, error) {
	var record ProjectRecord
	var kind string
	if err := row.Scan(
		&record.ID, &record.Name, &record.Paths, &record.Aliases, &kind,
		&record.RepoURL, &record.DefaultBranch, &record.Revision,
		&record.CreatedAt, &record.UpdatedAt,
	); err != nil {
		return ProjectRecord{}, err
	}
	record.Kind = domain.ProjectKind(kind)
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, nil
}
