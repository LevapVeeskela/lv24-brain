package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Journal events of the supersede review lifecycle. One case belongs to one
// predecessor; the newest event for that id is its current state.
const (
	// JournalSupersedeProposed opens a case: the successor claims the
	// predecessor, the predecessor stays valid until the case is accepted.
	JournalSupersedeProposed = "supersede"
	// JournalSupersedeAccepted records the approved validity tuple.
	JournalSupersedeAccepted = "supersede-accept"
	// JournalSupersedeRejected records a refused case and its reason.
	JournalSupersedeRejected = "supersede-reject"
	// JournalSupersedeRolledBack records a restore of the previous tuple.
	JournalSupersedeRolledBack = "supersede-rollback"
	// JournalInvalidate records an explicit invalidation with its reason.
	JournalInvalidate = "invalidate"
)

// maxLifecycleJournalEntries bounds the journal read behind the review queue.
// Cases older than this many lifecycle lines are not proposed for review any
// more; their files stay untouched.
const maxLifecycleJournalEntries = 1000

// maxSupersedeTextRunes bounds a case reason or decision note. The journal
// stores one line per event, so a long value would be clipped or dropped.
const maxSupersedeTextRunes = 240

// SupersedeCaseStatus is the review state of one supersede proposal.
type SupersedeCaseStatus string

const (
	// SupersedeProposed waits for a review decision. The predecessor is
	// still valid and searchable.
	SupersedeProposed SupersedeCaseStatus = "proposed"
	// SupersedeAccepted means the predecessor was ended by this successor.
	SupersedeAccepted SupersedeCaseStatus = "accepted"
	// SupersedeRejected means the case was refused and the predecessor was
	// left byte-for-byte untouched.
	SupersedeRejected SupersedeCaseStatus = "rejected"
	// SupersedeRolledBack means an accepted case was undone: the previous
	// validity tuple is back in place.
	SupersedeRolledBack SupersedeCaseStatus = "rolled_back"
)

// SupersedeValidity is the stored validity tuple of one record. It is the
// exact frontmatter state, so a rollback restores the same field values,
// including fields that were left empty.
type SupersedeValidity struct {
	ValidFrom    time.Time
	ValidTo      time.Time
	SupersededBy string
	// Status is set for decisions only: a superseded decision also carries
	// the superseded status, which a rollback restores.
	Status string
}

// SupersedeCase is one review case: the successor named by NewID claims the
// predecessor OldID. It is the staged form of a supersede: proposing it writes
// nothing to the predecessor, accepting it ends the predecessor, rejecting it
// leaves the predecessor alone, and rolling an accepted case back restores the
// Previous tuple exactly.
type SupersedeCase struct {
	ProjectID string
	// Kind is the predecessor's record kind.
	Kind  string
	OldID string
	NewID string
	// Reason is why the case was written, given at proposal time.
	Reason string
	// Status is the current state of the case.
	Status SupersedeCaseStatus
	// Revision is the position of this state in the case timeline. The journal
	// stores one line per transition, and two lines can share a timestamp, so
	// the revision (not the clock) decides which state is current.
	Revision int
	// ProposedAt is when the case was opened; DecidedAt when it was accepted,
	// rejected or rolled back.
	ProposedAt time.Time
	DecidedAt  time.Time
	// DecisionNote explains an acceptance, rejection or rollback.
	DecisionNote string
	// Previous is the predecessor's validity tuple before acceptance; Applied
	// is the tuple the acceptance wrote. Both are empty until acceptance.
	Previous SupersedeValidity
	Applied  SupersedeValidity
	// ExpectedHash is SupersedeStateHash of the predecessor as the approval
	// left it. A rollback refuses to restore over a later change.
	ExpectedHash string
}

// SupersedeResult reports both ends of a supersede link.
type SupersedeResult struct {
	Old RecordSummary
	New RecordSummary
	// Changed is false for an idempotent repeat of the same link.
	Changed bool
	// Case is the staged review case. It is nil when the store has no
	// journal: without a review queue the link applies immediately.
	Case *SupersedeCase
}

// Supersede stages a successor claim: the successor records that it replaces
// oldID, and a proposed review case names both ends and the default reason. The
// predecessor stays valid and searchable until the case is accepted, so a
// belief is never retired without a review decision.
//
// Stores without a journal (server mode) have no review queue; there the link
// applies immediately, exactly as before 0.27.
func (a *App) Supersede(ctx context.Context, projectID, oldID, newID string) (SupersedeResult, error) {
	return a.SupersedeWithReason(ctx, projectID, oldID, newID, "")
}

// SupersedeWithReason is Supersede with a written reason for the case. An
// empty reason becomes "superseded by <new id>".
func (a *App) SupersedeWithReason(ctx context.Context, projectID, oldID, newID, reason string) (SupersedeResult, error) {
	if err := a.requireStore(); err != nil {
		return SupersedeResult{}, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return SupersedeResult{}, err
	}
	old, replacement, err := a.supersedePair(ctx, project.ID, oldID, newID)
	if err != nil {
		return SupersedeResult{}, err
	}
	oldID, newID = old.Summary().ID, replacement.Summary().ID
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "superseded by " + newID
	}
	if err := validateSupersedeText("reason", reason); err != nil {
		return SupersedeResult{}, err
	}
	// An applied link is idempotent: the predecessor already ends at newID.
	if current := strings.TrimSpace(old.Validity().SupersededBy); current == newID && !old.Validity().ValidTo.IsZero() {
		return a.supersedeResult(ctx, project.ID, oldID, newID, false, nil)
	}
	if a.deps.Journal == nil {
		changed, err := a.applySupersedeLink(ctx, project.ID, old, replacement)
		if err != nil {
			return SupersedeResult{}, err
		}
		return a.supersedeResult(ctx, project.ID, oldID, newID, changed, nil)
	}
	cases, err := a.supersedeCaseMap(ctx, project.ID)
	if err != nil {
		return SupersedeResult{}, err
	}
	if current, ok := cases[oldID]; ok && current.Status == SupersedeProposed {
		if current.NewID != newID {
			return SupersedeResult{}, domain.NewValidationError("by",
				fmt.Sprintf("%s already has a proposed supersede case for %s; review that case first", oldID, current.NewID))
		}
		return a.supersedeResult(ctx, project.ID, oldID, newID, false, &current)
	}
	// A record that already ended without this successor cannot gain one.
	if end := old.Validity().ValidTo; !end.IsZero() {
		return SupersedeResult{}, domain.NewValidationError("id",
			fmt.Sprintf("%s ended at %s; it cannot be superseded", oldID, formatTime(end)))
	}
	changed := false
	if strings.TrimSpace(replacement.Validity().Supersedes) != oldID {
		if err := a.markSupersedes(ctx, project.ID, replacement, oldID); err != nil {
			return SupersedeResult{}, err
		}
		changed = true
	}
	staged := SupersedeCase{
		ProjectID: project.ID,
		Kind:      old.Summary().Kind,
		OldID:     oldID,
		NewID:     newID,
		Reason:    reason,
		Status:    SupersedeProposed,
		Revision:  1,
	}
	if previous, ok := cases[oldID]; ok {
		// A rejected or rolled-back case can be proposed again; the revision
		// keeps the new proposal newer than the decision it follows.
		staged.Revision = previous.Revision + 1
	}
	if err := a.recordSupersedeCase(ctx, JournalSupersedeProposed, staged); err != nil {
		return SupersedeResult{}, fmt.Errorf("the successor link was written but the review case was not recorded: %w", err)
	}
	changed = true
	return a.supersedeResult(ctx, project.ID, oldID, newID, changed, &staged)
}

// SupersedeReviewAction is what a review does with one supersede case.
type SupersedeReviewAction string

const (
	// SupersedeAccept ends the predecessor at the approved tuple.
	SupersedeAccept SupersedeReviewAction = "accept"
	// SupersedeReject refuses the case and leaves the predecessor alone.
	SupersedeReject SupersedeReviewAction = "reject"
	// SupersedeRollback restores the predecessor's previous tuple.
	SupersedeRollback SupersedeReviewAction = "rollback"
)

// SupersedeReviewInput selects one case and what to do with it. OldID names the
// case; NewID is optional and must match the case when set. Reason is the
// written decision: required for a rejection, explanatory for the others.
// ExpectedHash is optional and either the caller's own SupersedeStateHash of
// the predecessor or empty to use the one recorded at approval.
type SupersedeReviewInput struct {
	ProjectID    string
	OldID        string
	NewID        string
	Action       SupersedeReviewAction
	Reason       string
	ExpectedHash string
}

// ReviewSupersede accepts, rejects or rolls back one supersede case.
//
// Accepting writes the predecessor's valid_to and superseded_by and marks the
// case accepted in the same call. Rejecting writes nothing to the predecessor
// and records the refusal reason. Rolling an accepted case back restores the
// exact previous validity tuple and refuses with domain.ErrConflict when the
// predecessor changed after the approval.
func (a *App) ReviewSupersede(ctx context.Context, in SupersedeReviewInput) (SupersedeCase, error) {
	if err := a.requireStore(); err != nil {
		return SupersedeCase{}, err
	}
	switch in.Action {
	case SupersedeAccept, SupersedeReject, SupersedeRollback:
	default:
		return SupersedeCase{}, domain.NewValidationError("action",
			fmt.Sprintf("unknown action %q (accept, reject, rollback)", in.Action))
	}
	if a.deps.Journal == nil {
		return SupersedeCase{}, fmt.Errorf("%w: a supersede case needs the activity journal; this store has no review queue", domain.ErrUnsupported)
	}
	project, err := a.ensureProject(ctx, in.ProjectID, false)
	if err != nil {
		return SupersedeCase{}, err
	}
	oldID := strings.TrimSpace(in.OldID)
	if oldID == "" {
		return SupersedeCase{}, domain.NewValidationError("id", "the predecessor id is required")
	}
	cases, err := a.supersedeCaseMap(ctx, project.ID)
	if err != nil {
		return SupersedeCase{}, err
	}
	current, ok := cases[oldID]
	if !ok {
		return SupersedeCase{}, fmt.Errorf("supersede case for %q in project %s: %w", oldID, project.ID, domain.ErrNotFound)
	}
	if newID := strings.TrimSpace(in.NewID); newID != "" && newID != current.NewID {
		return SupersedeCase{}, domain.NewValidationError("new",
			fmt.Sprintf("the case for %s names %s, not %s", oldID, current.NewID, newID))
	}
	return a.reviewSupersedeCase(ctx, project.ID, current, in.Action, strings.TrimSpace(in.Reason), strings.TrimSpace(in.ExpectedHash))
}

// SupersedeCases lists the lifecycle cases of a project, newest proposal
// first. Without includeDecided only cases that still wait for a decision are
// returned.
func (a *App) SupersedeCases(ctx context.Context, projectID string, includeDecided bool) ([]SupersedeCase, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return nil, err
	}
	cases, err := a.supersedeCaseMap(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	out := make([]SupersedeCase, 0, len(cases))
	for _, c := range cases {
		if !includeDecided && c.Status != SupersedeProposed {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].ProposedAt.Equal(out[j].ProposedAt) {
			return out[i].ProposedAt.After(out[j].ProposedAt)
		}
		return out[i].OldID < out[j].OldID
	})
	return out, nil
}

// InvalidateResult is the outcome of an explicit invalidation.
type InvalidateResult struct {
	Record        RecordSummary
	Reason        string
	InvalidatedAt time.Time
	// Changed is false when the record had already ended.
	Changed bool
}

// InvalidateRecord ends a record explicitly and records why. It is the archive
// path without a successor: the file keeps its text, only valid_to is written,
// and default search/context hide the record while --archive (include
// superseded) still returns it. Nothing is moved or deleted.
func (a *App) InvalidateRecord(ctx context.Context, projectID, id, reason string) (InvalidateResult, error) {
	if err := a.requireStore(); err != nil {
		return InvalidateResult{}, err
	}
	reason = strings.TrimSpace(reason)
	if err := validateSupersedeText("reason", reason); err != nil {
		return InvalidateResult{}, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return InvalidateResult{}, err
	}
	record, err := a.GetRecord(ctx, project.ID, id)
	if err != nil {
		return InvalidateResult{}, err
	}
	summary := record.Summary()
	if end := record.Validity().ValidTo; !end.IsZero() {
		return InvalidateResult{Record: a.Summarize(record), Reason: reason, InvalidatedAt: end}, nil
	}
	if err := a.enforceUpdateLimits(project.ID, summary.Kind, summary.ID, recordWriteContent(record)); err != nil {
		return InvalidateResult{}, err
	}
	invalidatedAt := a.supersedeTime(record)
	validity := record.Validity()
	validity.ValidTo = invalidatedAt
	if err := a.applyValidityTuple(ctx, project.ID, record, validity); err != nil {
		return InvalidateResult{}, err
	}
	if a.deps.Journal != nil {
		entry := ports.JournalEntry{
			Time: a.now(), Event: JournalInvalidate, User: a.JournalUser(),
			Project: project.ID, ID: summary.ID,
			Title: summary.Kind + ": " + SummarizeLine(reason, 70),
			Fields: map[string]string{
				"kind": summary.Kind, "status": "invalidated", "rev": "1",
				"reason": reason, "invalidated_at": caseTime(invalidatedAt),
			},
		}
		if err := a.deps.Journal.Append(ctx, entry); err != nil {
			return InvalidateResult{}, fmt.Errorf("%s %s was invalidated but the journal append failed: %w", summary.Kind, summary.ID, err)
		}
	}
	updated, err := a.GetRecord(ctx, project.ID, summary.ID)
	if err != nil {
		return InvalidateResult{}, err
	}
	return InvalidateResult{Record: a.Summarize(updated), Reason: reason, InvalidatedAt: invalidatedAt, Changed: true}, nil
}

// ArchiveEntry is one invalidated record as the archive reports it.
type ArchiveEntry struct {
	Kind          string
	ID            string
	InvalidatedAt time.Time
	Reason        string
	// SupersededBy names the successor of an approved supersede; it is empty
	// for an explicit invalidation.
	SupersededBy string
}

// Archive lists the records whose validity ended and are still on disk. It is
// the read side of LIFE-2: the reason and instant come from the lifecycle
// journal, and an entry disappears when the approval was rolled back. A record
// that is no longer ended (or was trashed) is not reported.
func (a *App) Archive(ctx context.Context, projectID string) ([]ArchiveEntry, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return nil, err
	}
	entries, err := a.lifecycleJournalEntries(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	// archiveState is the winning event of one record: the highest revision,
	// then the latest timestamp. Revision decides because two events of one
	// record can share a clock tick.
	type archiveState struct {
		revision int
		at       time.Time
		entry    ArchiveEntry
	}
	latest := make(map[string]archiveState, len(entries))
	for _, entry := range entries {
		state := archiveState{revision: caseRevision(entry.Fields["rev"]), at: entry.Time}
		switch entry.Event {
		case JournalSupersedeAccepted, JournalInvalidate:
			state.entry = ArchiveEntry{
				Kind:          entry.Fields["kind"],
				ID:            entry.ID,
				InvalidatedAt: parseCaseTime(entry.Fields["invalidated_at"]),
				Reason:        entry.Fields["reason"],
				SupersededBy:  entry.Fields["new"],
			}
		default:
			// A rejection never ended anything; a rollback restored it.
			state.entry = ArchiveEntry{}
		}
		if current, ok := latest[entry.ID]; ok {
			if state.revision < current.revision || (state.revision == current.revision && !state.at.After(current.at)) {
				continue
			}
		}
		latest[entry.ID] = state
	}
	records, err := a.projectRecords(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	live := make(map[string]Record, len(records))
	for _, record := range records {
		live[record.Summary().ID] = record
	}
	out := make([]ArchiveEntry, 0, len(latest))
	for id, state := range latest {
		entry := state.entry
		if entry.ID == "" {
			continue
		}
		record, ok := live[id]
		if !ok || !invalidated(record.Validity()) {
			continue
		}
		if entry.Kind == "" {
			entry.Kind = record.Summary().Kind
		}
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].InvalidatedAt.Equal(out[j].InvalidatedAt) {
			return out[i].InvalidatedAt.After(out[j].InvalidatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// SupersedeStateHash is the stable fingerprint of a record's lifecycle state:
// identity, text, status and the validity tuple. A rollback compares it with
// the fingerprint recorded at approval, so a record edited after the approval
// is never restored over.
func SupersedeStateHash(record Record) string {
	summary := record.Summary()
	validity := record.Validity()
	var out strings.Builder
	write := func(values ...string) {
		for _, value := range values {
			out.WriteString(value)
			out.WriteByte(0)
		}
	}
	write("supersede-v1", summary.Kind, summary.ID, summary.Status,
		validity.ValidFrom.UTC().Format(time.RFC3339Nano),
		validity.ValidTo.UTC().Format(time.RFC3339Nano),
		validity.SupersededBy, validity.Supersedes,
		summary.UpdatedAt.UTC().Format(time.RFC3339Nano))
	switch {
	case record.Note != nil:
		write(record.Note.Title, record.Note.Body, strings.Join(record.Note.Tags, "\x1f"))
	case record.Decision != nil:
		write(record.Decision.Title, record.Decision.Context,
			strings.Join(record.Decision.Alternatives, "\x1f"),
			strings.Join(record.Decision.Consequences, "\x1f"))
	case record.Attempt != nil:
		write(record.Attempt.Title, record.Attempt.Hypothesis, record.Attempt.Result, record.Attempt.Reason)
	case record.Handoff != nil:
		write(record.Handoff.Summary, record.Handoff.NextAction)
	}
	sum := sha256.Sum256([]byte(out.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
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

// supersedePair validates a supersede request and returns both records without
// writing anything.
func (a *App) supersedePair(ctx context.Context, projectID, oldID, newID string) (Record, Record, error) {
	oldID, newID = strings.TrimSpace(oldID), strings.TrimSpace(newID)
	if oldID == "" || newID == "" {
		return Record{}, Record{}, domain.NewValidationError("supersede", "both the old and the new record id are required")
	}
	if oldID == newID {
		return Record{}, Record{}, domain.NewValidationError("by", "a record cannot supersede itself")
	}
	if err := domain.ValidateRecordReference("id", oldID); err != nil {
		return Record{}, Record{}, err
	}
	if err := domain.ValidateRecordReference("by", newID); err != nil {
		return Record{}, Record{}, err
	}
	records, err := a.projectRecords(ctx, projectID)
	if err != nil {
		return Record{}, Record{}, err
	}
	index := make(map[string]Record, len(records))
	for _, record := range records {
		index[record.Summary().ID] = record
	}
	old, ok := index[oldID]
	if !ok {
		return Record{}, Record{}, fmt.Errorf("old record %q in project %s: %w", oldID, projectID, domain.ErrNotFound)
	}
	replacement, ok := index[newID]
	if !ok {
		return Record{}, Record{}, fmt.Errorf("new record %q in project %s: %w", newID, projectID, domain.ErrNotFound)
	}
	if current := strings.TrimSpace(old.Validity().SupersededBy); current != "" && current != newID {
		return Record{}, Record{}, domain.NewValidationError("id",
			fmt.Sprintf("%s is already superseded by %s; supersede that record instead", oldID, current))
	}
	if current := strings.TrimSpace(replacement.Validity().Supersedes); current != "" && current != oldID {
		return Record{}, Record{}, domain.NewValidationError("by",
			fmt.Sprintf("%s already supersedes %s", newID, current))
	}
	if err := checkSupersedeCycle(index, oldID, newID); err != nil {
		return Record{}, Record{}, err
	}
	// Check both ends before the first write: a cap or protection refusal on
	// the replacement must not leave the old record already ended.
	for _, record := range []Record{old, replacement} {
		summary := record.Summary()
		if err := a.enforceUpdateLimits(projectID, summary.Kind, summary.ID, recordWriteContent(record)); err != nil {
			return Record{}, Record{}, err
		}
	}
	return old, replacement, nil
}

// applySupersedeLink writes both ends of a supersede immediately. The old
// record is ended first: if the second write fails, the chain still says the
// old record is no longer current and a retry finishes it.
func (a *App) applySupersedeLink(ctx context.Context, projectID string, old, replacement Record) (bool, error) {
	oldID, newID := old.Summary().ID, replacement.Summary().ID
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

// linkSupersede marks oldID replaced by newID and reports whether anything
// changed. It is the immediate form used by a write that passes --supersedes:
// the author declares the link in the same write, so there is nothing to
// review.
func (a *App) linkSupersede(ctx context.Context, projectID, oldID, newID string) (bool, error) {
	old, replacement, err := a.supersedePair(ctx, projectID, oldID, newID)
	if err != nil {
		return false, err
	}
	return a.applySupersedeLink(ctx, projectID, old, replacement)
}

// markSuperseded ends a record and points it at its replacement.
func (a *App) markSuperseded(ctx context.Context, projectID string, record Record, newID string) error {
	return a.markSupersededAt(ctx, projectID, record, newID, a.supersedeTime(record))
}

// markSupersededAt ends a record at an explicit instant. The caller that also
// records the case uses the same instant, so the journal and the frontmatter
// agree.
func (a *App) markSupersededAt(ctx context.Context, projectID string, record Record, newID string, validTo time.Time) error {
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

// applyValidityTuple writes a complete validity tuple to a record of any kind.
func (a *App) applyValidityTuple(ctx context.Context, projectID string, record Record, validity domain.Validity) error {
	if record.Decision != nil {
		_, err := a.updateDecision(ctx, projectID, record.Decision.ID, func(decision *domain.Decision) error {
			decision.Validity = validity
			return nil
		})
		return err
	}
	return a.setRecordValidity(ctx, projectID, record, validity)
}

// reviewSupersedeCase is the shared core of the review action.
func (a *App) reviewSupersedeCase(ctx context.Context, projectID string, c SupersedeCase, action SupersedeReviewAction, note, expectedHash string) (SupersedeCase, error) {
	switch action {
	case SupersedeAccept, SupersedeReject:
		if c.Status != SupersedeProposed {
			return c, domain.NewValidationError("case",
				fmt.Sprintf("the case for %s is %s, not proposed", c.OldID, c.Status))
		}
	case SupersedeRollback:
		if c.Status != SupersedeAccepted {
			return c, domain.NewValidationError("case",
				fmt.Sprintf("the case for %s is %s, not accepted", c.OldID, c.Status))
		}
	}
	switch action {
	case SupersedeAccept:
		if note == "" {
			note = "approved"
		}
		return a.acceptSupersedeCase(ctx, projectID, c, note)
	case SupersedeReject:
		if note == "" {
			return c, domain.NewValidationError("reason", "a rejection needs a written reason")
		}
		return a.decideSupersedeCase(ctx, c, SupersedeRejected, JournalSupersedeRejected, note)
	default:
		if note == "" {
			note = "rolled back"
		}
		return a.rollbackSupersedeCase(ctx, projectID, c, note, expectedHash)
	}
}

// acceptSupersedeCase ends the predecessor at the approved tuple and records
// the case as accepted.
func (a *App) acceptSupersedeCase(ctx context.Context, projectID string, c SupersedeCase, note string) (SupersedeCase, error) {
	if err := validateSupersedeText("reason", note); err != nil {
		return c, err
	}
	old, err := a.GetRecord(ctx, projectID, c.OldID)
	if err != nil {
		return c, err
	}
	successor, err := a.GetRecord(ctx, projectID, c.NewID)
	if err != nil {
		return c, err
	}
	if successor.Decision != nil && successor.Decision.Status == domain.DecisionRejected {
		return c, domain.NewValidationError("new",
			fmt.Sprintf("%s was rejected; a rejected record cannot supersede %s", c.NewID, c.OldID))
	}
	if current := strings.TrimSpace(old.Validity().SupersededBy); current != "" && current != c.NewID {
		return c, domain.NewValidationError("id",
			fmt.Sprintf("%s is already superseded by %s", c.OldID, current))
	}
	if end := old.Validity().ValidTo; !end.IsZero() && strings.TrimSpace(old.Validity().SupersededBy) != c.NewID {
		return c, domain.NewValidationError("id",
			fmt.Sprintf("%s ended at %s; it cannot be superseded", c.OldID, formatTime(end)))
	}
	if err := a.enforceUpdateLimits(projectID, old.Summary().Kind, c.OldID, recordWriteContent(old)); err != nil {
		return c, err
	}
	previous := supersedeValidityOf(old)
	appliedAt := a.supersedeTime(old)
	if err := a.markSupersededAt(ctx, projectID, old, c.NewID, appliedAt); err != nil {
		return c, err
	}
	if strings.TrimSpace(successor.Validity().Supersedes) != c.OldID {
		if err := a.markSupersedes(ctx, projectID, successor, c.OldID); err != nil {
			return c, err
		}
	}
	applied, err := a.GetRecord(ctx, projectID, c.OldID)
	if err != nil {
		return c, err
	}
	c.Status = SupersedeAccepted
	c.Revision++
	c.DecidedAt = a.now().UTC()
	c.DecisionNote = note
	c.Previous = previous
	c.Applied = supersedeValidityOf(applied)
	c.ExpectedHash = SupersedeStateHash(applied)
	if err := a.recordSupersedeCase(ctx, JournalSupersedeAccepted, c); err != nil {
		return c, fmt.Errorf("%s %s was superseded but the review case was not recorded: %w",
			old.Summary().Kind, c.OldID, err)
	}
	return c, nil
}

// decideSupersedeCase records a terminal, non-applied decision (a rejection).
// It never writes the predecessor.
func (a *App) decideSupersedeCase(ctx context.Context, c SupersedeCase, status SupersedeCaseStatus, event, note string) (SupersedeCase, error) {
	if err := validateSupersedeText("reason", note); err != nil {
		return c, err
	}
	if _, err := a.GetRecord(ctx, c.ProjectID, c.OldID); err != nil {
		return c, err
	}
	c.Status = status
	c.Revision++
	c.DecidedAt = a.now().UTC()
	c.DecisionNote = note
	c.Previous = SupersedeValidity{}
	c.Applied = SupersedeValidity{}
	c.ExpectedHash = ""
	if err := a.recordSupersedeCase(ctx, event, c); err != nil {
		return c, err
	}
	return c, nil
}

// rollbackSupersedeCase restores the exact previous tuple and refuses when the
// predecessor changed after the approval.
func (a *App) rollbackSupersedeCase(ctx context.Context, projectID string, c SupersedeCase, note, expectedHash string) (SupersedeCase, error) {
	if err := validateSupersedeText("reason", note); err != nil {
		return c, err
	}
	old, err := a.GetRecord(ctx, projectID, c.OldID)
	if err != nil {
		return c, err
	}
	expected := expectedHash
	if expected == "" {
		expected = c.ExpectedHash
	}
	if expected != "" && SupersedeStateHash(old) != expected {
		return c, domain.NewRevisionConflictError(old.Summary().Kind, c.OldID, old.Summary().UpdatedAt)
	}
	if current := strings.TrimSpace(old.Validity().SupersededBy); current != c.NewID {
		return c, domain.NewValidationError("id",
			fmt.Sprintf("%s is no longer superseded by %s (link %q); nothing to roll back", c.OldID, c.NewID, current))
	}
	if err := a.enforceUpdateLimits(projectID, old.Summary().Kind, c.OldID, recordWriteContent(old)); err != nil {
		return c, err
	}
	restored := old.Validity()
	restored.ValidFrom = c.Previous.ValidFrom
	restored.ValidTo = c.Previous.ValidTo
	restored.SupersededBy = c.Previous.SupersededBy
	if old.Decision != nil {
		status := domain.DecisionStatus(c.Previous.Status)
		_, err := a.updateDecision(ctx, projectID, old.Decision.ID, func(decision *domain.Decision) error {
			decision.Validity = restored
			if status != "" {
				decision.Status = status
			}
			return nil
		})
		if err != nil {
			return c, err
		}
	} else if err := a.setRecordValidity(ctx, projectID, old, restored); err != nil {
		return c, err
	}
	// The successor stops claiming a record the review released.
	successor, err := a.GetRecord(ctx, projectID, c.NewID)
	if err != nil {
		return c, err
	}
	if strings.TrimSpace(successor.Validity().Supersedes) == c.OldID {
		validity := successor.Validity()
		validity.Supersedes = ""
		if err := a.applyValidityTuple(ctx, projectID, successor, validity); err != nil {
			return c, err
		}
	}
	c.Status = SupersedeRolledBack
	c.Revision++
	c.DecidedAt = a.now().UTC()
	c.DecisionNote = note
	if err := a.recordSupersedeCase(ctx, JournalSupersedeRolledBack, c); err != nil {
		return c, fmt.Errorf("%s %s was restored but the review case was not recorded: %w",
			old.Summary().Kind, c.OldID, err)
	}
	return c, nil
}

// settleSupersedeCases applies the review that a decision status change
// implies: accepting or rejecting a successor also accepts or rejects the case
// that names it. It is the existing `record status` review action.
func (a *App) settleSupersedeCases(ctx context.Context, projectID, successorID string, status domain.DecisionStatus) error {
	if a.deps.Journal == nil {
		return nil
	}
	var action SupersedeReviewAction
	note := ""
	switch status {
	case domain.DecisionAccepted:
		action, note = SupersedeAccept, "successor decision accepted"
	case domain.DecisionRejected:
		action, note = SupersedeReject, "successor decision rejected"
	default:
		return nil
	}
	cases, err := a.SupersedeCases(ctx, projectID, false)
	if err != nil {
		return err
	}
	for _, c := range cases {
		if c.NewID != successorID {
			continue
		}
		if _, err := a.reviewSupersedeCase(ctx, projectID, c, action, note, ""); err != nil {
			return fmt.Errorf("decision %s is %s but its supersede case for %s failed: %w",
				successorID, status, c.OldID, err)
		}
	}
	return nil
}

// supersedeResult re-reads both ends so the caller sees the stored state.
func (a *App) supersedeResult(ctx context.Context, projectID, oldID, newID string, changed bool, c *SupersedeCase) (SupersedeResult, error) {
	old, err := a.GetRecord(ctx, projectID, oldID)
	if err != nil {
		return SupersedeResult{}, err
	}
	replacement, err := a.GetRecord(ctx, projectID, newID)
	if err != nil {
		return SupersedeResult{}, err
	}
	return SupersedeResult{Old: old.Summary(), New: replacement.Summary(), Changed: changed, Case: c}, nil
}

// lifecycleJournalEntries returns the lifecycle events of a project, newest
// first. A store without a journal has no cases.
func (a *App) lifecycleJournalEntries(ctx context.Context, projectID string) ([]ports.JournalEntry, error) {
	if a.deps.Journal == nil {
		return nil, nil
	}
	entries, err := a.deps.Journal.Read(ctx, ports.JournalQuery{Project: projectID, Limit: maxLifecycleJournalEntries})
	if err != nil {
		return nil, fmt.Errorf("read the activity journal: %w", err)
	}
	out := make([]ports.JournalEntry, 0, len(entries))
	for _, entry := range entries {
		switch entry.Event {
		case JournalSupersedeProposed, JournalSupersedeAccepted, JournalSupersedeRejected, JournalSupersedeRolledBack, JournalInvalidate:
			out = append(out, entry)
		}
	}
	return out, nil
}

// supersedeCaseMap returns the current case of every predecessor, keyed by the
// predecessor id.
func (a *App) supersedeCaseMap(ctx context.Context, projectID string) (map[string]SupersedeCase, error) {
	entries, err := a.lifecycleJournalEntries(ctx, projectID)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]SupersedeCase, len(entries))
	proposed := make(map[string]SupersedeCase, len(entries))
	for _, entry := range entries {
		if entry.Event == JournalInvalidate {
			continue
		}
		c := supersedeCaseFromEntry(entry)
		if current, ok := latest[entry.ID]; !ok || caseNewer(c, current) {
			latest[entry.ID] = c
		}
		if entry.Event == JournalSupersedeProposed {
			if current, ok := proposed[entry.ID]; !ok || c.Revision > current.Revision {
				proposed[entry.ID] = c
			}
		}
	}
	for id, p := range proposed {
		if c, ok := latest[id]; ok {
			c.ProposedAt = p.ProposedAt
			latest[id] = c
		}
	}
	return latest, nil
}

// caseNewer reports whether a is the later state of the same case. The revision
// is the primary key because a journal line pair can share a timestamp; the
// event time only breaks a tie between lines that carry no revision.
func caseNewer(a, b SupersedeCase) bool {
	if a.Revision != b.Revision {
		return a.Revision > b.Revision
	}
	return a.ProposedAt.After(b.ProposedAt) || a.DecidedAt.After(b.DecidedAt)
}

// supersedeCaseFromEntry decodes one lifecycle journal line.
func supersedeCaseFromEntry(entry ports.JournalEntry) SupersedeCase {
	fields := entry.Fields
	c := SupersedeCase{
		ProjectID:    entry.Project,
		Kind:         fields["kind"],
		OldID:        entry.ID,
		NewID:        fields["new"],
		Reason:       fields["reason"],
		Status:       SupersedeCaseStatus(fields["status"]),
		Revision:     caseRevision(fields["rev"]),
		DecisionNote: fields["note"],
		ExpectedHash: fields["hash"],
		Previous: SupersedeValidity{
			ValidFrom:    parseCaseTime(fields["prev_from"]),
			ValidTo:      parseCaseTime(fields["prev_to"]),
			SupersededBy: fields["prev_by"],
			Status:       fields["prev_status"],
		},
		Applied: SupersedeValidity{
			ValidFrom:    parseCaseTime(fields["app_from"]),
			ValidTo:      parseCaseTime(fields["app_to"]),
			SupersededBy: fields["app_by"],
			Status:       fields["app_status"],
		},
	}
	if entry.Event == JournalSupersedeProposed {
		c.ProposedAt = entry.Time
	} else {
		c.DecidedAt = entry.Time
	}
	return c
}

// recordSupersedeCase appends one lifecycle event. The journal is the case
// store: git keeps the records, the journal keeps the review decisions and the
// exact tuple an approval wrote.
func (a *App) recordSupersedeCase(ctx context.Context, event string, c SupersedeCase) error {
	if a.deps.Journal == nil {
		return nil
	}
	fields := map[string]string{
		"kind":           c.Kind,
		"new":            c.NewID,
		"reason":         c.Reason,
		"status":         string(c.Status),
		"rev":            strconv.Itoa(c.Revision),
		"note":           c.DecisionNote,
		"hash":           c.ExpectedHash,
		"invalidated_at": caseTime(c.Applied.ValidTo),
		"prev_from":      caseTime(c.Previous.ValidFrom),
		"prev_to":        caseTime(c.Previous.ValidTo),
		"prev_by":        c.Previous.SupersededBy,
		"prev_status":    c.Previous.Status,
		"app_from":       caseTime(c.Applied.ValidFrom),
		"app_to":         caseTime(c.Applied.ValidTo),
		"app_by":         c.Applied.SupersededBy,
		"app_status":     c.Applied.Status,
	}
	return a.deps.Journal.Append(ctx, ports.JournalEntry{
		Time: a.now(), Event: event, User: a.JournalUser(),
		Project: c.ProjectID, ID: c.OldID,
		Title:  "supersede " + c.OldID + " -> " + c.NewID,
		Fields: fields,
	})
}

// supersedeValidityOf captures the stored tuple of one record.
func supersedeValidityOf(record Record) SupersedeValidity {
	validity := record.Validity()
	out := SupersedeValidity{
		ValidFrom:    validity.ValidFrom,
		ValidTo:      validity.ValidTo,
		SupersededBy: validity.SupersededBy,
	}
	if record.Decision != nil {
		out.Status = string(record.Decision.Status)
	}
	return out
}

// validateSupersedeText bounds a one-line case reason or note.
func validateSupersedeText(field, value string) error {
	if value == "" {
		return domain.NewValidationError(field, "is required")
	}
	if strings.ContainsAny(value, "\r\n") {
		return domain.NewValidationError(field, "must be a single line")
	}
	if runes := []rune(value); len(runes) > maxSupersedeTextRunes {
		return domain.NewValidationError(field,
			fmt.Sprintf("must be at most %d characters", maxSupersedeTextRunes))
	}
	return nil
}

// caseTime renders a lifecycle instant; a zero instant stays empty so a field
// that was never written restores as never written.
func caseTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

// parseCaseTime is the inverse of caseTime. An unreadable value is zero: a
// damaged journal line must not invent an instant.
func parseCaseTime(value string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

// caseRevision reads the stored timeline position; a missing value is 0.
func caseRevision(value string) int {
	rev, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || rev < 0 {
		return 0
	}
	return rev
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
