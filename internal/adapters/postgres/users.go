package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const userColumns = `id, login, display_name, role, auth_source, password_hash, oidc_issuer, oidc_subject, revision, created_at, updated_at`

// UserRecord is the persisted server principal. It carries the password hash
// and OIDC link that are intentionally absent from domain.User. Auth fields
// never appear in JSON output.
type UserRecord struct {
	domain.User
	PasswordHash string    `json:"-"`
	OIDCIssuer   string    `json:"-"`
	OIDCSubject  string    `json:"-"`
	Revision     int64     `json:"revision"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Domain returns the public projection of the user.
func (u UserRecord) Domain() domain.User { return u.User }

// RoleRecord is one row of the roles reference table.
type RoleRecord struct {
	Name        domain.Role `json:"name"`
	Description string      `json:"description,omitempty"`
}

// CreateUser inserts a local or OIDC-linked user. passwordHash is supplied by
// the auth layer (Argon2id) and is stored verbatim; an empty value means the
// account has no local password. An empty role defaults to member and an empty
// auth source defaults to local.
func (s *Store) CreateUser(ctx context.Context, user domain.User, passwordHash string) (UserRecord, domain.SyncRevision, error) {
	if user.ID == "" {
		id, err := newID("user")
		if err != nil {
			return UserRecord{}, domain.SyncRevision{}, err
		}
		user.ID = id
	}
	user, err := normalizeNewUser(user)
	if err != nil {
		return UserRecord{}, domain.SyncRevision{}, err
	}

	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	record := UserRecord{User: user, PasswordHash: passwordHash, CreatedAt: at, UpdatedAt: at}
	var revision domain.SyncRevision

	err = s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "user", user.ID, map[string]any{"role": string(user.Role)}, at)
		if err != nil {
			return err
		}
		record.Revision = revisionID
		if _, err := tx.Exec(
			ctx,
			`INSERT INTO users
			 (id, login, display_name, role, auth_source, password_hash, oidc_issuer, oidc_subject, revision, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			user.ID, user.Login, user.DisplayName, string(user.Role), user.AuthSource, passwordHash, "", "", revisionID, at, at,
		); err != nil {
			return mapPgError("create user", err)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "user.create",
			EntityKind: "user", EntityID: user.ID,
			Detail: map[string]any{"role": string(user.Role)}, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return UserRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

// GetUser loads a user by ID.
func (s *Store) GetUser(ctx context.Context, id string) (UserRecord, error) {
	if err := domain.ValidateRequired("user.id", id); err != nil {
		return UserRecord{}, err
	}
	return getUser(ctx, s.pool, id)
}

// GetUserByLogin loads a user by case-insensitive login.
func (s *Store) GetUserByLogin(ctx context.Context, login string) (UserRecord, error) {
	login = strings.TrimSpace(login)
	if err := domain.ValidateRequired("user.login", login); err != nil {
		return UserRecord{}, err
	}
	record, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE lower(login) = lower($1)`, login))
	if err != nil {
		return UserRecord{}, mapPgError("get user by login", err)
	}
	return record, nil
}

// GetUserByOIDC loads a user by issuer and subject. Both values are required.
func (s *Store) GetUserByOIDC(ctx context.Context, issuer, subject string) (UserRecord, error) {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if err := ValidateOIDCPair(issuer, subject); err != nil {
		return UserRecord{}, err
	}
	if issuer == "" {
		return UserRecord{}, domain.NewValidationError("user.oidcSubject", "is required")
	}
	record, err := scanUser(s.pool.QueryRow(
		ctx,
		`SELECT `+userColumns+` FROM users WHERE oidc_issuer = $1 AND oidc_subject = $2`,
		issuer, subject,
	))
	if err != nil {
		return UserRecord{}, mapPgError("get user by oidc", err)
	}
	return record, nil
}

// ListUsers returns every user ordered by login.
func (s *Store) ListUsers(ctx context.Context) ([]UserRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userColumns+` FROM users ORDER BY lower(login), id`)
	if err != nil {
		return nil, mapPgError("list users", err)
	}
	return collectRows(rows, scanUser)
}

// ListRoles returns the role reference rows.
func (s *Store) ListRoles(ctx context.Context) ([]RoleRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, description FROM roles ORDER BY name`)
	if err != nil {
		return nil, mapPgError("list roles", err)
	}
	defer rows.Close()
	var roles []RoleRecord
	for rows.Next() {
		var name, description string
		if err := rows.Scan(&name, &description); err != nil {
			return nil, mapPgError("scan role", err)
		}
		roles = append(roles, RoleRecord{Name: domain.Role(name), Description: description})
	}
	if err := rows.Err(); err != nil {
		return nil, mapPgError("read roles", err)
	}
	return roles, nil
}

// UpdateUser updates display name, role and auth source. The password hash and
// OIDC link are changed through their dedicated methods. expected is the
// caller's last seen revision; zero skips the optimistic check.
func (s *Store) UpdateUser(ctx context.Context, user domain.User, expected int64) (UserRecord, domain.SyncRevision, error) {
	if err := domain.ValidateRequired("user.id", user.ID); err != nil {
		return UserRecord{}, domain.SyncRevision{}, err
	}
	user, err := normalizeUser(user)
	if err != nil {
		return UserRecord{}, domain.SyncRevision{}, err
	}

	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var record UserRecord
	var revision domain.SyncRevision

	err = s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "user", user.ID, map[string]any{"role": string(user.Role)}, at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(
			ctx,
			`UPDATE users
			 SET display_name = $1, role = $2, auth_source = $3, revision = $4, updated_at = $5
			 WHERE id = $6 AND ($7 = 0 OR revision = $7)`,
			user.DisplayName, string(user.Role), user.AuthSource, revisionID, at, user.ID, expected,
		)
		if err != nil {
			return mapPgError("update user", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "users", user.ID, expected)
		}
		updated, err := getUser(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		record = updated
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "user.update",
			EntityKind: "user", EntityID: user.ID,
			Detail: map[string]any{"role": string(user.Role)}, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return UserRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

// UpdateUserRole changes only the role. It loads the current user first and
// then applies UpdateUser with the caller's expected revision.
func (s *Store) UpdateUserRole(ctx context.Context, id string, role domain.Role, expected int64) (UserRecord, domain.SyncRevision, error) {
	if !role.Valid() {
		return UserRecord{}, domain.SyncRevision{}, domain.NewValidationError("user.role", fmt.Sprintf("unknown role %q", role))
	}
	current, err := s.GetUser(ctx, id)
	if err != nil {
		return UserRecord{}, domain.SyncRevision{}, err
	}
	user := current.User
	user.Role = role
	return s.UpdateUser(ctx, user, expected)
}

// UpdateUserPasswordHash replaces or clears the stored password hash. The
// adapter never hashes or inspects the value; the auth layer owns Argon2id.
func (s *Store) UpdateUserPasswordHash(ctx context.Context, id, passwordHash string, expected int64) (domain.SyncRevision, error) {
	if err := domain.ValidateRequired("user.id", id); err != nil {
		return domain.SyncRevision{}, err
	}
	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "user", id, nil, at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(
			ctx,
			`UPDATE users SET password_hash = $1, revision = $2, updated_at = $3
			 WHERE id = $4 AND ($5 = 0 OR revision = $5)`,
			passwordHash, revisionID, at, id, expected,
		)
		if err != nil {
			return mapPgError("update user password", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "users", id, expected)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "user.password",
			EntityKind: "user", EntityID: id, At: at,
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

// SetUserOIDC links a user to an OIDC issuer/subject pair. Passing empty
// strings clears the link. Subjects are not copied into audit details.
func (s *Store) SetUserOIDC(ctx context.Context, id, issuer, subject string, expected int64) (domain.SyncRevision, error) {
	if err := domain.ValidateRequired("user.id", id); err != nil {
		return domain.SyncRevision{}, err
	}
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if err := ValidateOIDCPair(issuer, subject); err != nil {
		return domain.SyncRevision{}, err
	}

	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "user", id, nil, at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(
			ctx,
			`UPDATE users SET oidc_issuer = $1, oidc_subject = $2, revision = $3, updated_at = $4
			 WHERE id = $5 AND ($6 = 0 OR revision = $6)`,
			issuer, subject, revisionID, at, id, expected,
		)
		if err != nil {
			return mapPgError("set user oidc", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "users", id, expected)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "user.oidc",
			EntityKind: "user", EntityID: id, At: at,
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

// DeleteUser removes a user. Expected revision semantics are the same as for
// updates.
func (s *Store) DeleteUser(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if err := domain.ValidateRequired("user.id", id); err != nil {
		return domain.SyncRevision{}, err
	}
	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "user", id, nil, at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1 AND ($2 = 0 OR revision = $2)`, id, expected)
		if err != nil {
			return mapPgError("delete user", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "users", id, expected)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "user.delete",
			EntityKind: "user", EntityID: id, At: at,
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

// ValidateOIDCPair checks that issuer and subject are either both empty or both
// set.
func ValidateOIDCPair(issuer, subject string) error {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	switch {
	case issuer == "" && subject == "":
		return nil
	case issuer == "":
		return domain.NewValidationError("user.oidcIssuer", "is required when a subject is set")
	case subject == "":
		return domain.NewValidationError("user.oidcSubject", "is required when an issuer is set")
	default:
		return nil
	}
}

func normalizeNewUser(user domain.User) (domain.User, error) {
	if user.Role == "" {
		user.Role = domain.RoleMember
	}
	if strings.TrimSpace(user.AuthSource) == "" {
		user.AuthSource = "local"
	}
	return normalizeUser(user)
}

func normalizeUser(user domain.User) (domain.User, error) {
	user.Login = strings.TrimSpace(user.Login)
	user.AuthSource = strings.TrimSpace(user.AuthSource)
	if user.Role == "" {
		return domain.User{}, domain.NewValidationError("user.role", "is required")
	}
	if user.AuthSource == "" {
		return domain.User{}, domain.NewValidationError("user.authSource", "is required")
	}
	if err := user.Validate(); err != nil {
		return domain.User{}, err
	}
	return user, nil
}

func getUser(ctx context.Context, q dbQuerier, id string) (UserRecord, error) {
	record, err := scanUser(q.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if err != nil {
		return UserRecord{}, mapPgError("get user", err)
	}
	return record, nil
}

func scanUser(row rowScanner) (UserRecord, error) {
	var record UserRecord
	var role string
	if err := row.Scan(
		&record.ID, &record.Login, &record.DisplayName, &role, &record.AuthSource,
		&record.PasswordHash, &record.OIDCIssuer, &record.OIDCSubject,
		&record.Revision, &record.CreatedAt, &record.UpdatedAt,
	); err != nil {
		return UserRecord{}, err
	}
	record.Role = domain.Role(role)
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, nil
}
