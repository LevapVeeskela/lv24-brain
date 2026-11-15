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

// SupersedeResult reports both ends of a supersede link.
type SupersedeResult struct {
	Old RecordSummary
	New RecordSummary
	// Changed is false for an idempotent repeat of the same link.
	Changed bool
}

// Supersede marks oldID as replaced by newID: the old record gets valid_to and
// superseded_by (and a decision also gets status superseded), the new record
// gets supersedes. Both files stay in git, so the history is the audit log.
// The call is idempotent; conflicting links and cycles are validation errors.
func (a *App) Supersede(ctx context.Context, projectID, oldID, newID string) (SupersedeResult, error) {
	if err := a.requireStore(); err != nil {
		return SupersedeResult{}, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return SupersedeResult{}, err
	}
	changed, err := a.linkSupersede(ctx, project.ID, oldID, newID)
	if err != nil {
		return SupersedeResult{}, err
	}
	old, err := a.GetRecord(ctx, project.ID, oldID)
	if err != nil {
		return SupersedeResult{}, err
	}
	replacement, err := a.GetRecord(ctx, project.ID, newID)
	if err != nil {
		return SupersedeResult{}, err
	}
	return SupersedeResult{Old: old.Summary(), New: replacement.Summary(), Changed: changed}, nil
}

// SupersedeChain returns the records connected to id by supersede links, in
// creation order. A record without links returns just itself.
func (a *App) SupersedeChain(ctx context.Context, projectID, id string) ([]RecordSummary, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return nil, err
	}
	records, err := a.projectRecords(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	index := make(map[string]Record, len(records))
	for _, record := range records {
		index[record.Summary().ID] = record
	}
	id = strings.TrimSpace(id)
	if _, ok := index[id]; !ok {
		return nil, fmt.Errorf("record %q in project %s: %w", id, project.ID, domain.ErrNotFound)
	}
	seen := map[string]bool{id: true}
	queue := []string{id}
	chain := make([]RecordSummary, 0, 3)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		record := index[current]
		chain = append(chain, record.Summary())
		validity := record.Validity()
		for _, next := range []string{validity.SupersededBy, validity.Supersedes} {
			next = strings.TrimSpace(next)
			if next == "" || seen[next] {
				continue
			}
			if _, ok := index[next]; !ok {
				continue
			}
			seen[next] = true
			queue = append(queue, next)
		}
	}
	sort.SliceStable(chain, func(i, j int) bool {
		if !chain[i].CreatedAt.Equal(chain[j].CreatedAt) {
			return chain[i].CreatedAt.Before(chain[j].CreatedAt)
		}
		return chain[i].ID < chain[j].ID
	})
	return chain, nil
}

// linkSupersede marks oldID replaced by newID and reports whether anything
// changed. The old record is ended first: if the second write fails, the chain
// still says the old record is no longer current and a retry finishes it.
func (a *App) linkSupersede(ctx context.Context, projectID, oldID, newID string) (bool, error) {
	oldID, newID = strings.TrimSpace(oldID), strings.TrimSpace(newID)
	if oldID == "" || newID == "" {
		return false, domain.NewValidationError("supersede", "both the old and the new record id are required")
	}
	if oldID == newID {
		return false, domain.NewValidationError("by", "a record cannot supersede itself")
	}
	if err := domain.ValidateRecordReference("id", oldID); err != nil {
		return false, err
	}
	if err := domain.ValidateRecordReference("by", newID); err != nil {
		return false, err
	}
	records, err := a.projectRecords(ctx, projectID)
	if err != nil {
		return false, err
	}
	index := make(map[string]Record, len(records))
	for _, record := range records {
		index[record.Summary().ID] = record
	}
	old, ok := index[oldID]
	if !ok {
		return false, fmt.Errorf("old record %q in project %s: %w", oldID, projectID, domain.ErrNotFound)
	}
	replacement, ok := index[newID]
	if !ok {
		return false, fmt.Errorf("new record %q in project %s: %w", newID, projectID, domain.ErrNotFound)
	}
	if current := strings.TrimSpace(old.Validity().SupersededBy); current != "" && current != newID {
		return false, domain.NewValidationError("id",
			fmt.Sprintf("%s is already superseded by %s; supersede that record instead", oldID, current))
	}
	if current := strings.TrimSpace(replacement.Validity().Supersedes); current != "" && current != oldID {
		return false, domain.NewValidationError("by",
			fmt.Sprintf("%s already supersedes %s", newID, current))
	}
	if err := checkSupersedeCycle(index, oldID, newID); err != nil {
		return false, err
	}
	// Check both ends before the first write: a cap or protection refusal on
	// the replacement must not leave the old record already ended.
	for _, record := range []Record{old, replacement} {
		summary := record.Summary()
		if err := a.enforceUpdateLimits(projectID, summary.Kind, summary.ID, recordWriteContent(record)); err != nil {
			return false, err
		}
	}

	changed := false
	if old.Validity().SupersededBy != newID || old.Validity().ValidTo.IsZero() {
		if err := a.markSuperseded(ctx, projectID, old, newID); err != nil {
			return changed, err
		}
		changed = true
	}
	if replacement.Validity().Supersedes != oldID {
		if err := a.markSupersedes(ctx, projectID, replacement, oldID); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// markSuperseded ends a record and points it at its replacement.
func (a *App) markSuperseded(ctx context.Context, projectID string, record Record, newID string) error {
	validTo := a.supersedeTime(record)
	if record.Decision != nil {
		_, err := a.updateDecision(ctx, projectID, record.Decision.ID, func(decision *domain.Decision) error {
			decision.Validity.ValidTo = validTo
			decision.Validity.SupersededBy = newID
			decision.Status = domain.DecisionSuperseded
			return nil
		})
		return err
	}
	validity := record.Validity()
	validity.ValidTo = validTo
	validity.SupersededBy = newID
	return a.setRecordValidity(ctx, projectID, record, validity)
}

// markSupersedes records on the replacement which record it replaced.
func (a *App) markSupersedes(ctx context.Context, projectID string, record Record, oldID string) error {
	if record.Decision != nil {
		_, err := a.updateDecision(ctx, projectID, record.Decision.ID, func(decision *domain.Decision) error {
			decision.Validity.Supersedes = oldID
			return nil
		})
		return err
	}
	validity := record.Validity()
	validity.Supersedes = oldID
	return a.setRecordValidity(ctx, projectID, record, validity)
}

// supersedeTime is the end instant of a superseded record. It never equals
// valid_from: two writes within the same clock tick still yield an ordered
// interval.
func (a *App) supersedeTime(record Record) time.Time {
	now := a.now().UTC()
	if from := record.Validity().EffectiveFrom(record.Summary().CreatedAt); !now.After(from) {
		return from.Add(time.Nanosecond)
	}
	return now
}

// setRecordValidity persists the validity of a note, attempt or handoff and
// refreshes its index projection.
func (a *App) setRecordValidity(ctx context.Context, projectID string, record Record, validity domain.Validity) error {
	setter, ok := a.deps.Store.(ports.RecordValiditySetter)
	if !ok {
		return fmt.Errorf("%w: this store cannot change record validity", domain.ErrUnsupported)
	}
	summary := record.Summary()
	if err := a.enforceUpdateLimits(projectID, summary.Kind, summary.ID, recordWriteContent(record)); err != nil {
		return err
	}
	if err := setter.SetRecordValidity(ctx, projectID, summary.Kind, summary.ID, validity); err != nil {
		return err
	}
	record = record.SetValidity(validity)
	if err := a.upsertIndex(ctx, recordDocument(record)); err != nil {
		return fmt.Errorf("%s %s was updated but the search index update failed: %w", summary.Kind, summary.ID, err)
	}
	detail := "valid_to " + formatTime(validity.ValidTo)
	if validity.SupersededBy != "" {
		detail += " superseded_by " + validity.SupersededBy
	}
	if validity.Supersedes != "" {
		detail += " supersedes " + validity.Supersedes
	}
	if err := a.journalWrite(ctx, JournalStatus, projectID, summary.ID, detail); err != nil {
		return fmt.Errorf("%s %s was updated but the journal append failed: %w", summary.Kind, summary.ID, err)
	}
	return nil
}

// checkSupersedeCycle rejects a link that closes a supersede loop. Chains are
// walked in both directions, because either link makes the other a cycle.
func checkSupersedeCycle(index map[string]Record, oldID, newID string) error {
	cycle := domain.NewValidationError("by",
		fmt.Sprintf("superseding %s by %s would create a supersede cycle", oldID, newID))
	if strings.TrimSpace(index[oldID].Validity().Supersedes) == newID {
		return cycle
	}
	if reachesRecord(index, newID, oldID, func(record Record) string { return record.Validity().SupersededBy }) {
		return cycle
	}
	if reachesRecord(index, oldID, newID, func(record Record) string { return record.Validity().Supersedes }) {
		return cycle
	}
	return nil
}

// reachesRecord walks successor from start and reports whether target is
// reached. A walk longer than the record count means the stored links already
// loop, which is treated as a cycle.
func reachesRecord(index map[string]Record, start, target string, successor func(Record) string) bool {
	current := start
	for steps := 0; steps <= len(index); steps++ {
		record, ok := index[current]
		if !ok {
			return false
		}
		next := strings.TrimSpace(successor(record))
		if next == "" {
			return false
		}
		if next == target {
			return true
		}
		current = next
	}
	return true
}
