package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const createMigrationTableSQL = `
CREATE TABLE IF NOT EXISTS brain_schema_migrations (
    version integer PRIMARY KEY,
    name text NOT NULL,
    checksum text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

const migrationLockSQL = `SELECT pg_advisory_lock(hashtext('lv24_brain_migrations'))`
const migrationUnlockSQL = `SELECT pg_advisory_unlock(hashtext('lv24_brain_migrations'))`

// Migration is one embedded, forward-only schema migration.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// MigrationResult reports which migration versions were applied, which already
// existed, and the resulting schema version.
type MigrationResult struct {
	Applied  []int
	Existing []int
	Version  int
}

var migrationNamePattern = regexp.MustCompile(`^([0-9]{4})_([a-z0-9][a-z0-9_]*)$`)

// LoadMigrations reads, parses, checksums and sorts the embedded migrations.
// Files must be named NNNN_lowercase_name.sql and versions must be unique.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	seen := map[int]string{}
	migrations := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		version, name, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, err
		}
		if previous, ok := seen[version]; ok {
			return nil, fmt.Errorf("postgres: duplicate migration version %d (%s and %s)", version, previous, entry.Name())
		}
		seen[version] = entry.Name()
		raw, err := fs.ReadFile(migrationsFS, path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(raw)
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     name,
			SQL:      string(raw),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	if len(migrations) == 0 {
		return nil, errors.New("postgres: no embedded migrations found")
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	if highest := migrations[len(migrations)-1].Version; highest != SchemaVersion {
		return nil, fmt.Errorf("postgres: embedded migration version %d does not match SchemaVersion %d", highest, SchemaVersion)
	}
	return migrations, nil
}

func parseMigrationName(fileName string) (int, string, error) {
	if !strings.HasSuffix(fileName, ".sql") {
		return 0, "", fmt.Errorf("postgres: migration %q must end with .sql", fileName)
	}
	base := strings.TrimSuffix(fileName, ".sql")
	matches := migrationNamePattern.FindStringSubmatch(base)
	if matches == nil {
		return 0, "", fmt.Errorf("postgres: migration %q must match NNNN_lowercase_name.sql", fileName)
	}
	version, err := strconv.Atoi(matches[1])
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("postgres: migration %q has an invalid version", fileName)
	}
	return version, matches[2], nil
}

// Migrate applies all pending embedded migrations in order. It is idempotent,
// serialized by a PostgreSQL advisory lock, and never runs a destructive down
// migration. Each migration and its version row commit atomically. A checksum
// change for an already-applied migration is treated as schema drift.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (MigrationResult, error) {
	if pool == nil {
		return MigrationResult{}, errors.New("postgres: nil pool")
	}
	migrations, err := LoadMigrations()
	if err != nil {
		return MigrationResult{}, err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return MigrationResult{}, mapPgError("acquire migration connection", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, migrationLockSQL); err != nil {
		return MigrationResult{}, mapPgError("acquire migration lock", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, migrationUnlockSQL)
	}()

	if _, err := conn.Exec(ctx, createMigrationTableSQL); err != nil {
		return MigrationResult{}, mapPgError("create migration table", err)
	}

	applied := map[int]string{}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM brain_schema_migrations ORDER BY version`)
	if err != nil {
		return MigrationResult{}, mapPgError("read migration versions", err)
	}
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return MigrationResult{}, mapPgError("scan migration version", err)
		}
		applied[version] = checksum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MigrationResult{}, mapPgError("read migration versions", err)
	}

	known := make(map[int]struct{}, len(migrations))
	for _, migration := range migrations {
		known[migration.Version] = struct{}{}
	}
	for version := range applied {
		if _, ok := known[version]; !ok {
			return MigrationResult{}, fmt.Errorf("postgres: database schema version %d is newer than this engine supports (%d)", version, SchemaVersion)
		}
	}

	result := MigrationResult{}
	for _, migration := range migrations {
		if checksum, ok := applied[migration.Version]; ok {
			if checksum != migration.Checksum {
				return result, fmt.Errorf("postgres: migration %04d_%s checksum mismatch: applied migrations are immutable", migration.Version, migration.Name)
			}
			result.Existing = append(result.Existing, migration.Version)
			result.Version = migration.Version
			continue
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return result, mapPgError("begin migration transaction", err)
		}
		if _, err := tx.Exec(ctx, migration.SQL); err != nil {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = tx.Rollback(rollbackCtx)
			cancel()
			return result, fmt.Errorf("apply migration %04d_%s: %w", migration.Version, migration.Name, mapPgError("execute migration", err))
		}
		if _, err := tx.Exec(
			ctx,
			`INSERT INTO brain_schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			migration.Version, migration.Name, migration.Checksum,
		); err != nil {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = tx.Rollback(rollbackCtx)
			cancel()
			return result, mapPgError("record migration version", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return result, mapPgError("commit migration", err)
		}
		result.Applied = append(result.Applied, migration.Version)
		result.Version = migration.Version
	}
	return result, nil
}

// Migrate applies pending migrations using the store's pool.
func (s *Store) Migrate(ctx context.Context) (MigrationResult, error) {
	if s == nil || s.pool == nil {
		return MigrationResult{}, errors.New("postgres: store is not configured")
	}
	return Migrate(ctx, s.pool)
}
