package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ReviewRecordOptions selects records whose quarantine to clear and, optionally,
// moves their review date. Exactly one of ID and All is required.
type ReviewRecordOptions struct {
	ProjectID string
	// ID reviews one record.
	ID string
	// All reviews every record that is not reviewed yet. Records with an
	// unknown origin stay quarantined: review cannot repair a malformed
	// origin, so they need a hand edit.
	All bool
	// Trust optionally replaces the trust level of the reviewed records.
	// Empty keeps the stored one.
	Trust domain.Trust
	// ReviewAfter sets the next review date of the record. Nil keeps the
	// stored date.
	ReviewAfter *time.Time
	// ClearReviewAfter removes the review date. It cannot be combined with
	// ReviewAfter.
	ClearReviewAfter bool
}

// ReviewRecordResult lists the records a review now covers.
type ReviewRecordResult struct {
	ProjectID string
	Reviewed  []RecordSummary
}

// ReviewRecords marks one record (or every unreviewed record) reviewed and
// optionally sets its trust. It is idempotent: reviewing an already reviewed
// record without changing its trust writes nothing.
func (a *App) ReviewRecords(ctx context.Context, opts ReviewRecordOptions) (ReviewRecordResult, error) {
	if err := a.requireStore(); err != nil {
		return ReviewRecordResult{}, err
	}
	id := strings.TrimSpace(opts.ID)
	if id != "" && opts.All {
		return ReviewRecordResult{}, domain.NewValidationError("review", "pass either a record id or --all, not both")
	}
	if id == "" && !opts.All {
		return ReviewRecordResult{}, domain.NewValidationError("review", "pass a record id or --all")
	}
	if opts.Trust != "" && !opts.Trust.Valid() {
		return ReviewRecordResult{}, domain.NewValidationError("trust",
			fmt.Sprintf("unknown trust %q (verified, stated, inferred)", opts.Trust))
	}
	if opts.ReviewAfter != nil && opts.ClearReviewAfter {
		return ReviewRecordResult{}, domain.NewValidationError("review-after",
			"pass either --review-after or --clear-review-after, not both")
	}
	if (opts.ReviewAfter != nil || opts.ClearReviewAfter) && id == "" {
		return ReviewRecordResult{}, domain.NewValidationError("review-after",
			"a review date applies to one record; pass its id instead of --all")
	}
	reviewer, ok := a.deps.Store.(ports.RecordReviewer)
	if !ok {
		return ReviewRecordResult{}, fmt.Errorf("%w: reviewing records is not supported by this store", domain.ErrUnsupported)
	}
	var scheduler ports.RecordScheduler
	if opts.ReviewAfter != nil || opts.ClearReviewAfter {
		scheduler, ok = a.deps.Store.(ports.RecordScheduler)
		if !ok {
			return ReviewRecordResult{}, fmt.Errorf("%w: scheduling a review is not supported by this store", domain.ErrUnsupported)
		}
	}
	project, err := a.ensureProject(ctx, opts.ProjectID, false)
	if err != nil {
		return ReviewRecordResult{}, err
	}

	records, err := a.reviewTargets(ctx, project.ID, id, opts.All)
	if err != nil {
		return ReviewRecordResult{}, err
	}
	result := ReviewRecordResult{ProjectID: project.ID, Reviewed: make([]RecordSummary, 0, len(records))}
	for _, record := range records {
		summary := record.Summary()
		changed, err := reviewer.ReviewRecord(ctx, project.ID, summary.Kind, summary.ID, opts.Trust)
		if err != nil {
			return result, err
		}
		if scheduler != nil {
			scheduled, err := scheduler.SetReviewAfter(ctx, project.ID, summary.Kind, summary.ID, reviewAfterValue(opts))
			if err != nil {
				return result, err
			}
			changed = changed || scheduled
		}
		result.Reviewed = append(result.Reviewed, summary)
		if !changed {
			continue
		}
		if err := a.journalWrite(ctx, JournalReview, project.ID, summary.ID,
			summary.Kind+": "+SummarizeLine(summary.Title, 70)); err != nil {
			return result, fmt.Errorf("%s %s was reviewed but the journal append failed: %w", summary.Kind, summary.ID, err)
		}
	}
	return result, nil
}

// reviewAfterValue is the date the schedule editor writes: zero clears it.
func reviewAfterValue(opts ReviewRecordOptions) time.Time {
	if opts.ReviewAfter == nil {
		return time.Time{}
	}
	return *opts.ReviewAfter
}

// reviewTargets loads the records a review call covers, in deterministic
// order.
func (a *App) reviewTargets(ctx context.Context, projectID, id string, all bool) ([]Record, error) {
	if !all {
		record, err := a.GetRecord(ctx, projectID, id)
		if err != nil {
			return nil, err
		}
		return []Record{record}, nil
	}
	records, err := a.projectRecords(ctx, projectID)
	if err != nil {
		return nil, err
	}
	targets := make([]Record, 0, len(records))
	for _, record := range records {
		if record.NeedsReview() {
			targets = append(targets, record)
		}
	}
	sort.SliceStable(targets, func(i, j int) bool {
		return targets[i].Summary().ID < targets[j].Summary().ID
	})
	return targets, nil
}

// NeedsReview reports whether a review clears the record's quarantine.
func (r Record) NeedsReview() bool {
	switch {
	case r.Note != nil:
		return r.Note.NeedsReview()
	case r.Decision != nil:
		return r.Decision.NeedsReview()
	case r.Attempt != nil:
		return r.Attempt.NeedsReview()
	case r.Handoff != nil:
		return r.Handoff.NeedsReview()
	}
	return false
}

// Quarantined reports whether the record is excluded from trusted context:
// an unreviewed write or an origin this engine does not know.
func (r Record) Quarantined() bool {
	switch {
	case r.Note != nil:
		return r.Note.Quarantined()
	case r.Decision != nil:
		return r.Decision.Quarantined()
	case r.Attempt != nil:
		return r.Attempt.Quarantined()
	case r.Handoff != nil:
		return r.Handoff.Quarantined()
	}
	return false
}
