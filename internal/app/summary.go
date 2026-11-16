package app

import "context"

// QuarantinedCount returns how many curated records of a project are excluded
// from trusted context because they are unreviewed non-human writes or carry an
// origin this engine does not know. It is the exact size of the quarantine
// queue, not a sample.
func (a *App) QuarantinedCount(ctx context.Context, projectID string) (int, error) {
	if err := a.requireStore(); err != nil {
		return 0, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return 0, err
	}
	records, err := a.projectRecords(ctx, project.ID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, record := range records {
		if record.Quarantined() {
			count++
		}
	}
	return count, nil
}

// Summarize returns the listing form of one record with its effective review
// date. Summary alone cannot know the date because the default depends on the
// configured review windows.
func (a *App) Summarize(record Record) RecordSummary {
	summary := record.Summary()
	summary.ReviewAfter, summary.ReviewSource = a.reviewAfter(summary.Kind, record.ReviewSchedule(), summary.CreatedAt)
	return summary
}
