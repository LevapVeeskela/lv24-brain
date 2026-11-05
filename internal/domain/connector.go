package domain

import (
	"fmt"
	"time"
)

// ConnectorStatus is the outcome of one connector run.
type ConnectorStatus string

const (
	ConnectorRunning ConnectorStatus = "running"
	ConnectorSuccess ConnectorStatus = "success"
	ConnectorPartial ConnectorStatus = "partial"
	ConnectorFailed  ConnectorStatus = "failed"
)

func (s ConnectorStatus) Valid() bool {
	switch s {
	case ConnectorRunning, ConnectorSuccess, ConnectorPartial, ConnectorFailed:
		return true
	default:
		return false
	}
}

// ConnectorRun records one connector execution, including counts and errors.
type ConnectorRun struct {
	ID         string          `json:"id"`
	Source     string          `json:"source"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
	Status     ConnectorStatus `json:"status"`
	Counts     map[string]int  `json:"counts,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// Validate checks required connector run fields.
func (c ConnectorRun) Validate() error {
	if err := ValidateRequired("connectorRun.source", c.Source); err != nil {
		return err
	}
	if !c.Status.Valid() {
		return NewValidationError("connectorRun.status", fmt.Sprintf("unknown status %q", c.Status))
	}
	return nil
}
