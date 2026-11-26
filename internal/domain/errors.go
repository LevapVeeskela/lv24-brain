package domain

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrValidation marks invalid user or adapter input.
	ErrValidation = errors.New("validation failed")
	// ErrNotFound marks a missing entity.
	ErrNotFound = errors.New("not found")
	// ErrConflict marks an optimistic concurrency conflict.
	ErrConflict = errors.New("conflict")
	// ErrUnsupported marks a feature or mode the engine does not support.
	ErrUnsupported = errors.New("unsupported")
	// ErrForbidden marks an action the active mode or publish policy refuses
	// before any data is changed (for example a server-mode local-only
	// connector run). It maps to HTTP 403.
	ErrForbidden = errors.New("forbidden")
	// ErrLastAdmin marks a change that would leave no admin account: demoting
	// or deleting the last remaining admin.
	ErrLastAdmin = errors.New("last admin")
	// ErrTargetExists marks a write that would replace a record file that
	// already exists. It is never resolved with a numeric suffix: the caller
	// must surface the conflict and name the existing record.
	ErrTargetExists = errors.New("target exists")
	// ErrDuplicate marks a write refused because another record of the same
	// project and kind already holds the same normalized text. The write is
	// never merged silently: the caller names the existing record and the
	// author decides whether to reuse it or pass the explicit override.
	ErrDuplicate = errors.New("duplicate record")
)

// ValidationError describes one invalid field.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

func (e *ValidationError) Unwrap() error { return ErrValidation }

// NewValidationError builds a typed validation error.
func NewValidationError(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}

// LimitError reports a write whose field is larger than the configured cap.
// Unit is "bytes" or "chars"; Limit and Actual are the effective values, and
// ConfigKey names the brain.yaml setting an operator changes.
type LimitError struct {
	Field     string
	ConfigKey string
	Limit     int
	Actual    int
	Unit      string
}

func (e *LimitError) Error() string {
	unit := e.Unit
	if unit == "" {
		unit = "bytes"
	}
	return fmt.Sprintf("write refused: %s is %d %s, over %s (%d %s)",
		e.Field, e.Actual, unit, e.ConfigKey, e.Limit, unit)
}

func (e *LimitError) Unwrap() error { return ErrValidation }

// ProtectedError reports a write into a project or path the configuration
// protects. ConfigKey names the setting ("limits.protected_projects" or
// "limits.protected_globs") and Pattern is the matching entry.
type ProtectedError struct {
	Kind      string
	ConfigKey string
	Pattern   string
	ProjectID string
	Path      string
}

func (e *ProtectedError) Error() string {
	switch e.Kind {
	case "project":
		return fmt.Sprintf("write to project %q is refused: %s protects it", e.ProjectID, e.ConfigKey)
	case "glob":
		return fmt.Sprintf("write to %q is refused: %s pattern %q protects it", e.Path, e.ConfigKey, e.Pattern)
	default:
		return fmt.Sprintf("write is refused: %s protects it", e.ConfigKey)
	}
}

func (e *ProtectedError) Unwrap() error { return ErrForbidden }

// NewProtectedProjectError builds a typed "this project is protected" error.
func NewProtectedProjectError(projectID string) error {
	return &ProtectedError{Kind: "project", ConfigKey: "limits.protected_projects", ProjectID: projectID}
}

// NewProtectedGlobError builds a typed "this path is protected" error.
func NewProtectedGlobError(recordPath, pattern string) error {
	return &ProtectedError{Kind: "glob", ConfigKey: "limits.protected_globs", Path: recordPath, Pattern: pattern}
}

// NewLimitError builds a typed "over the configured cap" error.
func NewLimitError(field, configKey string, limit, actual int, unit string) error {
	return &LimitError{Field: field, ConfigKey: configKey, Limit: limit, Actual: actual, Unit: unit}
}

// ValidateRequired returns a validation error when value is empty.
func ValidateRequired(field, value string) error {
	if value == "" {
		return NewValidationError(field, "is required")
	}
	return nil
}

// TargetExistsError reports a record that could not be published because its
// identity or file name is already taken. ID names the record that already
// occupies the path, so the user can inspect or pick a different one.
type TargetExistsError struct {
	Kind string
	ID   string
	Path string
}

func (e *TargetExistsError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("%s %q already exists: %v", e.Kind, e.ID, ErrTargetExists)
	}
	return fmt.Sprintf("%s %q already exists at %s: %v", e.Kind, e.ID, e.Path, ErrTargetExists)
}

func (e *TargetExistsError) Unwrap() error { return ErrTargetExists }

// Is keeps the documented ErrConflict contract of record restore: callers that
// already treat a taken record as a conflict keep working.
func (e *TargetExistsError) Is(target error) bool { return target == ErrConflict }

// NewTargetExistsError builds a typed "the name is taken" error.
func NewTargetExistsError(kind, id, path string) error {
	return &TargetExistsError{Kind: kind, ID: id, Path: path}
}

// ProblemError reports a record file the engine could not read as a valid
// record. It is returned instead of a zero record, so a caller can show the
// path and reason and map it to a conflict without touching the file.
type ProblemError struct {
	Path   string
	Kind   string
	Reason string
}

func (e *ProblemError) Error() string {
	return fmt.Sprintf("%s problem at %s: %s", e.Kind, e.Path, e.Reason)
}

// Unwrap matches the problem-file contract of the local API: a conflicted or
// unparsable file is a conflict (409), never a zero record.
func (e *ProblemError) Unwrap() error { return ErrConflict }

// RevisionConflictError reports a record edit refused because the stored
// record changed after the caller read it. CurrentUpdatedAt is the revision the
// caller must send to retry; the conflicting edit wrote nothing.
type RevisionConflictError struct {
	Kind             string
	ID               string
	CurrentUpdatedAt time.Time
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("%s %q changed since it was read: current revision is %s: %v",
		e.Kind, e.ID, e.CurrentRevision(), ErrConflict)
}

// Unwrap keeps the documented conflict contract of a failed compare-and-swap.
func (e *RevisionConflictError) Unwrap() error { return ErrConflict }

// CurrentRevision renders the stored revision in the same RFC3339 form the
// local API exposes as updated_at.
func (e *RevisionConflictError) CurrentRevision() string {
	return e.CurrentUpdatedAt.UTC().Format(time.RFC3339Nano)
}

// NewRevisionConflictError builds a typed record edit conflict.
func NewRevisionConflictError(kind, id string, current time.Time) error {
	return &RevisionConflictError{Kind: kind, ID: id, CurrentUpdatedAt: current}
}

// DuplicateError reports a write whose normalized text already exists in the
// same project and kind. ID names the existing record, so the author can read
// it, supersede it, or repeat the write with the explicit override.
type DuplicateError struct {
	Kind      string
	ID        string
	ProjectID string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("%s %q already records the same text: %v", e.Kind, e.ID, ErrDuplicate)
}

func (e *DuplicateError) Unwrap() error { return ErrDuplicate }
