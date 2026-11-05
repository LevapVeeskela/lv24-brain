package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// RevisionConflictError reports an optimistic concurrency failure. It unwraps
// to domain.ErrConflict so HTTP and CLI layers can map it to a conflict
// response while still returning the current revision to the caller.
type RevisionConflictError struct {
	Entity   string
	ID       string
	Expected int64
	Current  int64
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("%s %q: revision conflict: expected %d, current %d", e.Entity, e.ID, e.Expected, e.Current)
}

// Unwrap makes errors.Is(err, domain.ErrConflict) true.
func (e *RevisionConflictError) Unwrap() error { return domain.ErrConflict }

// mapPgError converts low-level pgx errors into typed domain errors. It never
// copies PostgreSQL detail or hint text because those may echo row values;
// only the SQLSTATE and constraint name are safe to include.
func mapPgError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return constraintError(op, domain.ErrConflict, pgErr.ConstraintName)
		case "23503": // foreign_key_violation
			return constraintError(op, domain.ErrValidation, pgErr.ConstraintName)
		case "23502": // not_null_violation
			return constraintError(op, domain.ErrValidation, pgErr.ColumnName)
		case "23514": // check_violation
			return constraintError(op, domain.ErrValidation, pgErr.ConstraintName)
		case "22P02": // invalid_text_representation
			return constraintError(op, domain.ErrValidation, "")
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

func constraintError(op string, target error, name string) error {
	if name == "" {
		return fmt.Errorf("%s: %w", op, target)
	}
	return fmt.Errorf("%s: %w (constraint %s)", op, target, name)
}
