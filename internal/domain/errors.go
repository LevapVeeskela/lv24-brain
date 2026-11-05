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
