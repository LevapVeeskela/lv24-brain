// Package postgres implements the LV24 Brain server-mode persistence layer on
// PostgreSQL via pgx.
//
// The store keeps a globally monotonic server revision. Every write inserts a
// revision row, mutates the entity inside the same transaction, and records an
// audit event. Updates and deletes use the entity's stored revision for
// optimistic concurrency: an expected revision of zero means "no check", any
// positive value must match the current row revision. A mismatch returns
// RevisionConflictError, which unwraps to domain.ErrConflict.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// DatabaseURLEnv names the environment variable holding the server database
	// URL for OpenFromEnv.
	DatabaseURLEnv = "BRAIN_DATABASE_URL"
	// TestDatabaseURLEnv names the environment variable that enables the
	// PostgreSQL integration tests. When it is unset those tests skip.
	TestDatabaseURLEnv = "BRAIN_TEST_DATABASE_URL"
	// SchemaVersion is the newest server schema this engine understands.
	SchemaVersion = 8

	defaultApplicationName = "lv24-brain-server"
	defaultListLimit       = 50
	maxListLimit           = 500
)

var _ ports.Store = (*Store)(nil)

// Options tunes the pgx connection pool. Zero values keep pgx defaults.
type Options struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ApplicationName string
	// StatementTimeout is applied to every connection as the statement_timeout
	// runtime parameter. Zero leaves the PostgreSQL server default in place.
	StatementTimeout time.Duration
}

// Store is the PostgreSQL implementation of the server persistence layer.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// New wraps an existing pgx pool. The caller owns migration and pool lifetime;
// use Open for a managed pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

// Open parses databaseURL, creates a bounded pool, verifies connectivity and
// returns a Store. It never includes the database URL in returned errors
// because a URL may contain credentials.
func Open(ctx context.Context, databaseURL string, opts Options) (*Store, error) {
	if databaseURL == "" {
		return nil, domain.NewValidationError("postgres.url", "is required")
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("postgres: invalid database url")
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	if opts.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = opts.MaxConnLifetime
	}
	if opts.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = opts.MaxConnIdleTime
	}
	applicationName := opts.ApplicationName
	if applicationName == "" {
		applicationName = defaultApplicationName
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = applicationName
	if opts.StatementTimeout > 0 {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return New(pool), nil
}

// OpenFromEnv reads DatabaseURLEnv and calls Open.
func OpenFromEnv(ctx context.Context, opts Options) (*Store, error) {
	databaseURL := os.Getenv(DatabaseURLEnv)
	if databaseURL == "" {
		return nil, fmt.Errorf("postgres: %s is not set", DatabaseURLEnv)
	}
	return Open(ctx, databaseURL, opts)
}

// Pool exposes the underlying pool for health checks and tests. Callers must
// not close it directly; use Close.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Close releases all pooled connections.
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Ping verifies that the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("postgres: store is not configured")
	}
	return s.pool.Ping(ctx)
}

func (s *Store) nowUTC() time.Time {
	if s.now == nil {
		return databaseTime(time.Now())
	}
	return databaseTime(s.now())
}

// databaseTime normalizes a timestamp to the microsecond precision PostgreSQL
// stores. Returning the same value the database will hold keeps created
// records consistent with subsequent reads.
func databaseTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
}

// withTx runs fn inside a read-committed transaction and rolls it back on any
// error. All store writes use this helper so revision and audit rows commit or
// roll back together with the entity change.
func (s *Store) withTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mapPgError("begin transaction", err)
	}
	if err := fn(tx); err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return mapPgError("commit transaction", err)
	}
	return nil
}

// dbQuerier is satisfied by both *pgxpool.Pool and pgx.Tx.
type dbQuerier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// rowScanner is satisfied by pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// WithActor attaches the acting principal to a context. Mutations copy it into
// audit_events; storage methods never log it. It shares ports' context key with
// the HTTP layer, so the authenticated login reaches every revision row.
func WithActor(ctx context.Context, actor string) context.Context {
	return ports.WithActor(ctx, actor)
}

// ActorFromContext returns the actor stored by WithActor, or an empty string.
func ActorFromContext(ctx context.Context) string {
	return ports.ActorFromContext(ctx)
}

// RevisionValue converts a server SyncRevision into its numeric value.
func RevisionValue(revision domain.SyncRevision) (int64, error) {
	if revision.Kind != domain.SyncKindServer {
		return 0, domain.NewValidationError("revision.kind", fmt.Sprintf("unsupported revision kind %q", revision.Kind))
	}
	value, err := strconv.ParseInt(revision.Value, 10, 64)
	if err != nil || value < 0 {
		return 0, domain.NewValidationError("revision.value", "must be a non-negative integer")
	}
	return value, nil
}

// RevisionFromValue builds a server SyncRevision from a numeric value.
func RevisionFromValue(value int64, at time.Time) domain.SyncRevision {
	return domain.SyncRevision{
		Kind:  domain.SyncKindServer,
		Value: strconv.FormatInt(value, 10),
		At:    databaseTime(at),
	}
}

func newID(prefix string) (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate %s id: %w", prefix, err)
	}
	encoded := hex.EncodeToString(raw[:])
	if prefix == "" {
		return encoded, nil
	}
	return prefix + "_" + encoded, nil
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	if limit > maxListLimit {
		return maxListLimit
	}
	return limit
}

func normalizeOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func marshalJSON(value any) ([]byte, error) {
	if value == nil {
		return []byte("{}"), nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode json: %w", err)
	}
	if string(raw) == "null" {
		return []byte("{}"), nil
	}
	return raw, nil
}

func unmarshalJSON[T any](raw []byte, fallback T) (T, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback, nil
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return fallback, fmt.Errorf("decode json: %w", err)
	}
	return value, nil
}

func insertRevisionTx(ctx context.Context, tx pgx.Tx, actor, entityKind, entityID string, detail map[string]any, at time.Time) (int64, error) {
	raw, err := marshalJSON(detail)
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(
		ctx,
		`INSERT INTO revisions (kind, actor, entity_kind, entity_id, detail, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		string(domain.SyncKindServer), actor, entityKind, entityID, raw, at,
	).Scan(&id)
	if err != nil {
		return 0, mapPgError("insert revision", err)
	}
	return id, nil
}

type auditEntry struct {
	Revision   int64
	Actor      string
	Action     string
	EntityKind string
	EntityID   string
	Detail     map[string]any
	At         time.Time
}

func insertAuditTx(ctx context.Context, tx pgx.Tx, entry auditEntry) error {
	_, err := insertAuditEventTx(ctx, tx, entry)
	return err
}

func insertAuditEventTx(ctx context.Context, tx pgx.Tx, entry auditEntry) (int64, error) {
	raw, err := marshalJSON(entry.Detail)
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(
		ctx,
		`INSERT INTO audit_events (revision, actor, action, entity_kind, entity_id, detail, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id`,
		entry.Revision, entry.Actor, entry.Action, entry.EntityKind, entry.EntityID, raw, entry.At,
	).Scan(&id)
	if err != nil {
		return 0, mapPgError("insert audit event", err)
	}
	return id, nil
}

var revisionKeyColumns = map[string]string{
	"users":          "id",
	"projects":       "id",
	"project_state":  "project_id",
	"notes":          "id",
	"decisions":      "id",
	"attempts":       "id",
	"handoffs":       "id",
	"connector_runs": "id",
}

func revisionKeyColumn(table string) (string, error) {
	column, ok := revisionKeyColumns[table]
	if !ok {
		return "", fmt.Errorf("postgres: unsupported revision table %q", table)
	}
	return column, nil
}

// currentRevisionTx reads the stored revision of one entity. The table and key
// column come from the fixed revisionKeyColumns allowlist.
func currentRevisionTx(ctx context.Context, q dbQuerier, table, key string) (int64, error) {
	column, err := revisionKeyColumn(table)
	if err != nil {
		return 0, err
	}
	query := fmt.Sprintf("SELECT revision FROM %s WHERE %s = $1", table, column)
	var revision int64
	if err := q.QueryRow(ctx, query, key).Scan(&revision); err != nil {
		return 0, err
	}
	return revision, nil
}

// revisionConflict describes why a conditional write did not apply.
func revisionConflict(ctx context.Context, q dbQuerier, table, key string, expected int64) error {
	current, err := currentRevisionTx(ctx, q, table, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s %q: %w", table, key, domain.ErrNotFound)
	}
	if err != nil {
		return mapPgError("read revision", err)
	}
	return &RevisionConflictError{
		Entity:   table,
		ID:       key,
		Expected: expected,
		Current:  current,
	}
}
