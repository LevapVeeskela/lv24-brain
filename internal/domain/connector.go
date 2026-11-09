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
//
// Legacy runs (ports.Connector) fill Source and Counts only. Item-level runs
// (ports.Source) also set ConnectionID and Stats; Source then holds the
// provider name so existing listings by source keep working. Both new fields
// are omitted from JSON when empty, so legacy records are byte-identical.
type ConnectorRun struct {
	ID         string          `json:"id"`
	Source     string          `json:"source"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
	Status     ConnectorStatus `json:"status"`
	Counts     map[string]int  `json:"counts,omitempty"`
	Error      string          `json:"error,omitempty"`
	// ConnectionID is the Connection.ID of an item-level run.
	ConnectionID string `json:"connectionId,omitempty"`
	// Stats are item-level counts of an item-level run.
	Stats *RunStats `json:"stats,omitempty"`
}

// Validate checks required connector run fields.
func (c ConnectorRun) Validate() error {
	if err := ValidateRequired("connectorRun.source", c.Source); err != nil {
		return err
	}
	if !c.Status.Valid() {
		return NewValidationError("connectorRun.status", fmt.Sprintf("unknown status %q", c.Status))
	}
	if c.ConnectionID != "" {
		if err := ValidateConnectionID("connectorRun.connectionId", c.ConnectionID); err != nil {
			return err
		}
	}
	if c.Stats != nil {
		if err := c.Stats.Validate(); err != nil {
			return err
		}
	}
	return nil
}
