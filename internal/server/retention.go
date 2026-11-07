package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/postgres"
)

const (
	// apiTokenExpiredGrace keeps expired API tokens visible to owners and
	// admins for this long after their expiry before they are removed.
	apiTokenExpiredGrace = 30 * 24 * time.Hour
	// retentionInitialDelay postpones the first run so startup stays quick.
	retentionInitialDelay = time.Minute
	// retentionRunTimeout bounds one sweep. A cut-off sweep resumes next run.
	retentionRunTimeout = 30 * time.Minute
)

// retentionJob periodically deletes data past its retention window. It runs
// only against PostgreSQL. Replicas coordinate through an advisory lock inside
// PruneRetained, so at most one sweep runs at a time.
type retentionJob struct {
	store             *postgres.Store
	auditRetention    time.Duration
	revisionRetention time.Duration
	// refreshLifetime is the maximum refresh-token lifetime. Auth tombstones
	// older than this cannot guard any token that is still valid.
	refreshLifetime time.Duration
	interval        time.Duration
	logger          *slog.Logger
	now             func() time.Time
}

func newRetentionJob(store *postgres.Store, cfg Config, logger *slog.Logger) *retentionJob {
	return &retentionJob{
		store:             store,
		auditRetention:    cfg.AuditRetention,
		revisionRetention: cfg.RevisionRetention,
		refreshLifetime:   cfg.RefreshTTL,
		interval:          cfg.RetentionInterval,
		logger:            logger,
		now:               time.Now,
	}
}

// Run sweeps on a fixed interval until ctx is cancelled. A nil job is a no-op
// so memory-mode servers can call it unconditionally.
func (j *retentionJob) Run(ctx context.Context) {
	if j == nil {
		return
	}
	timer := time.NewTimer(retentionInitialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		j.runOnce(ctx)
		timer.Reset(j.interval)
	}
}

func (j *retentionJob) runOnce(ctx context.Context) {
	now := j.now().UTC()
	policy := postgres.RetentionPolicy{
		Audit:         now.Add(-j.auditRetention),
		Revisions:     now.Add(-j.revisionRetention),
		Revocations:   now.Add(-j.refreshLifetime),
		RefreshTokens: now,
		APITokens:     now.Add(-apiTokenExpiredGrace),
	}
	runCtx, cancel := context.WithTimeout(ctx, retentionRunTimeout)
	defer cancel()
	started := time.Now()
	result, err := j.store.PruneRetained(runCtx, policy)
	switch {
	case errors.Is(err, postgres.ErrRetentionBusy):
		j.logger.Debug("retention skipped: another replica is running it")
	case err != nil:
		j.logger.Error("retention failed",
			"error", err,
			"audit_events", result.AuditEvents,
			"revisions", result.Revisions,
			"auth_revocations", result.Revocations,
			"refresh_tokens", result.RefreshTokens,
			"api_tokens", result.APITokens,
		)
	default:
		j.logger.Info("retention completed",
			"audit_events", result.AuditEvents,
			"revisions", result.Revisions,
			"auth_revocations", result.Revocations,
			"refresh_tokens", result.RefreshTokens,
			"api_tokens", result.APITokens,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	}
}
