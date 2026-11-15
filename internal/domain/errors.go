package domain

import (
	"errors"
	"fmt"
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
