package app

import "github.com/levapveeskela/lv24-brain/internal/domain"

// AutoCaptureProvenance is the provenance of a record an agent wrote from its
// own output without an explicit review, such as the session-end handoff or a
// decision captured from the final message. The summary is the agent's stated
// account, not a verified fact, so the trust is stated.
//
// The record is reviewed by default, so the next session still sees the latest
// handoff. With security.quarantine_agent it stays unreviewed and is excluded
// from context until brain record review (or brain_review_record) clears it.
func (a *App) AutoCaptureProvenance() domain.Provenance {
	return domain.Provenance{
		Origin:   domain.OriginAgent,
		Reviewed: !a.cfg.Security.QuarantineAgent,
		Trust:    domain.TrustStated,
	}
}
