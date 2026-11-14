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
