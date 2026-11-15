package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// DefaultIngestMaxBatches bounds a run whose options set no MaxBatches,
	// so a source that keeps reporting More cannot run forever.
	DefaultIngestMaxBatches = 1000
	// maxReportedSkips caps RunConnectionResult.Skips.
	maxReportedSkips = 100
	// persistGrace bounds storing a batch that was read before the run's
	// context ended: a read batch is finished, not dropped halfway.
	persistGrace = 30 * time.Second
)

// Run counters added to ConnectorRun.Counts by item-level runs.
const (
	CountRedactions      = "redactions"
	CountEmailsHashed    = "emailsHashed"
	CountBodiesTruncated = "bodiesTruncated"
	CountIndexed         = "indexed"
	CountIndexRemoved    = "indexRemoved"
)

// ItemIndexer refreshes the search index for stored items; *App implements
// it with IndexItems.
type ItemIndexer interface {
	IndexItems(ctx context.Context, connectionID string, keys []string) (ItemIndexResult, error)
}

// publishPolicyStore is implemented by item stores that place data by
// publish policy (fsstore writes a per-connection .gitignore). Server stores
// keep every item server-side and do not implement it.
type publishPolicyStore interface {
	SetPublishPolicy(ctx context.Context, connectionID string, policy domain.PublishPolicy) error
}

// publishPolicyReader lets the host skip rewriting an unchanged policy and
// report a change.
type publishPolicyReader interface {
	PublishPolicy(ctx context.Context, connectionID string) (domain.PublishPolicy, error)
}

// legacyRunReporter is implemented by connectorbuild.LegacySource: its run
// record carries the status and counts of a snapshot connector.
type legacyRunReporter interface {
	LastRun() (domain.ConnectorRun, bool)
}

// Ingestion runs item-level sources for configured connections (WP0.4). It
// owns the host contract of ports.Source: one run per connection at a time,
// items persisted before checkpoints, policy applied to every item, and a
// run record for each attempt.
//
// Connections, Registry, Items, Checkpoints and Locker are required. Index
// and Runs are optional (server mode indexes in Postgres; tests may skip run
// history). Deps carries the factory inputs (credentials, HTTP client and the
// legacy secret store, sink and state path); its Now is replaced by Now.
type Ingestion struct {
	Connections ports.ConnectionStore
	Registry    *connectorbuild.Registry
	Deps        connectorbuild.Deps
	Items       ports.ItemStore
	Checkpoints ports.CheckpointStore
	Locker      ports.ConnectionLocker
	Index       ItemIndexer
	Runs        ports.ConnectorRunStore
	// PreparePublish runs on every run before item writes. A local git runtime
	// uses it to untrack committed data when a connection becomes local-only.
	PreparePublish func(ctx context.Context, connectionID string, policy domain.PublishPolicy) error
	// RawZoneTracked reports whether the shared tree already publishes the
	// connection's items or checkpoints. A git runtime wires it so a zone that an
	// earlier engine published is grandfathered as shared instead of being
	// silently untracked; nil means "nothing is tracked" (local mode).
	RawZoneTracked func(ctx context.Context, connectionID string) (bool, error)
	// DisallowLocalOnlyItems makes the host refuse to persist items whose
	// effective raw-zone policy is local-only. Server mode sets it because its
	// only item store is shared; local and git mode leave it false, so a
	// local-only connection still stores items on this machine. A refused item
	// is counted and reported as a skip; the run still reads and checkpoints.
	DisallowLocalOnlyItems bool
	// Now is the clock; nil selects time.Now.
	Now func() time.Time
	// NewRunID returns a run ID; nil derives one from the start time.
	NewRunID func(connectionID string, started time.Time) string
}

// RunConnectionOptions bounds one run.
type RunConnectionOptions struct {
	ConnectionID string
	// Since narrows reads to provider updates at or after it.
	Since *time.Time
	// Limit caps the items read in this run (0 = no cap). The last batch is
	// stored whole, so a run may store slightly more.
	Limit int
	// MaxBatches caps Source.Read calls (0 = DefaultIngestMaxBatches).
	MaxBatches int
	// Deadline stops the run before the next read once reached (by Now) and
	// interrupts a read in progress; zero means none.
	Deadline time.Time
	// InitialWindow, when Since is nil and the connection has no checkpoint
	// for any stream yet (first run, or a source without checkpoints such as
	// legacy IMAP), reads only the last InitialWindow: Since = now - window.
	InitialWindow time.Duration
}

// ItemSkip names one item that was not stored and why. Key is the hashed
// item key, never the external ID, which may carry personal data.
type ItemSkip struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// RunConnectionResult reports one run.
type RunConnectionResult struct {
	// Run is the run record (persisted when Runs is set). It is zero when
	// Busy is true.
	Run domain.ConnectorRun `json:"run"`
	// Busy is true when another process or goroutine is running the
	// connection; nothing was read or written.
	Busy bool `json:"busy,omitempty"`
	// Publish is the effective publish policy applied before the first write.
	Publish domain.PublishPolicy `json:"publish,omitempty"`
	// PublishNotice explains a grandfather decision (the raw zone is already
	// tracked by git and no explicit policy is set) or a git index that could not
	// be read. It is advisory and changes nothing by itself.
	PublishNotice string `json:"publishNotice,omitempty"`
	// PublishChanged reports a policy different from the one recorded by
	// the item store; on a shared -> local-only switch the caller must untrack
	// connectors/<id> from git (git rm --cached -r).
	PublishChanged bool `json:"publishChanged,omitempty"`
	// PreviousPublish is the recorded policy before the change.
	PreviousPublish domain.PublishPolicy `json:"previousPublish,omitempty"`
	// Skips lists up to 100 skipped items.
	Skips []ItemSkip `json:"skips,omitempty"`
}

// RunConnection runs one connection once within the options' budget.
//
// Errors before a run starts (unknown connection, invalid options, a lock
// failure other than busy) return no run. Once started, every outcome is a
// run record: a failed run (nothing stored) also returns its error; a partial
// run (some batches stored, then an error or cancellation) returns nil with
// Run.Status partial and Run.Error set; a run cut by Limit, MaxBatches or
// Deadline is a success with Stats.Truncated set. A context that ends during
// preparation (before the source is read) truncates the run the same way.
func (s *Ingestion) RunConnection(ctx context.Context, opts RunConnectionOptions) (RunConnectionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.validate(opts); err != nil {
		return RunConnectionResult{}, err
	}
	connection, err := s.Connections.Get(ctx, opts.ConnectionID)
	if err != nil {
		return RunConnectionResult{}, fmt.Errorf("load connection %s: %w", opts.ConnectionID, err)
	}
	release, err := s.Locker.TryLock(ctx, connection.ID)
	if errors.Is(err, domain.ErrConflict) {
		return RunConnectionResult{Busy: true}, nil
	}
	if err != nil {
		return RunConnectionResult{}, fmt.Errorf("lock connection %s: %w", connection.ID, err)
	}
	defer func() { _ = release() }()
	// Reload under the lock: an update between the first read and the lock
	// (for example shared -> local-only) must not run with the stale snapshot.
	// Catalog mutations that affect an existing connection (remove; no update
	// command exists yet) take the same lock; add/migrate only create entries.
	if connection, err = s.Connections.Get(ctx, connection.ID); err != nil {
		return RunConnectionResult{}, fmt.Errorf("reload connection %s under lock: %w", opts.ConnectionID, err)
	}

	run := &ingestRun{host: s, opts: opts, connection: connection, started: s.now()}
	run.execute(ctx)
	return run.finish(ctx)
}

func (s *Ingestion) validate(opts RunConnectionOptions) error {
	switch {
	case s.Connections == nil || s.Registry == nil || s.Items == nil || s.Checkpoints == nil || s.Locker == nil:
		return domain.NewValidationError("ingestion", "connections, registry, items, checkpoints and locker are required")
	case opts.Limit < 0 || opts.MaxBatches < 0 || opts.InitialWindow < 0:
		return domain.NewValidationError("ingestion.options", "limit, maxBatches and initialWindow must not be negative")
	}
	return domain.ValidateConnectionID("connection.id", opts.ConnectionID)
}

func (s *Ingestion) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// ingestRun is the state of one locked run.
type ingestRun struct {
	host       *Ingestion
	opts       RunConnectionOptions
	connection domain.Connection
	started    time.Time

	source      ports.Source
	descriptor  domain.SourceDescriptor
	policy      ingestPolicy
	checkpoints map[string]domain.Checkpoint
	since       *time.Time

	stats   domain.RunStats
	report  itemReport
	indexed ItemIndexResult
	skips   []ItemSkip
	result  RunConnectionResult
	err     error
}

func (r *ingestRun) execute(ctx context.Context) {
	if err := r.prepare(ctx); err != nil {
		r.recordPrepareError(ctx, err)
		return
	}
	maxBatches := r.opts.MaxBatches
	if maxBatches == 0 {
		maxBatches = DefaultIngestMaxBatches
	}
	for {
		if reason := r.budgetReason(ctx, maxBatches); reason != "" {
			r.stats.Truncated = reason
			return
		}
		more, err := r.batch(ctx)
		if err != nil {
			r.err = err
			return
		}
		if !more {
			return
		}
	}
}

// prepare builds the source, applies the publish policy and loads checkpoints.
func (r *ingestRun) prepare(ctx context.Context) error {
	deps := r.host.Deps
	deps.Now = r.host.now
	source, err := r.host.Registry.Build(r.connection, deps)
	if err != nil {
		return fmt.Errorf("build %s source: %w", r.connection.Provider, err)
	}
	r.source = source
	r.descriptor = source.Descriptor()
	if err := r.descriptor.Validate(); err != nil {
		return fmt.Errorf("source descriptor: %w", err)
	}
	r.policy = newIngestPolicy(r.connection, r.descriptor)
	if err := r.applyPublishPolicy(ctx); err != nil {
		return err
	}
	stored, err := r.host.Checkpoints.List(ctx, r.connection.ID)
	if err != nil {
		return fmt.Errorf("load checkpoints: %w", err)
	}
	r.checkpoints = make(map[string]domain.Checkpoint, len(stored))
	for _, checkpoint := range stored {
		r.checkpoints[checkpoint.Stream] = checkpoint.Clone()
	}
	r.since = r.initialSince()
	return nil
}

// recordPrepareError records a preparation failure. Preparation is setup only:
// a run whose budget ends while applying the publish policy or loading
// checkpoints has read nothing from the source, so it is truncated exactly like
// a deadline that ends a read instead of being reported as a failed connector.
// The error must be a context error from the run's own context; a sub-operation
// deadline that expired on its own stays a real failure, and every other
// preparation error fails the run even when the context ended meanwhile.
func (r *ingestRun) recordPrepareError(ctx context.Context, err error) {
	if ctxErr := ctx.Err(); ctxErr != nil && isContextError(err) {
		r.stats.Truncated = contextReason(ctxErr)
		return
	}
	r.err = err
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (r *ingestRun) applyPublishPolicy(ctx context.Context) error {
	resolution, notice := r.resolvePublishPolicy(ctx)
	r.result.Publish = resolution.Policy
	if notice != "" {
		r.result.PublishNotice = notice
	}
	if resolution.Grandfathered {
		// Compatibility first: the raw zone was already published by an earlier
		// engine and the owner has not chosen a policy. Keep the files, the
		// marker and the git index untouched and only recommend a choice; the
		// next explicit `publish` setting takes over.
		return nil
	}
	policy := resolution.Policy
	if setter, ok := r.host.Items.(publishPolicyStore); ok {
		if reader, ok := r.host.Items.(publishPolicyReader); ok {
			current, err := reader.PublishPolicy(ctx, r.connection.ID)
			if err != nil {
				return fmt.Errorf("read publish policy: %w", err)
			}
			if current != policy {
				r.result.PublishChanged = true
				r.result.PreviousPublish = current
			}
			// Applying an unchanged policy is cheap and idempotent; it also
			// upgrades an older marker that did not ignore checkpoints/ without
			// touching any tracked item.
			if err := setter.SetPublishPolicy(ctx, r.connection.ID, policy); err != nil {
				return fmt.Errorf("set publish policy: %w", err)
			}
		} else if err := setter.SetPublishPolicy(ctx, r.connection.ID, policy); err != nil {
			return fmt.Errorf("set publish policy: %w", err)
		}
	}
	// The hook is independent of the store's policy persistence: stores that
	// keep everything server-side have no publish policy but still must run it.
	if r.host.PreparePublish != nil {
		if err := r.host.PreparePublish(ctx, r.connection.ID, policy); err != nil {
			return fmt.Errorf("prepare publish policy: %w", err)
		}
	}
	return nil
}

// resolvePublishPolicy decides the raw-zone policy for this run and returns an
// optional notice for the run result. An explicit connection setting wins; a
// host without a machine-local raw zone (server mode) keeps the descriptor
// sensitivity policy; otherwise a zone git already tracks is grandfathered as
// shared and a zone with nothing tracked is machine-local.
func (r *ingestRun) resolvePublishPolicy(ctx context.Context) (domain.RawZoneResolution, string) {
	if r.host.DisallowLocalOnlyItems {
		// The server's only item store is the shared raw zone, so "local-only"
		// there means "must not be stored at all" and the descriptor decides.
		return domain.RawZoneResolution{Policy: r.connection.EffectivePublish(r.descriptor), Explicit: true}, ""
	}
	tracked := false
	if r.host.RawZoneTracked != nil {
		found, err := r.host.RawZoneTracked(ctx, r.connection.ID)
		if err != nil {
			// Never guess local-only when the index cannot be read: that would
			// untrack a published zone on a transient git failure.
			message, _ := RedactIngest(err.Error())
			return domain.RawZoneResolution{Policy: domain.PublishShared, Grandfathered: true},
				fmt.Sprintf("cannot read the git index for %s (%s): the raw zone stays as it is; run brain connection publish %s shared|local-only to choose", r.connection.ID, message, r.connection.ID)
		}
		tracked = found
	}
	resolution := domain.ResolveRawZonePolicy(r.connection.Publish, tracked)
	if resolution.Grandfathered {
		return resolution, fmt.Sprintf("raw zone of %s is already tracked by git and no publish policy is set: keeping it shared (grandfathered); run brain connection publish %s shared|local-only to choose", r.connection.ID, r.connection.ID)
	}
	return resolution, ""
}

// initialSince returns the run's lower bound: the caller's Since, or the
// initial window when no stream has a checkpoint yet.
func (r *ingestRun) initialSince() *time.Time {
	if r.opts.Since != nil {
		since := *r.opts.Since
		return &since
	}
	if r.opts.InitialWindow <= 0 || r.hasCheckpoint() {
		return nil
	}
	since := r.host.now().Add(-r.opts.InitialWindow)
	return &since
}

func (r *ingestRun) hasCheckpoint() bool {
	if len(r.descriptor.Streams) == 0 {
		return len(r.checkpoints) > 0
	}
	for _, stream := range r.descriptor.Streams {
		if _, ok := r.checkpoints[stream]; ok {
			return true
		}
	}
	return false
}

// budgetReason reports why the run must stop before the next read.
func (r *ingestRun) budgetReason(ctx context.Context, maxBatches int) domain.TruncationReason {
	switch {
	case ctx.Err() != nil:
		return contextReason(ctx.Err())
	case r.opts.Limit > 0 && r.stats.Seen >= r.opts.Limit:
		return domain.TruncatedLimit
	case r.stats.Batches >= maxBatches:
		return domain.TruncatedLimit
	case !r.opts.Deadline.IsZero() && !r.host.now().Before(r.opts.Deadline):
		return domain.TruncatedDeadline
	}
	return ""
}

func contextReason(err error) domain.TruncationReason {
	if errors.Is(err, context.DeadlineExceeded) {
		return domain.TruncatedDeadline
	}
	return domain.TruncatedCanceled
}

// batch reads, stores, indexes and checkpoints one batch. It reports whether
// the source has more.
func (r *ingestRun) batch(ctx context.Context) (bool, error) {
	readBatch, err := r.read(ctx)
	if err != nil {
		return false, err
	}
	if readBatch == nil {
		return false, nil
	}
	if err := validateCheckpoints(readBatch.Checkpoints); err != nil {
		return false, fmt.Errorf("source returned an invalid checkpoint: %w", err)
	}
	if readBatch.More && len(readBatch.Items) == 0 && !r.advances(readBatch.Checkpoints) {
		return false, errors.New("source reported more data without returning items or new checkpoints")
	}
	items, report, skips, seen, skipped := r.normalize(lastPerRef(readBatch.Items))
	items, skips, dropped := r.dropLocalOnlyItems(items, skips)
	skipped += dropped
	// A batch that was read is stored completely even if the run's context
	// ends meanwhile; the grace timeout still bounds a stuck store.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistGrace)
	defer cancel()
	if err := r.store(writeCtx, items, report, skips, seen, skipped); err != nil {
		return false, err
	}
	for _, checkpoint := range readBatch.Checkpoints {
		if err := r.host.Checkpoints.Put(writeCtx, r.connection.ID, checkpoint); err != nil {
			return false, fmt.Errorf("store checkpoint %s: %w", checkpoint.Stream, err)
		}
		r.checkpoints[checkpoint.Stream] = checkpoint.Clone()
	}
	r.stats.Batches++
	return readBatch.More, nil
}

// read calls the source. A nil batch with nil error means the run budget
// (context or deadline) ended the read: the batch is discarded and the run
// is truncated, not failed.
func (r *ingestRun) read(ctx context.Context) (*ports.ReadBatch, error) {
	readCtx := ctx
	if !r.opts.Deadline.IsZero() {
		var cancel context.CancelFunc
		readCtx, cancel = context.WithTimeout(ctx, r.opts.Deadline.Sub(r.host.now()))
		defer cancel()
	}
	request := ports.ReadRequest{
		Connection:  r.connection,
		Checkpoints: cloneCheckpoints(r.checkpoints),
		Since:       r.since,
		Limit:       r.remaining(),
	}
	batch, err := r.source.Read(readCtx, request)
	if err == nil {
		return &batch, nil
	}
	if readCtx.Err() != nil {
		r.stats.Truncated = contextReason(readCtx.Err())
		return nil, nil
	}
	return nil, fmt.Errorf("read %s: %w", r.connection.ID, err)
}

func (r *ingestRun) remaining() int {
	if r.opts.Limit == 0 {
		return 0
	}
	return r.opts.Limit - r.stats.Seen
}

func (r *ingestRun) advances(checkpoints []domain.Checkpoint) bool {
	for _, checkpoint := range checkpoints {
		previous, ok := r.checkpoints[checkpoint.Stream]
		if !ok || !previous.Equal(checkpoint) {
			return true
		}
	}
	return false
}

// normalize applies the ingest policy; skipped items are counted here.
func (r *ingestRun) normalize(raw []domain.Item) ([]domain.Item, itemReport, []ItemSkip, int, int) {
	observedAt := r.host.now()
	items := make([]domain.Item, 0, len(raw))
	var report itemReport
	skips := make([]ItemSkip, 0)
	skipped := 0
	for _, item := range raw {
		normalized, itemStats, err := r.policy.normalizeItem(item, observedAt)
		report.add(itemStats)
		if err != nil {
			skipped++
			if len(skips) < maxReportedSkips {
				key := domain.ItemRef{ConnectionID: r.connection.ID, ExternalID: item.Ref.ExternalID}.Key()
				message, _ := RedactIngest(err.Error())
				skips = append(skips, ItemSkip{Key: key, Reason: message})
			}
			continue
		}
		items = append(items, normalized)
	}
	return items, report, skips, len(raw), skipped
}

// dropLocalOnlyItems discards normalized items when the host has no local-only
// raw zone (server mode). The run still checkpoints, so the next run resumes;
// every dropped item is reported as a skip instead of silently vanishing.
func (r *ingestRun) dropLocalOnlyItems(items []domain.Item, skips []ItemSkip) ([]domain.Item, []ItemSkip, int) {
	if !r.host.DisallowLocalOnlyItems || r.result.Publish != domain.PublishLocalOnly || len(items) == 0 {
		return items, skips, 0
	}
	for _, item := range items {
		if len(skips) >= maxReportedSkips {
			break
		}
		skips = append(skips, ItemSkip{Key: item.Key(), Reason: localOnlySkipReason})
	}
	return nil, skips, len(items)
}

// localOnlySkipReason explains an item the host refused because the only item
// store is shared (server mode).
const localOnlySkipReason = "publish policy local-only: this host has no local-only raw zone"

// store applies items and indexes their keys. Stats change only when both
// succeed, so a failed batch leaves the counts of stored batches.
func (r *ingestRun) store(ctx context.Context, items []domain.Item, report itemReport, skips []ItemSkip, seen, skipped int) error {
	outcomes := make([]ports.UpsertOutcome, 0, len(items))
	if len(items) > 0 {
		var err error
		outcomes, err = r.host.Items.Apply(ctx, items)
		if err != nil {
			return fmt.Errorf("store items: %w", err)
		}
		if len(outcomes) != len(items) {
			return fmt.Errorf("store items: %d outcomes for %d items", len(outcomes), len(items))
		}
	}
	// Apply is durable at this point. Count the items before indexing so an
	// index failure still leaves a valid partial run record.
	for index, outcome := range outcomes {
		r.count(items[index].Deleted, outcome)
	}
	r.stats.Seen += seen
	r.stats.Skipped += skipped
	r.report.add(report)
	for _, skip := range skips {
		if len(r.skips) >= maxReportedSkips {
			break
		}
		r.skips = append(r.skips, skip)
	}
	keys := make([]string, len(items))
	for index, item := range items {
		keys[index] = item.Key()
	}
	if r.host.Index != nil {
		indexed, err := r.host.Index.IndexItems(ctx, r.connection.ID, keys)
		if err != nil {
			return fmt.Errorf("index items: %w", err)
		}
		r.indexed.Indexed += indexed.Indexed
		r.indexed.Removed += indexed.Removed
	}
	return nil
}

func (r *ingestRun) count(deleted bool, outcome ports.UpsertOutcome) {
	switch {
	case outcome == ports.UpsertUnchanged:
		r.stats.Unchanged++
	case deleted:
		r.stats.Deleted++
	default:
		r.stats.Upserted++
	}
}

// finish builds and persists the run record.
func (r *ingestRun) finish(ctx context.Context) (RunConnectionResult, error) {
	finished := r.host.now()
	stats := r.stats
	run := domain.ConnectorRun{
		ID:           r.runID(),
		Source:       r.connection.Provider,
		StartedAt:    r.started,
		FinishedAt:   &finished,
		Status:       r.status(),
		Counts:       r.counts(),
		ConnectionID: r.connection.ID,
		Stats:        &stats,
	}
	if r.err != nil {
		run.Error, _ = RedactIngest(r.err.Error())
	}
	r.mergeLegacy(&run)
	result := r.result
	result.Run = run
	result.Skips = r.skips
	if r.host.Runs != nil {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistGrace)
		defer cancel()
		if err := r.host.Runs.AppendConnectorRun(persistCtx, run); err != nil {
			return result, errors.Join(r.failure(run), fmt.Errorf("record run: %w", err))
		}
	}
	return result, r.failure(run)
}

func (r *ingestRun) failure(run domain.ConnectorRun) error {
	if run.Status != domain.ConnectorFailed {
		return nil
	}
	if r.err != nil {
		return errors.New(firstNonEmptyString(run.Error, "connector run failed"))
	}
	return errors.New(firstNonEmptyString(run.Error, run.Source+" run failed"))
}

func (r *ingestRun) status() domain.ConnectorStatus {
	switch {
	case r.err != nil && r.stats.Seen == 0:
		return domain.ConnectorFailed
	case r.err != nil || r.stats.Truncated == domain.TruncatedCanceled:
		return domain.ConnectorPartial
	default:
		return domain.ConnectorSuccess
	}
}

func (r *ingestRun) counts() map[string]int {
	return map[string]int{
		CountRedactions:      r.report.redactions,
		CountEmailsHashed:    r.report.emails,
		CountBodiesTruncated: r.report.bodyTruncated,
		CountIndexed:         r.indexed.Indexed,
		CountIndexRemoved:    r.indexed.Removed,
	}
}

// mergeLegacy copies a snapshot connector's status, counts and error.
func (r *ingestRun) mergeLegacy(run *domain.ConnectorRun) {
	reporter, ok := r.source.(legacyRunReporter)
	if !ok {
		return
	}
	legacy, ok := reporter.LastRun()
	if !ok {
		return
	}
	if legacy.Status.Valid() && legacy.Status != domain.ConnectorRunning && run.Status != domain.ConnectorFailed {
		run.Status = legacy.Status
	}
	for name, value := range legacy.Counts {
		run.Counts[name] = value
	}
	if run.Error == "" && legacy.Error != "" {
		run.Error, _ = RedactIngest(legacy.Error)
	}
}

func (r *ingestRun) runID() string {
	if r.host.NewRunID != nil {
		return r.host.NewRunID(r.connection.ID, r.started)
	}
	return fmt.Sprintf("run-%s-%d", r.connection.ID, r.started.UnixNano())
}

func validateCheckpoints(checkpoints []domain.Checkpoint) error {
	for _, checkpoint := range checkpoints {
		if err := checkpoint.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// lastPerRef keeps the last entry of each repeated Ref, in source order.
func lastPerRef(items []domain.Item) []domain.Item {
	last := make(map[domain.ItemRef]int, len(items))
	for index, item := range items {
		last[item.Ref] = index
	}
	if len(last) == len(items) {
		return items
	}
	out := make([]domain.Item, 0, len(last))
	for index, item := range items {
		if last[item.Ref] == index {
			out = append(out, item)
		}
	}
	return out
}

func cloneCheckpoints(checkpoints map[string]domain.Checkpoint) map[string]domain.Checkpoint {
	out := make(map[string]domain.Checkpoint, len(checkpoints))
	for stream, checkpoint := range checkpoints {
		out[stream] = checkpoint.Clone()
	}
	return out
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
