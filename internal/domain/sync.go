package domain

import (
	"fmt"
	"time"
)

// SyncKind identifies where a revision comes from.
type SyncKind string

const (
	SyncKindGit    SyncKind = "git"
	SyncKindServer SyncKind = "server"
)

func (k SyncKind) Valid() bool {
	return k == SyncKindGit || k == SyncKindServer
}

// SyncRevision is a git commit or server revision used for optimistic checks.
type SyncRevision struct {
	Kind  SyncKind  `json:"kind"`
	Value string    `json:"value"`
	At    time.Time `json:"at"`
}

// Validate checks the revision identity.
func (r SyncRevision) Validate() error {
	if !r.Kind.Valid() {
		return NewValidationError("revision.kind", fmt.Sprintf("unknown kind %q", r.Kind))
	}
	return ValidateRequired("revision.value", r.Value)
}
