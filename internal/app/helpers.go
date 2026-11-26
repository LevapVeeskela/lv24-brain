package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	maxRecordIDAttempts = 100
	maxListedItems      = 5
)

// The list capabilities are optional Store extensions used to build context
// and resolve record identifiers. Adapters that do not implement one still
// support writes; the corresponding context section simply reports no records.
type noteLister interface {
	ListNotes(ctx context.Context, projectID string) ([]domain.Note, error)
}

type decisionLister interface {
	ListDecisions(ctx context.Context, projectID string) ([]domain.Decision, error)
}

type attemptLister interface {
	ListAttempts(ctx context.Context, projectID string) ([]domain.Attempt, error)
}

type handoffLister interface {
	ListHandoffs(ctx context.Context, projectID string) ([]domain.Handoff, error)
}

func (a *App) now() time.Time {
	clock := a.deps.Now
	if clock == nil {
		clock = time.Now
	}
	return clock().UTC()
}

func (a *App) requireStore() error {
	if a.deps.Store != nil {
		return nil
	}
	if a.cfg.Mode == config.ModeServer {
		return fmt.Errorf("%w: server client is not configured; check server_url and BRAIN_TOKEN or run brain secret set server-token", domain.ErrUnsupported)
	}
	return fmt.Errorf("%w: brain data store is not available; check data_dir or run brain init", domain.ErrUnsupported)
}

func (a *App) resolveProject(ctx context.Context, projectID string) (domain.Project, error) {
	id := strings.TrimSpace(projectID)
	if id == "" {
		projects, err := a.deps.Store.ListProjects(ctx)
		if err != nil {
			return domain.Project{}, err
		}
		switch len(projects) {
		case 0:
			return domain.Project{}, domain.NewValidationError("project.id", "is required: no projects exist yet")
		case 1:
			return projects[0], nil
		default:
			// Like the legacy tool, pick the project registered for the
			// working directory before asking for --project.
			if resolved, err := a.ResolveSelector(ctx, ""); err == nil {
				return resolved.Project, nil
			}
			ids := make([]string, 0, len(projects))
			for _, project := range projects {
				ids = append(ids, project.ID)
			}
			return domain.Project{}, domain.NewValidationError(
				"project.id",
				"is required: multiple projects exist ("+strings.Join(ids, ", ")+"); pass --project",
			)
		}
	}
	project, err := a.deps.Store.GetProject(ctx, id)
	if err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

// ensureProject resolves the project of a write: an explicit id or alias, or
// the working directory's project when projectID is empty. An unknown project
// is created only with create=true, so a typo or an unregistered alias never
// silently starts a new project.
func (a *App) ensureProject(ctx context.Context, projectID string, create bool) (domain.Project, error) {
	id := strings.TrimSpace(projectID)
	if id == "" {
		// Existing projects are returned as is: reading must not write a
		// state file (a fresh state.md would look like a state change).
		return a.resolveProject(ctx, "")
	}
	if project, ok, err := a.LookupProject(ctx, id); err != nil {
		return domain.Project{}, err
	} else if ok {
		return project, nil
	}
	// A retired project name is a redirect, not a new project: "brotato"
	// resolves to the project that replaced it and never reaches the registry.
	if entry, retired := retiredPathBySlug(a.cfg, id); retired {
		if replacement, ok, err := a.LookupProject(ctx, entry.ReplacedBy); err != nil {
			return domain.Project{}, err
		} else if ok {
			return replacement, nil
		}
		return domain.Project{}, domain.NewValidationError("project", fmt.Sprintf(
			"%q is the retired name of %q, which is not registered here; register %s instead",
			id, entry.ReplacedBy, entry.ReplacedBy))
	}
	if !create {
		return domain.Project{}, domain.NewValidationError("project",
			fmt.Sprintf("%q is not a registered project id or alias; pass --create to register it", id))
	}
	project, err := a.deps.Store.GetProject(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		candidate := domain.Project{ID: id, Name: id, Kind: domain.ProjectKindOther}
		if validateErr := candidate.Validate(); validateErr != nil {
			return domain.Project{}, validateErr
		}
		if putErr := a.deps.Store.PutProject(ctx, candidate); putErr != nil {
			return domain.Project{}, putErr
		}
		project, err = a.deps.Store.GetProject(ctx, id)
	}
	if err != nil {
		return domain.Project{}, err
	}
	if err := a.ensureState(ctx, project.ID); err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

func (a *App) ensureState(ctx context.Context, projectID string) error {
	if _, err := a.deps.Store.GetState(ctx, projectID); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if _, err := a.deps.Store.PutState(ctx, domain.State{ProjectID: projectID, UpdatedAt: a.now()}); err != nil {
		return fmt.Errorf("initialize state for %s: %w", projectID, err)
	}
	return nil
}

func (a *App) listNotes(ctx context.Context, projectID string) ([]domain.Note, error) {
	lister, ok := a.deps.Store.(noteLister)
	if !ok {
		return nil, nil
	}
	return lister.ListNotes(ctx, projectID)
}

func (a *App) listDecisions(ctx context.Context, projectID string) ([]domain.Decision, error) {
	lister, ok := a.deps.Store.(decisionLister)
	if !ok {
		return nil, nil
	}
	return lister.ListDecisions(ctx, projectID)
}

func (a *App) listAttempts(ctx context.Context, projectID string) ([]domain.Attempt, error) {
	lister, ok := a.deps.Store.(attemptLister)
	if !ok {
		return nil, nil
	}
	return lister.ListAttempts(ctx, projectID)
}

func (a *App) listHandoffs(ctx context.Context, projectID string) ([]domain.Handoff, error) {
	lister, ok := a.deps.Store.(handoffLister)
	if !ok {
		return nil, nil
	}
	return lister.ListHandoffs(ctx, projectID)
}

// nextRecordID returns the globally unique id of a new record:
// <kind>-<YYYY-MM-DD>-<slug>-<ULID>. On the first try a caller-pinned id is
// used as is; every retry allocates a fresh ULID, so a taken name is never
// resolved with a numeric suffix that would keep the duplicate identity.
func nextRecordID(kind, slug string, now time.Time, pinned string, first bool) (string, error) {
	if first && pinned != "" {
		return pinned, nil
	}
	ulid, err := domain.NewULID(now)
	if err != nil {
		return "", err
	}
	return domain.NewRecordID(kind, now, slug, ulid), nil
}

// upsertIndex updates the search projection after a write. On failure it
// marks the index dirty so the next search repairs it.
func (a *App) upsertIndex(ctx context.Context, doc ports.Document) error {
	if a.deps.Index == nil {
		return nil
	}
	if err := a.upsertProjected(ctx, doc); err != nil {
		a.markIndexDirty()
		return err
	}
	return nil
}

// recordProjection is the derived, machine-independent representation of one
// record, stored next to the document by the index.
type recordProjection struct {
	// ContentHash is the sha256 of the normalized kind, title and body. The
	// record id, project and timestamps are not part of it: the same text
	// indexed on two clones hashes the same, which is what duplicate
	// collapsing compares.
	ContentHash string
	// Stub marks a low-signal record: an auto-generated label+id title or a
	// body that only says nothing happened. The flag is derived, never stored
	// in Markdown, so it is reversible.
	Stub bool
}

// projectDocument derives the index projection of one document. Title and body
// already exclude the record id and date markers the format keeps in
// frontmatter, so no extra stripping is needed here.
func projectDocument(doc ports.Document) recordProjection {
	return recordProjection{
		ContentHash: contentHash(doc.Kind, doc.Title, doc.Body),
		Stub:        IsStubRecord(doc.Kind, doc.Title, doc.Body),
	}
}

// contentHash normalizes each part to lowercase with collapsed whitespace and
// hashes "kind\ntitle\nbody". Separators are kept between the parts so a
// boundary shift cannot produce the same digest for different text.
func contentHash(kind, title, body string) string {
	normalized := strings.Join([]string{
		normalizeProjectionText(kind),
		normalizeProjectionText(title),
		normalizeProjectionText(body),
	}, "\n")
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func normalizeProjectionText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

// projectionWriter is the optional index capability that stores the derived
// projection columns. An index without it still upserts, but duplicate
// collapsing and stub filtering then see empty values.
type projectionWriter interface {
	UpsertProjection(ctx context.Context, doc ports.Document, contentHash string, stub bool) error
}

// projectionBatchWriter is projectionWriter for a whole reindex chunk.
type projectionBatchWriter interface {
	UpsertProjectionBatch(ctx context.Context, docs []ports.Document, contentHashes []string, stubs []bool) error
}

// upsertProjected writes one document with its derived projection.
func (a *App) upsertProjected(ctx context.Context, doc ports.Document) error {
	return upsertProjected(ctx, a.deps.Index, doc)
}

func upsertProjected(ctx context.Context, index ports.Index, doc ports.Document) error {
	projection := projectDocument(doc)
	if writer, ok := index.(projectionWriter); ok {
		return writer.UpsertProjection(ctx, doc, projection.ContentHash, projection.Stub)
	}
	return index.Upsert(ctx, doc)
}

// projectionColumns derives the hash and stub slices of one batch in document
// order, which is the shape UpsertProjectionBatch expects.
func projectionColumns(docs []ports.Document) ([]string, []bool) {
	hashes := make([]string, len(docs))
	stubs := make([]bool, len(docs))
	for idx, doc := range docs {
		projection := projectDocument(doc)
		hashes[idx] = projection.ContentHash
		stubs[idx] = projection.Stub
	}
	return hashes, stubs
}

// provenanceColumns projects a record's provenance into the two index columns
// the ranking reads. The empty values of a record written before provenance
// existed are filled with their documented defaults (human, verified), so the
// projection states what a reader acts on; an unknown trust level is folded to
// inferred by EffectiveTrust, never to verified.
func provenanceColumns(provenance domain.Provenance) (origin, trust string) {
	return string(provenance.Origin.Effective()), string(provenance.EffectiveTrust())
}

func noteDocument(note domain.Note) ports.Document {
	body := note.Body
	if len(note.Tags) > 0 {
		body = joinText(body, strings.Join(note.Tags, " "))
	}
	origin, trust := provenanceColumns(note.Provenance)
	return ports.Document{
		ID:               indexKey(note.ProjectID, note.ID),
		ProjectID:        note.ProjectID,
		Kind:             "note",
		Title:            fallbackTitle(note.Title, note.Body),
		Body:             body,
		Origin:           origin,
		Trust:            trust,
		UpdatedAt:        updatedOrCreated(note.UpdatedAt, note.CreatedAt),
		ContentUpdatedAt: contentUpdatedOr(note.ContentUpdatedAt, updatedOrCreated(note.UpdatedAt, note.CreatedAt)),
		// ValidFrom defaults to the creation time so an as-of query has an
		// anchor even for records written before validity existed.
		ValidFrom:    note.Validity.EffectiveFrom(note.CreatedAt),
		ValidTo:      note.Validity.ValidTo,
		SupersededBy: note.Validity.SupersededBy,
		Supersedes:   note.Validity.Supersedes,
		Confidence:   note.Validity.Confidence,
		Pinned:       note.Pinned,
		Subtype:      note.Subtype,
		Scope:        string(note.Scope),
		TopicKey:     note.TopicKey,
		Occurrences:  note.Taxonomy.NormalizedOccurrences(),
		Relations:    relationLinks(note.Relations),
	}
}

func decisionDocument(decision domain.Decision) ports.Document {
	origin, trust := provenanceColumns(decision.Provenance)
	return ports.Document{
		ID:        indexKey(decision.ProjectID, decision.ID),
		ProjectID: decision.ProjectID,
		Kind:      "decision",
		Title:     decision.Title,
		Body: joinText(
			decision.Context,
			strings.Join(decision.Alternatives, "\n"),
			strings.Join(decision.Consequences, "\n"),
			strings.Join(decision.Evidence, "\n"),
		),
		Origin:           origin,
		Trust:            trust,
		UpdatedAt:        updatedOrCreated(decision.UpdatedAt, decision.CreatedAt),
		ContentUpdatedAt: contentUpdatedOr(decision.ContentUpdatedAt, updatedOrCreated(decision.UpdatedAt, decision.CreatedAt)),
		ValidFrom:        decision.Validity.EffectiveFrom(decision.CreatedAt),
		ValidTo:          decision.Validity.ValidTo,
		SupersededBy:     decision.Validity.SupersededBy,
		Supersedes:       decision.Validity.Supersedes,
		Status:           string(decision.Status),
		Confidence:       decision.Validity.Confidence,
		Pinned:           decision.Pinned,
		Subtype:          decision.Subtype,
		Scope:            string(decision.Scope),
		TopicKey:         decision.TopicKey,
		Occurrences:      decision.Taxonomy.NormalizedOccurrences(),
		Relations:        relationLinks(decision.Relations),
	}
}

func attemptDocument(attempt domain.Attempt) ports.Document {
	origin, trust := provenanceColumns(attempt.Provenance)
	return ports.Document{
		ID:               indexKey(attempt.ProjectID, attempt.ID),
		ProjectID:        attempt.ProjectID,
		Kind:             "attempt",
		Title:            attempt.Title,
		Body:             joinText(attempt.Hypothesis, attempt.Result, attempt.Reason),
		Origin:           origin,
		Trust:            trust,
		UpdatedAt:        updatedOrCreated(attempt.UpdatedAt, attempt.CreatedAt),
		ContentUpdatedAt: contentUpdatedOr(attempt.ContentUpdatedAt, updatedOrCreated(attempt.UpdatedAt, attempt.CreatedAt)),
		ValidFrom:        attempt.Validity.EffectiveFrom(attempt.CreatedAt),
		ValidTo:          attempt.Validity.ValidTo,
		SupersededBy:     attempt.Validity.SupersededBy,
		Supersedes:       attempt.Validity.Supersedes,
		Confidence:       attempt.Validity.Confidence,
		Pinned:           attempt.Pinned,
		Subtype:          attempt.Subtype,
		Scope:            string(attempt.Scope),
		TopicKey:         attempt.TopicKey,
		Occurrences:      attempt.Taxonomy.NormalizedOccurrences(),
		Relations:        relationLinks(attempt.Relations),
	}
}

func handoffDocument(handoff domain.Handoff) ports.Document {
	origin, trust := provenanceColumns(handoff.Provenance)
	return ports.Document{
		ID:               indexKey(handoff.ProjectID, handoff.ID),
		ProjectID:        handoff.ProjectID,
		Kind:             "handoff",
		Title:            handoffTitle(handoff),
		Body:             joinText(handoff.Summary, handoff.GitContext, handoff.NextAction),
		Origin:           origin,
		Trust:            trust,
		UpdatedAt:        updatedOrCreated(handoff.UpdatedAt, handoff.CreatedAt),
		ContentUpdatedAt: contentUpdatedOr(handoff.ContentUpdatedAt, updatedOrCreated(handoff.UpdatedAt, handoff.CreatedAt)),
		ValidFrom:        handoff.Validity.EffectiveFrom(handoff.CreatedAt),
		ValidTo:          handoff.Validity.ValidTo,
		SupersededBy:     handoff.Validity.SupersededBy,
		Supersedes:       handoff.Validity.Supersedes,
		Confidence:       handoff.Validity.Confidence,
		Pinned:           handoff.Pinned,
		Subtype:          handoff.Subtype,
		Scope:            string(handoff.Scope),
		TopicKey:         handoff.TopicKey,
		Occurrences:      handoff.Taxonomy.NormalizedOccurrences(),
		Relations:        relationLinks(handoff.Relations),
	}
}

// ideaDocument projects one idea into the search index. The body keeps the
// idea text and appends the tags so a tag is searchable without a separate
// column. Ideas do not write content_updated_at, so the projection uses the
// revision for it, and the due instant is indexed in Unix milliseconds by the
// adapter. The planning fields travel in dedicated columns: search filters and
// rows read them instead of parsing the body.
func ideaDocument(idea domain.Idea) ports.Document {
	origin, trust := provenanceColumns(idea.Provenance)
	body := idea.Body
	if len(idea.Tags) > 0 {
		body = joinText(body, strings.Join(idea.Tags, " "))
	}
	updated := updatedOrCreated(idea.UpdatedAt, idea.CreatedAt)
	doc := ports.Document{
		ID:               indexKey(idea.ProjectID, idea.ID),
		ProjectID:        idea.ProjectID,
		Kind:             ports.RecordIdea,
		Title:            idea.Title,
		Body:             body,
		Origin:           origin,
		Trust:            trust,
		UpdatedAt:        updated,
		ContentUpdatedAt: updated,
		ValidFrom:        idea.Validity.EffectiveFrom(idea.CreatedAt),
		ValidTo:          idea.Validity.ValidTo,
		SupersededBy:     idea.Validity.SupersededBy,
		Supersedes:       idea.Validity.Supersedes,
		Status:           string(idea.Status),
		Priority:         string(idea.Priority),
		DueDate:          idea.DueDate,
		Confidence:       idea.Validity.Confidence,
		Subtype:          idea.Subtype,
		Scope:            string(idea.Scope),
		TopicKey:         idea.TopicKey,
		Occurrences:      idea.Taxonomy.NormalizedOccurrences(),
		Relations:        relationLinks(idea.Relations),
	}
	if idea.DueAt != nil {
		doc.DueAt = idea.DueAt.UTC()
	}
	return doc
}

func handoffTitle(handoff domain.Handoff) string {
	if handoff.CreatedAt.IsZero() {
		return "Handoff"
	}
	return "Handoff " + handoff.CreatedAt.UTC().Format("2006-01-02 15:04")
}

func updatedOrCreated(updated, created time.Time) time.Time {
	if !updated.IsZero() {
		return updated
	}
	return created
}

// contentUpdatedOr resolves the content edit time of an indexed record: the
// explicit content time, else the record revision. A record written before
// content_updated_at existed changed its text when it was last written, and
// reporting it once too often is safer than hiding it.
func contentUpdatedOr(contentUpdatedAt, updatedAt time.Time) time.Time {
	if !contentUpdatedAt.IsZero() {
		return contentUpdatedAt
	}
	return updatedAt
}

func fallbackTitle(title, body string) string {
	title = strings.TrimSpace(title)
	if title != "" {
		return title
	}
	if line := firstLine(body); line != "" {
		return line
	}
	return "Note"
}

func joinText(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return strings.Join(out, "\n\n")
}

func firstLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return truncateRunes(trimmed, 160)
		}
	}
	return ""
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

func humanDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	switch {
	case value < time.Minute:
		return "just now"
	case value < time.Hour:
		return fmt.Sprintf("%dm", int(value/time.Minute))
	case value < 24*time.Hour:
		hours := int(value / time.Hour)
		minutes := int((value % time.Hour) / time.Minute)
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		days := int(value / (24 * time.Hour))
		hours := int((value % (24 * time.Hour)) / time.Hour)
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, hours)
	}
}

func stateAge(now time.Time, state domain.State) string {
	if state.UpdatedAt.IsZero() {
		return "unknown"
	}
	return humanDuration(now.Sub(state.UpdatedAt))
}

func freshness(now time.Time, state domain.State) string {
	if state.FreshUntil.IsZero() {
		return "not set"
	}
	if now.After(state.FreshUntil) {
		return "stale since " + state.FreshUntil.UTC().Format(time.RFC3339)
	}
	return "fresh until " + state.FreshUntil.UTC().Format(time.RFC3339)
}

func parseSince(value string, now time.Time) (time.Time, bool, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return time.Time{}, false, nil
	}
	unit := value[len(value)-1]
	number := strings.TrimSpace(value[:len(value)-1])
	multiplier := time.Hour
	switch unit {
	case 'm':
		multiplier = time.Minute
	case 'h':
		multiplier = time.Hour
	case 'd':
		multiplier = 24 * time.Hour
	case 'w':
		multiplier = 7 * 24 * time.Hour
	default:
		return time.Time{}, false, domain.NewValidationError("review.since", "must look like 30m, 24h, 7d, or 2w")
	}
	count, err := strconv.Atoi(number)
	if err != nil || count <= 0 {
		return time.Time{}, false, domain.NewValidationError("review.since", "must look like 30m, 24h, 7d, or 2w")
	}
	return now.Add(-time.Duration(count) * multiplier), true, nil
}

func compareVersions(a, b string) int {
	ac, ap := splitVersion(a)
	bc, bp := splitVersion(b)
	if cmp := compareVersionCore(ac, bc); cmp != 0 {
		return cmp
	}
	switch {
	case ap == "" && bp == "":
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	default:
		return strings.Compare(ap, bp)
	}
}

func splitVersion(value string) (string, string) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	value = strings.TrimPrefix(value, "V")
	if idx := strings.IndexByte(value, '+'); idx >= 0 {
		value = value[:idx]
	}
	if idx := strings.IndexByte(value, '-'); idx >= 0 {
		return value[:idx], value[idx+1:]
	}
	return value, ""
}

func compareVersionCore(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	count := len(aParts)
	if len(bParts) > count {
		count = len(bParts)
	}
	for i := 0; i < count; i++ {
		aPart := "0"
		if i < len(aParts) && aParts[i] != "" {
			aPart = aParts[i]
		}
		bPart := "0"
		if i < len(bParts) && bParts[i] != "" {
			bPart = bParts[i]
		}
		aNumber, aErr := strconv.Atoi(aPart)
		bNumber, bErr := strconv.Atoi(bPart)
		if aErr == nil && bErr == nil {
			switch {
			case aNumber < bNumber:
				return -1
			case aNumber > bNumber:
				return 1
			}
			continue
		}
		if cmp := strings.Compare(aPart, bPart); cmp != 0 {
			return cmp
		}
	}
	return 0
}

// LookupProject finds a project by exact id or alias (case-insensitive); it
// never interprets the selector as a path.
func (a *App) LookupProject(ctx context.Context, selector string) (domain.Project, bool, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return domain.Project{}, false, nil
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return domain.Project{}, false, err
	}
	for _, project := range projects {
		if strings.EqualFold(project.ID, selector) || containsFold(project.Aliases, selector) {
			return project, true, nil
		}
	}
	return domain.Project{}, false, nil
}
