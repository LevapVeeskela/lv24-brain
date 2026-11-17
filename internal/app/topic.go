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

// resolveTaxonomy validates the optional classification of a write. An empty
// topic key means an append-only record; occurrences start at one and are only
// changed by a topic upsert.
func resolveTaxonomy(subtype string, scope domain.RecordScope, topicKey string) (domain.Taxonomy, error) {
	if !scope.Valid() {
		return domain.Taxonomy{}, domain.NewValidationError("scope",
			fmt.Sprintf("must be %q, %q or %q", domain.ScopeProject, domain.ScopePersonal, domain.ScopeGlobal))
	}
	topic, err := domain.NormalizeTopicKey(topicKey)
	if err != nil {
		return domain.Taxonomy{}, err
	}
	taxonomy := domain.Taxonomy{Subtype: subtype, Scope: scope, TopicKey: topic, Occurrences: 1}.Normalized()
	if err := taxonomy.Validate("record.taxonomy", ""); err != nil {
		return domain.Taxonomy{}, err
	}
	return taxonomy, nil
}

// lockTopic serializes a topic upsert. Topic upserts must refuse on stores
// that cannot provide a per-(project,kind,topic) lock, because the read/
// modify/write sequence would otherwise be a TOCTOU duplicate risk.
func (a *App) lockTopic(ctx context.Context, projectID, kind, topicKey string) (func(), error) {
	locker, ok := a.deps.Store.(ports.TopicLocker)
	if !ok {
		return nil, fmt.Errorf("%w: topic upsert is not supported by this store (missing TopicLocker)", domain.ErrUnsupported)
	}
	return locker.LockTopic(ctx, projectID, kind, topicKey)
}

// findTopicRecord returns the oldest record of kind that holds topicKey. The
// oldest one wins so a legacy duplicate does not make the result depend on
// listing order; the newest duplicates stay untouched and are reported by
// review like any other duplicate.
func (a *App) findTopicRecord(ctx context.Context, projectID, kind, topicKey string) (Record, bool, error) {
	if strings.TrimSpace(topicKey) == "" {
		return Record{}, false, nil
	}
	records, err := a.projectRecords(ctx, projectID)
	if err != nil {
		return Record{}, false, err
	}
	matches := make([]Record, 0, 2)
	for _, record := range records {
		if record.Kind != kind || recordTaxonomy(record).TopicKey != topicKey {
			continue
		}
		matches = append(matches, record)
	}
	if len(matches) == 0 {
		return Record{}, false, nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		left, right := matches[i].Summary(), matches[j].Summary()
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.ID < right.ID
	})
	return matches[0], true, nil
}

// recordTaxonomy returns the classification of whichever record is set.
func recordTaxonomy(record Record) domain.Taxonomy {
	switch {
	case record.Note != nil:
		return record.Note.Taxonomy.Normalized()
	case record.Decision != nil:
		return record.Decision.Taxonomy.Normalized()
	case record.Attempt != nil:
		return record.Attempt.Taxonomy.Normalized()
	case record.Handoff != nil:
		return record.Handoff.Taxonomy.Normalized()
	}
	return domain.Taxonomy{}.Normalized()
}

// recordHistory returns the stored history of whichever record is set.
func recordHistory(record Record) []domain.HistoryEntry {
	switch {
	case record.Note != nil:
		return record.Note.History
	case record.Decision != nil:
		return record.Decision.History
	case record.Attempt != nil:
		return record.Attempt.History
	case record.Handoff != nil:
		return record.Handoff.History
	}
	return nil
}

// historyWithBody prepends the record's current body to its history. The body
// is the same text a reader sees, so the History section never references a
// rendering the engine cannot reproduce.
func historyWithBody(record Record, now time.Time) []domain.HistoryEntry {
	at := record.Summary().UpdatedAt
	if at.IsZero() {
		at = now
	}
	entry := domain.HistoryEntry{At: at.UTC().Format(time.RFC3339), Body: RecordText(record)}
	return domain.PrependedHistory(recordHistory(record), entry)
}

// mergeTopicTaxonomy applies an explicit classification to the stored one.
// Fields the caller left empty keep their stored value; the occurrence count
// is the stored count plus this write.
func mergeTopicTaxonomy(stored, requested domain.Taxonomy, scopeExplicit bool, occurrences int) domain.Taxonomy {
	out := stored.Normalized()
	if requested.Subtype != "" {
		out.Subtype = requested.Subtype
	}
	if scopeExplicit {
		out.Scope = requested.Scope.Normalized()
	}
	if requested.TopicKey != "" {
		out.TopicKey = requested.TopicKey
	}
	out.Occurrences = occurrences
	return out.Normalized()
}

// topicBodyEqual compares the current text of two writes, ignoring only
// surrounding whitespace.
func topicBodyEqual(left, right string) bool {
	return strings.TrimSpace(left) == strings.TrimSpace(right)
}

// validateClassificationFilters normalizes the optional search filters. A
// scope outside the documented set is a usage error, not an empty result.
func validateClassificationFilters(subtype, scope, topicKey string) (string, string, string, error) {
	scopeValue := domain.RecordScope(strings.TrimSpace(scope))
	if !scopeValue.Valid() {
		return "", "", "", domain.NewValidationError("scope",
			fmt.Sprintf("must be %q, %q or %q", domain.ScopeProject, domain.ScopePersonal, domain.ScopeGlobal))
	}
	topic, err := domain.NormalizeTopicKey(topicKey)
	if err != nil {
		return "", "", "", err
	}
	scopeFilter := ""
	if strings.TrimSpace(scope) != "" {
		scopeFilter = string(scopeValue.Normalized())
	}
	return strings.TrimSpace(subtype), scopeFilter, topic, nil
}
