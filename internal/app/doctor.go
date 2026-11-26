package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/gitinfo"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/version"
	"github.com/levapveeskela/lv24-brain/internal/winproc"
)

const (
	doctorOK   = "ok"
	doctorWarn = "warn"
	doctorFail = "fail"
)

// Doctor checks the configuration, data directory, schema, index and git
// availability for the selected mode. It reports problems instead of failing,
// so a broken instance can still be diagnosed.
func (a *App) Doctor(ctx context.Context, opts DoctorOptions) (DoctorReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return DoctorReport{}, err
	}
	checks := make([]DoctorCheck, 0, 6)
	checks = append(checks, a.doctorConfig(opts.ConfigPath))
	checks = append(checks, a.doctorLimits())
	if a.cfg.Mode == config.ModeServer {
		checks = append(checks, a.doctorServer(ctx))
	}
	checks = append(checks, a.doctorDataDir())
	checks = append(checks, a.doctorLocks(ctx))
	checks = append(checks, a.doctorSchema())
	checks = append(checks, a.doctorIndex(ctx))
	checks = append(checks, a.doctorIndexGeneration(ctx))
	// One problem scan feeds both checks: the generic record view and the
	// idea-kind count read the same snapshot.
	problems := a.collectProblemRecords(ctx)
	checks = append(checks, a.doctorRecordsCheck(problems))
	checks = append(checks, a.doctorIdeas(ctx, problems))
	checks = append(checks, a.doctorRecordLimits(ctx))
	checks = append(checks, a.doctorJournal(ctx))
	checks = append(checks, a.doctorIDs(ctx))
	checks = append(checks, a.doctorQuarantine(ctx))
	checks = append(checks, a.doctorValidity(ctx))
	checks = append(checks, a.doctorDuplicates(ctx))
	checks = append(checks, a.doctorReviews(ctx))
	checks = append(checks, a.doctorGit(ctx))
	checks = append(checks, a.doctorMergeDrivers(ctx, opts.Fix))
	checks = append(checks, a.doctorRawZone(ctx)...)
	checks = append(checks, a.doctorLegacyDumpZones(ctx))
	checks = append(checks, a.doctorTrackedPII(ctx))
	checks = append(checks, a.doctorPrompts(ctx))
	checks = append(checks, a.doctorRetiredPaths(ctx))
	checks = append(checks, a.doctorPendingConflicts(ctx))
	checks = append(checks, a.doctorDuplicateRepo(ctx))
	return DoctorReport{Checks: checks}, nil
}

func (a *App) doctorConfig(configPath string) DoctorCheck {
	message := "configuration is valid"
	if configPath != "" {
		info, err := os.Stat(configPath)
		if err != nil {
			if os.IsNotExist(err) {
				return DoctorCheck{Name: "config", Status: doctorFail, Message: "not found: " + configPath}
			}
			return DoctorCheck{Name: "config", Status: doctorFail, Message: err.Error()}
		}
		if info.IsDir() {
			return DoctorCheck{Name: "config", Status: doctorFail, Message: "is a directory: " + configPath}
		}
		message = "configuration is valid: " + configPath
	}
	if err := a.cfg.Validate(); err != nil {
		return DoctorCheck{Name: "config", Status: doctorFail, Message: err.Error()}
	}
	return DoctorCheck{Name: "config", Status: doctorOK, Message: message}
}

func (a *App) doctorDataDir() DoctorCheck {
	switch a.cfg.Mode {
	case config.ModeServer:
		return DoctorCheck{Name: "data_dir", Status: doctorOK, Message: "not used in server mode"}
	case config.ModeLocal, config.ModeGit:
	default:
		return DoctorCheck{Name: "data_dir", Status: doctorFail, Message: "unknown mode " + a.cfg.Mode}
	}
	dataDir := strings.TrimSpace(a.cfg.DataDir)
	if dataDir == "" {
		return DoctorCheck{Name: "data_dir", Status: doctorFail, Message: "config.data_dir is empty"}
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return DoctorCheck{Name: "data_dir", Status: doctorFail, Message: "does not exist: " + dataDir}
		}
		return DoctorCheck{Name: "data_dir", Status: doctorFail, Message: err.Error()}
	}
	if !info.IsDir() {
		return DoctorCheck{Name: "data_dir", Status: doctorFail, Message: "is not a directory: " + dataDir}
	}
	if projectsInfo, projectsErr := os.Stat(filepath.Join(dataDir, "projects")); projectsErr != nil || !projectsInfo.IsDir() {
		return DoctorCheck{Name: "data_dir", Status: doctorWarn, Message: "projects/ is missing; run brain init"}
	}
	return DoctorCheck{Name: "data_dir", Status: doctorOK, Message: "exists: " + dataDir}
}

func (a *App) doctorSchema() DoctorCheck {
	if minimum := strings.TrimSpace(a.cfg.MinEngineVersion); minimum != "" && compareVersions(version.Version, minimum) < 0 {
		return DoctorCheck{
			Name:    "schema",
			Status:  doctorFail,
			Message: fmt.Sprintf("engine %s is older than the data requirement %s", version.Version, minimum),
		}
	}
	switch {
	case a.cfg.SchemaVersion > config.CurrentSchemaVersion:
		return DoctorCheck{
			Name:   "schema",
			Status: doctorFail,
			Message: fmt.Sprintf("data schema %d requires a newer engine (this engine supports %d)",
				a.cfg.SchemaVersion, config.CurrentSchemaVersion),
		}
	case a.cfg.SchemaVersion < config.CurrentSchemaVersion:
		return DoctorCheck{
			Name:   "schema",
			Status: doctorWarn,
			Message: fmt.Sprintf("data schema %d is older than engine schema %d",
				a.cfg.SchemaVersion, config.CurrentSchemaVersion),
		}
	default:
		return DoctorCheck{Name: "schema", Status: doctorOK, Message: fmt.Sprintf("schema %d", a.cfg.SchemaVersion)}
	}
}

// doctorIDs reports record IDs used by more than one record. Lookups by ID
// (show, status, trash) and journal lines cannot tell such records apart.
func (a *App) doctorIDs(ctx context.Context) DoctorCheck {
	if a.deps.Store == nil || a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "ids", Status: doctorOK, Message: "not checked in this mode"}
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return DoctorCheck{Name: "ids", Status: doctorFail, Message: err.Error()}
	}
	seen := map[string]string{}
	var duplicates []string
	for _, project := range projects {
		records, err := a.projectRecords(ctx, project.ID)
		if err != nil {
			return DoctorCheck{Name: "ids", Status: doctorFail, Message: project.ID + ": " + err.Error()}
		}
		for _, record := range records {
			id := record.Summary().ID
			if first, ok := seen[id]; ok {
				duplicates = append(duplicates, fmt.Sprintf("%s (%s, %s)", id, first, project.ID))
				continue
			}
			seen[id] = project.ID
		}
	}
	if len(duplicates) == 0 {
		return DoctorCheck{Name: "ids", Status: doctorOK, Message: fmt.Sprintf("%d record ids are unique", len(seen))}
	}
	return DoctorCheck{Name: "ids", Status: doctorWarn, Code: "duplicate-id", Message: fmt.Sprintf("%d duplicate record ids: %s", len(duplicates), strings.Join(duplicates[:min(len(duplicates), maxListedProblems)], "; "))}
}

// doctorQuarantine reports unreviewed records. They are excluded from
// context, so a growing queue is a sign that agent or connector text is
// waiting for review. Report only: reviewing is an explicit owner action.
func (a *App) doctorQuarantine(ctx context.Context) DoctorCheck {
	if a.deps.Store == nil || a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "quarantine", Status: doctorOK, Message: "not checked in this mode"}
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return DoctorCheck{Name: "quarantine", Status: doctorFail, Message: err.Error()}
	}
	total := 0
	ids := make([]string, 0)
	for _, project := range projects {
		records, err := a.projectRecords(ctx, project.ID)
		if err != nil {
			return DoctorCheck{Name: "quarantine", Status: doctorFail, Message: project.ID + ": " + err.Error()}
		}
		for _, record := range records {
			if !record.Quarantined() {
				continue
			}
			total++
			if len(ids) < maxListedProblems {
				ids = append(ids, record.Summary().ID)
			}
		}
	}
	if total == 0 {
		return DoctorCheck{Name: "quarantine", Status: doctorOK, Message: "no quarantined records"}
	}
	more := ""
	if total > len(ids) {
		more = fmt.Sprintf(", … %d more", total-len(ids))
	}
	return DoctorCheck{
		Name:   "quarantine",
		Status: doctorWarn,
		Code:   "quarantined",
		Message: fmt.Sprintf("%d quarantined records are excluded from context (unreviewed or unknown origin): %s%s; inspect with `brain review` and clear with `brain record review --all` (an unknown origin needs a hand edit)",
			total, strings.Join(ids, ", "), more),
	}
}

// doctorValidity reports validity values that cannot be true (valid_to at or
// before valid_from, a superseded record without an end) and supersede links
// that name a record the project does not have. Report only: a supersede is an
// explicit owner action.
func (a *App) doctorValidity(ctx context.Context) DoctorCheck {
	if a.deps.Store == nil || a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "validity", Status: doctorOK, Message: "not checked in this mode"}
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return DoctorCheck{Name: "validity", Status: doctorFail, Message: err.Error()}
	}
	checked := 0
	var problems []string
	for _, project := range projects {
		records, err := a.projectRecords(ctx, project.ID)
		if err != nil {
			return DoctorCheck{Name: "validity", Status: doctorFail, Message: project.ID + ": " + err.Error()}
		}
		ids := make(map[string]bool, len(records))
		for _, record := range records {
			ids[record.Summary().ID] = true
		}
		for _, record := range records {
			summary := record.Summary()
			checked++
			validity := record.Validity()
			if err := validity.Validate("validity", summary.CreatedAt, summary.ID); err != nil {
				problems = append(problems, project.ID+"/"+summary.ID+": "+err.Error())
				continue
			}
			if validity.SupersededBy != "" && validity.ValidTo.IsZero() {
				problems = append(problems, project.ID+"/"+summary.ID+": superseded_by is set but valid_to is empty")
			}
			for _, link := range []struct{ field, id string }{
				{"superseded_by", validity.SupersededBy},
				{"supersedes", validity.Supersedes},
			} {
				if link.id != "" && !ids[link.id] {
					problems = append(problems, project.ID+"/"+summary.ID+": "+link.field+" names missing record "+link.id)
				}
			}
		}
	}
	if len(problems) == 0 {
		return DoctorCheck{Name: "validity", Status: doctorOK, Message: fmt.Sprintf("%d records have consistent validity", checked)}
	}
	listed := problems[:min(len(problems), maxListedProblems)]
	return DoctorCheck{
		Name:   "validity",
		Status: doctorWarn,
		Code:   "invalid-validity",
		Message: fmt.Sprintf("%d records have invalid validity or a broken supersede link: %s; fix the frontmatter or relink with `brain record supersede`",
			len(problems), strings.Join(listed, "; ")),
	}
}

// doctorLocks reports lock files no writer will respect: their owner is dead
// past the TTL, they came from another host, or a previous boot wrote them.
// Locks are machine-local and never published, so a reported file is a
// leftover from a pull or a crash, not a live writer.
func (a *App) doctorLocks(ctx context.Context) DoctorCheck {
	if a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "locks", Status: doctorOK, Message: "not used in server mode"}
	}
	inspector, ok := a.deps.Store.(ports.LockInspector)
	if !ok {
		return DoctorCheck{Name: "locks", Status: doctorOK, Message: "not checked for this store"}
	}
	stale, err := inspector.StaleLocks(ctx)
	if err != nil {
		return DoctorCheck{Name: "locks", Status: doctorFail, Message: err.Error()}
	}
	if len(stale) == 0 {
		return DoctorCheck{Name: "locks", Status: doctorOK, Message: "no stale lock files"}
	}
	listed := make([]string, 0, maxListedProblems)
	for _, path := range stale[:min(len(stale), maxListedProblems)] {
		listed = append(listed, a.displayPath(path))
	}
	return DoctorCheck{
		Name:   "locks",
		Status: doctorWarn,
		Code:   "stale-lock",
		Message: fmt.Sprintf("%d stale lock files (dead owner past the TTL, another host or boot): %s; a writer removes one itself before waiting, so delete it only if writes keep timing out",
			len(stale), strings.Join(listed, "; ")),
	}
}

// displayPath makes a data-directory path readable in a check message.
func (a *App) displayPath(path string) string {
	if dataDir := strings.TrimSpace(a.cfg.DataDir); dataDir != "" {
		if rel, err := filepath.Rel(dataDir, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return path
}

// doctorJournal reports recent records that no journal mentions.
func (a *App) doctorJournal(ctx context.Context) DoctorCheck {
	if a.deps.Journal == nil || a.deps.Store == nil {
		return DoctorCheck{Name: "journal", Status: doctorOK, Message: "not used in this mode"}
	}
	gaps, err := a.JournalGaps(ctx, a.now().Add(-DefaultJournalRepairWindow))
	if err != nil {
		return DoctorCheck{Name: "journal", Status: doctorFail, Message: err.Error()}
	}
	if len(gaps) == 0 {
		return DoctorCheck{Name: "journal", Status: doctorOK, Message: "every record of the last 30 days is journaled"}
	}
	return DoctorCheck{
		Name:    "journal",
		Status:  doctorWarn,
		Message: fmt.Sprintf("%d records of the last 30 days have no journal line (first: %s %s); run brain journal --repair", len(gaps), gaps[0].Event, gaps[0].ID),
	}
}

// maxListedProblems bounds the paths named in the records check.
const maxListedProblems = 3

// doctorLimits reports the effective write limits. A limit below the
// compatibility floor is valid but rejects ordinary writes, so it is a
// warning: the operator asked for something that will block day-to-day work.
func (a *App) doctorLimits() DoctorCheck {
	limits := a.cfg.EffectiveLimits()
	message := fmt.Sprintf("body %d bytes, title %d chars, metadata %d bytes, %d protected projects, %d protected globs",
		limits.MaxBodyBytes, limits.MaxTitleChars, limits.MaxMetadataBytes,
		len(limits.ProtectedProjects), len(limits.ProtectedGlobs))
	var small []string
	if limits.MaxBodyBytes < limitsWarnBodyBytes {
		small = append(small, fmt.Sprintf("max_body_bytes %d < %d", limits.MaxBodyBytes, limitsWarnBodyBytes))
	}
	if limits.MaxTitleChars < limitsWarnTitleChars {
		small = append(small, fmt.Sprintf("max_title_chars %d < %d", limits.MaxTitleChars, limitsWarnTitleChars))
	}
	if limits.MaxMetadataBytes < limitsWarnMetadataBytes {
		small = append(small, fmt.Sprintf("max_metadata_bytes %d < %d", limits.MaxMetadataBytes, limitsWarnMetadataBytes))
	}
	if len(small) > 0 {
		return DoctorCheck{
			Name:    "limits",
			Status:  doctorWarn,
			Code:    "limits-small",
			Message: message + "; very small limits reject ordinary writes: " + strings.Join(small, ", "),
		}
	}
	return DoctorCheck{Name: "limits", Status: doctorOK, Message: message}
}

// Compatibility floors of doctor's limits check: below them a limit is valid
// but likely to reject a normal note or decision.
const (
	limitsWarnBodyBytes     = 1024
	limitsWarnTitleChars    = 16
	limitsWarnMetadataBytes = 128
)

// doctorRecordLimits reports legacy records that exceed the configured limits.
// It is report-only: doctor never rewrites a record body, and --fix leaves
// these findings untouched. The owner either shrinks the record by hand or
// raises the limit in brain.yaml.
func (a *App) doctorRecordLimits(ctx context.Context) DoctorCheck {
	if a.deps.Store == nil || a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "record-limits", Status: doctorOK, Message: "not checked in this mode"}
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return DoctorCheck{Name: "record-limits", Status: doctorFail, Message: err.Error()}
	}
	limits := a.cfg.EffectiveLimits()
	checked, oversized := 0, 0
	listed := make([]string, 0, maxListedProblems)
	for _, project := range projects {
		records, err := a.projectRecords(ctx, project.ID)
		if err != nil {
			return DoctorCheck{Name: "record-limits", Status: doctorFail, Message: project.ID + ": " + err.Error()}
		}
		for _, record := range records {
			checked++
			violations := limitViolations(limits, recordWriteContent(record))
			if len(violations) == 0 {
				continue
			}
			oversized++
			if len(listed) < maxListedProblems {
				details := make([]string, 0, len(violations))
				for _, violation := range violations {
					details = append(details, violation.Error())
				}
				listed = append(listed, project.ID+"/"+record.Summary().ID+": "+strings.Join(details, "; "))
			}
		}
	}
	if oversized == 0 {
		return DoctorCheck{Name: "record-limits", Status: doctorOK, Message: fmt.Sprintf("%d records fit the configured limits", checked)}
	}
	more := ""
	if oversized > len(listed) {
		more = fmt.Sprintf(", … %d more", oversized-len(listed))
	}
	return DoctorCheck{
		Name:   "record-limits",
		Status: doctorWarn,
		Code:   "record-limits",
		Message: fmt.Sprintf("%d of %d records exceed the configured limits (legacy data, report only; doctor rewrites nothing): %s%s; shrink the record by hand or raise the limit in brain.yaml",
			oversized, checked, strings.Join(listed, "; "), more),
	}
}

// problemSnapshot is the one record-file problem scan doctor shares between
// the generic records check and the idea-kind view. available is false when the
// store has no problem reporter; err names the project whose scan failed.
type problemSnapshot struct {
	available bool
	projects  int
	problems  []ports.RecordProblem
	err       error
}

// collectProblemRecords scans every registered project once. The store's
// problem reporter is project-scoped, so orphan project directories are not
// part of the snapshot; their idea files stay visible through `brain idea list`
// and the idea reindex.
func (a *App) collectProblemRecords(ctx context.Context) problemSnapshot {
	reporter, ok := a.deps.Store.(ports.ProblemReporter)
	if !ok {
		return problemSnapshot{}
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return problemSnapshot{available: true, err: err}
	}
	snapshot := problemSnapshot{available: true, projects: len(projects)}
	for _, project := range projects {
		found, err := reporter.Problems(ctx, project.ID)
		if err != nil {
			snapshot.err = fmt.Errorf("%s: %w", project.ID, err)
			return snapshot
		}
		snapshot.problems = append(snapshot.problems, found...)
	}
	return snapshot
}

// doctorRecordsCheck reports record files that listing skips (damaged, empty or
// conflicted), which would otherwise be invisible to every command.
func (a *App) doctorRecordsCheck(snapshot problemSnapshot) DoctorCheck {
	if !snapshot.available {
		return DoctorCheck{Name: "records", Status: doctorOK, Message: "not checked for this store"}
	}
	if snapshot.err != nil {
		return DoctorCheck{Name: "records", Status: doctorFail, Message: snapshot.err.Error()}
	}
	if len(snapshot.problems) == 0 {
		return DoctorCheck{Name: "records", Status: doctorOK, Message: fmt.Sprintf("all record files readable (%d projects)", snapshot.projects)}
	}
	listed := make([]string, 0, maxListedProblems)
	for _, problem := range snapshot.problems[:min(len(snapshot.problems), maxListedProblems)] {
		listed = append(listed, problem.Path+" ("+problem.Reason+")")
	}
	return DoctorCheck{
		Name:    "records",
		Status:  doctorWarn,
		Message: fmt.Sprintf("%d record files are skipped by listing: %s", len(snapshot.problems), strings.Join(listed, "; ")),
	}
}

// doctorIdeas reports the idea layer: the live count, how many are open and
// the idea files the store skipped as problem rows. Ideas are optional and
// report-only here; doctor never moves, rewrites or deletes one.
func (a *App) doctorIdeas(ctx context.Context, snapshot problemSnapshot) DoctorCheck {
	const name = "ideas"
	if a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: name, Status: doctorOK, Message: "ideas are not supported in server mode"}
	}
	store, ok := a.deps.Store.(ports.IdeaStore)
	if !ok {
		return DoctorCheck{Name: name, Status: doctorOK, Message: "not checked for this store"}
	}
	if snapshot.available && snapshot.err != nil {
		return DoctorCheck{Name: name, Status: doctorFail, Message: snapshot.err.Error()}
	}
	ideas, err := store.ListIdeas(ctx, ports.IdeaFilter{})
	if err != nil {
		return DoctorCheck{Name: name, Status: doctorFail, Message: err.Error()}
	}
	open := 0
	for _, idea := range ideas {
		if !idea.Status.Terminal() {
			open++
		}
	}
	var problems []ports.RecordProblem
	if snapshot.available {
		for _, problem := range snapshot.problems {
			if problem.Kind == ports.RecordIdea {
				problems = append(problems, problem)
			}
		}
	}
	summary := "kind idea: no ideas"
	if len(ideas) > 0 {
		summary = fmt.Sprintf("kind idea: %s (%d open)", countLabel(len(ideas), "idea", "ideas"), open)
	}
	if len(problems) == 0 {
		return DoctorCheck{Name: name, Status: doctorOK, Message: summary}
	}
	listed := make([]string, 0, maxListedProblems)
	for _, problem := range problems[:min(len(problems), maxListedProblems)] {
		listed = append(listed, a.displayPath(problem.Path)+" ("+problem.Reason+")")
	}
	suffix := countLabel(len(problems), "problem file is", "problem files are") + " skipped and not indexed"
	return DoctorCheck{
		Name:   name,
		Status: doctorWarn,
		Code:   "idea-problems",
		Message: fmt.Sprintf("%s, %s: %s; resolve them in git (nothing is deleted or rewritten)",
			summary, suffix, strings.Join(listed, "; ")),
	}
}

// countLabel renders a count with a matching noun: "1 idea", "2 ideas".
func countLabel(count int, singular, plural string) string {
	if count == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", count, plural)
}

func (a *App) doctorIndex(ctx context.Context) DoctorCheck {
	if a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "index", Status: doctorOK, Message: "server mode uses the remote database"}
	}
	if a.deps.Index == nil {
		// The index is a local cache, not required data: a missing one only
		// disables search until `brain index` rebuilds it.
		return DoctorCheck{Name: "index", Status: doctorWarn, Message: "local search index is not available; run brain index (search is disabled until then)"}
	}
	if _, err := a.deps.Index.Search(ctx, ports.Query{Text: "brain-doctor", Limit: 1}); err != nil {
		return DoctorCheck{Name: "index", Status: doctorFail, Message: err.Error()}
	}
	message := "SQLite FTS5 is usable"
	if reader, ok := a.deps.Index.(indexMetaReader); ok {
		if meta, err := reader.Meta(ctx); err == nil {
			if version := strings.TrimSpace(meta[indexMetaSchemaVersion]); version != "" {
				message += " (schema " + version + ")"
			}
		}
	}
	if a.indexDirty() {
		return DoctorCheck{Name: "index", Status: doctorWarn, Message: message + "; the index may miss records (a write or sync could not update it); run brain index"}
	}
	return DoctorCheck{Name: "index", Status: doctorOK, Message: message}
}

// doctorIndexGeneration reports an index that lags the data directory. It is
// report-only: one commit without a reindex is normal (the next write or sync
// catches up), so only a larger gap or a dirty tree warns, and the remedy is
// always the explicit `brain index`.
func (a *App) doctorIndexGeneration(ctx context.Context) DoctorCheck {
	const name = "index-generation"
	if a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: name, Status: doctorOK, Message: "the index is local-only in server mode"}
	}
	status, err := a.IndexStatus(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: doctorOK, Message: "no local index to compare"}
	}
	if !status.Stale {
		head := status.Head
		if head == "" {
			head = "(no git marker)"
		}
		return DoctorCheck{Name: name, Status: doctorOK, Message: "index matches HEAD " + head}
	}
	if status.Generation == "" {
		return DoctorCheck{Name: name, Status: doctorWarn, Message: "the index has no generation yet (never indexed or just rebuilt); run brain index"}
	}
	if status.IndexedFrom == "" {
		return DoctorCheck{Name: name, Status: doctorWarn, Message: "the index has no indexed_from marker; run brain index"}
	}
	behind, comparable := indexCommitDistance(ctx, a.cfg.DataDir, status.IndexedFrom)
	if !comparable {
		return DoctorCheck{
			Name:    name,
			Status:  doctorWarn,
			Message: fmt.Sprintf("the index was built from %s, which this clone cannot compare with HEAD %s; run brain index", status.IndexedFrom, valueOr(status.Head, "(unknown)")),
		}
	}
	if status.Dirty || behind > 1 {
		state := fmt.Sprintf("%d commits behind", behind)
		if status.Dirty {
			state += ", data directory dirty"
		}
		return DoctorCheck{
			Name:    name,
			Status:  doctorWarn,
			Message: fmt.Sprintf("the index is stale at %s (HEAD %s, %s); run brain index", status.IndexedFrom, valueOr(status.Head, "(unknown)"), state),
		}
	}
	return DoctorCheck{Name: name, Status: doctorOK, Message: "the index is one commit behind HEAD; the next write or brain index catches up"}
}

// indexCommitDistance counts the commits between an indexed_from marker and
// HEAD, capped at two: the check only needs to know whether the gap is more
// than one commit, so it never walks the whole history.
func indexCommitDistance(ctx context.Context, dir, marker string) (int, bool) {
	dir = strings.TrimSpace(dir)
	marker = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(marker), gitDirtySuffix))
	if dir == "" || marker == "" {
		return 0, false
	}
	runCtx, cancel := context.WithTimeout(ctx, gitinfo.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "git", "-C", dir, "rev-list", "--count", "--max-count=2", marker+"..HEAD")
	winproc.Hide(cmd)
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	distance, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, false
	}
	return distance, true
}

func (a *App) doctorServer(ctx context.Context) DoctorCheck {
	if strings.TrimSpace(a.cfg.ServerURL) == "" {
		return DoctorCheck{Name: "server", Status: doctorFail, Message: "config.server_url is empty"}
	}
	if a.deps.Store == nil {
		return DoctorCheck{
			Name:    "server",
			Status:  doctorFail,
			Message: "server client is not configured; set BRAIN_TOKEN or run brain secret set server-token",
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	revision, err := a.deps.Store.CurrentRevision(checkCtx)
	if err != nil {
		return DoctorCheck{Name: "server", Status: doctorFail, Message: "server API: " + err.Error()}
	}
	return DoctorCheck{Name: "server", Status: doctorOK, Message: "API reachable (revision " + revision.Value + ")"}
}

func (a *App) doctorGit(ctx context.Context) DoctorCheck {
	switch a.cfg.Mode {
	case config.ModeLocal:
		return DoctorCheck{Name: "git", Status: doctorOK, Message: "not required in local mode"}
	case config.ModeServer:
		return DoctorCheck{Name: "git", Status: doctorOK, Message: "not used in server mode"}
	case config.ModeGit:
	default:
		return DoctorCheck{Name: "git", Status: doctorFail, Message: "unknown mode " + a.cfg.Mode}
	}
	if a.deps.Sync == nil {
		return DoctorCheck{Name: "git", Status: doctorFail, Message: "git sync provider is not configured"}
	}
	status, err := a.deps.Sync.Status(ctx)
	if err != nil {
		return DoctorCheck{Name: "git", Status: doctorFail, Message: err.Error()}
	}
	message := "git is available"
	if status.Branch != "" {
		message = fmt.Sprintf("branch %s, ahead %d, behind %d", status.Branch, status.Ahead, status.Behind)
	}
	return DoctorCheck{Name: "git", Status: doctorOK, Message: message}
}

// doctorMergeDrivers checks that the deterministic merge drivers named in
// .gitattributes are registered in this clone. Git silently falls back to a
// normal content conflict when a driver is missing, so an unregistered clone
// still works but conflicts on every state or registry change. --fix registers
// them; it changes only .git/config.
func (a *App) doctorMergeDrivers(ctx context.Context, fix bool) DoctorCheck {
	if a.cfg.Mode != config.ModeGit {
		return DoctorCheck{Name: "merge-drivers", Status: doctorOK, Message: "not used in this mode"}
	}
	if strings.TrimSpace(a.cfg.DataDir) == "" {
		return DoctorCheck{Name: "merge-drivers", Status: doctorFail, Message: "config.data_dir is empty"}
	}
	if fix {
		if err := ensureGitattributes(a.cfg.DataDir); err != nil {
			return DoctorCheck{Name: "merge-drivers", Status: doctorFail, Message: err.Error()}
		}
		// sync.lock and .tmp-* files must never be staged; an older data
		// repository may predate the managed ignore block.
		if err := ensureGitignore(a.cfg.DataDir); err != nil {
			return DoctorCheck{Name: "merge-drivers", Status: doctorFail, Message: err.Error()}
		}
	}
	missingAttributes, err := missingGitattributesLines(a.cfg.DataDir)
	if err != nil {
		return DoctorCheck{Name: "merge-drivers", Status: doctorFail, Message: err.Error()}
	}
	if len(missingAttributes) > 0 && !fix {
		return DoctorCheck{
			Name:   "merge-drivers",
			Status: doctorWarn,
			Code:   "merge-drivers-attributes-missing",
			Message: ".gitattributes misses " + strings.Join(missingAttributes, ", ") +
				"; run brain doctor --fix",
		}
	}

	installer := a.deps.MergeDrivers
	if installer == nil {
		return DoctorCheck{Name: "merge-drivers", Status: doctorWarn, Message: "git merge driver registration is not available"}
	}
	missing, err := installer.MissingMergeDrivers(ctx)
	if errors.Is(err, domain.ErrUnsupported) {
		return DoctorCheck{Name: "merge-drivers", Status: doctorOK, Message: "data directory is not a git repository yet"}
	}
	if err != nil {
		return DoctorCheck{Name: "merge-drivers", Status: doctorFail, Message: err.Error()}
	}
	if len(missing) == 0 {
		return DoctorCheck{Name: "merge-drivers", Status: doctorOK, Message: "state, notes, registry, and items drivers are registered"}
	}
	if fix {
		executable, execErr := os.Executable()
		if execErr != nil {
			return DoctorCheck{Name: "merge-drivers", Status: doctorFail, Message: "resolve the brain executable: " + execErr.Error()}
		}
		if _, err := installer.RegisterMergeDrivers(ctx, executable); err != nil {
			return DoctorCheck{Name: "merge-drivers", Status: doctorFail, Message: err.Error()}
		}
		return DoctorCheck{
			Name:    "merge-drivers",
			Status:  doctorOK,
			Message: "registered " + strings.Join(missing, ", "),
		}
	}
	return DoctorCheck{
		Name:    "merge-drivers",
		Status:  doctorWarn,
		Code:    "merge-drivers-missing",
		Message: "missing " + strings.Join(missing, ", ") + "; run brain doctor --fix",
	}
}

// doctorRawZone reports raw-zone files in the shared tree. Three states matter:
//
//   - an explicit local-only connection with tracked files: they must be
//     untracked (the owner's decision, `git rm --cached -r`; files and history
//     are kept), as must checkpoints of an explicitly shared connection;
//   - a connection without an explicit publish policy whose raw zone git
//     tracks: it keeps publishing (grandfathered), so doctor recommends an
//     explicit `publish` value instead of changing anything;
//   - a new local-only connection: nothing is tracked and there is no finding.
//
// It reports only; nothing is untracked or deleted here.
func (a *App) doctorRawZone(ctx context.Context) []DoctorCheck {
	if a.cfg.Mode != config.ModeGit || a.deps.Sync == nil {
		return []DoctorCheck{{Name: "raw-zone", Status: doctorOK, Message: "not used in this mode"}}
	}
	lister, ok := a.deps.Sync.(ports.GitPathLister)
	if !ok {
		return []DoctorCheck{{Name: "raw-zone", Status: doctorOK, Message: "not checked for this sync provider"}}
	}
	paths, err := lister.TrackedPaths(ctx, "connectors")
	if err != nil {
		return []DoctorCheck{{Name: "raw-zone", Status: doctorWarn, Message: "cannot list tracked raw-zone files: " + err.Error()}}
	}
	audit, err := a.auditRawZone(ctx, paths)
	if err != nil {
		return []DoctorCheck{{Name: "raw-zone", Status: doctorWarn, Message: err.Error()}}
	}
	var leftovers, grandfathered []rawZoneAudit
	for _, entry := range audit {
		switch {
		case entry.policy == "":
			grandfathered = append(grandfathered, entry)
		case entry.policy == domain.PublishShared && entry.checkpoints == 0:
			// Explicitly shared items are the documented state.
		default:
			leftovers = append(leftovers, entry)
		}
	}
	return []DoctorCheck{rawZoneLeftoversCheck(leftovers), rawZoneGrandfatheredCheck(grandfathered)}
}

// rawZoneAudit files of one connection that git tracks, with the explicit
// publish policy from the catalogue (empty when unset or unknown).
type rawZoneAudit struct {
	connection  string
	files       int
	checkpoints int
	policy      domain.PublishPolicy
}

// auditRawZone groups tracked raw-zone paths by connection and resolves the
// explicit publish setting of each.
func (a *App) auditRawZone(ctx context.Context, paths []string) ([]rawZoneAudit, error) {
	entries := make([]rawZoneAudit, 0, len(paths))
	index := map[string]int{}
	for _, path := range paths {
		connectionID, kind, ok := rawZonePath(path)
		if !ok {
			continue
		}
		position, seen := index[connectionID]
		if !seen {
			policy, err := a.explicitPublishPolicy(ctx, connectionID)
			if err != nil {
				return nil, err
			}
			position = len(entries)
			index[connectionID] = position
			entries = append(entries, rawZoneAudit{connection: connectionID, policy: policy})
		}
		if kind == "items" {
			entries[position].files++
		} else {
			entries[position].files++
			entries[position].checkpoints++
		}
	}
	return entries, nil
}

// explicitPublishPolicy returns the publish setting the catalogue records for a
// connection. An unknown connection has none, so its zone counts as unset.
func (a *App) explicitPublishPolicy(ctx context.Context, connectionID string) (domain.PublishPolicy, error) {
	if a.deps.Connections == nil {
		return "", nil
	}
	connection, err := a.deps.Connections.Get(ctx, connectionID)
	if errors.Is(err, domain.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read connection %s: %w", connectionID, err)
	}
	if !connection.Publish.Valid() {
		return "", nil
	}
	return connection.Publish, nil
}

func rawZoneLeftoversCheck(entries []rawZoneAudit) DoctorCheck {
	if len(entries) == 0 {
		return DoctorCheck{Name: "raw-zone", Status: doctorOK, Message: "no tracked raw files of local-only connections"}
	}
	return DoctorCheck{
		Name:   "raw-zone",
		Status: doctorWarn,
		Code:   "raw-zone-tracked",
		Message: fmt.Sprintf("raw items or checkpoints are tracked by git although the connection does not publish them (%s; explicit local-only, or checkpoints of a shared connection): untrack them with git rm --cached -r connectors/<id>; the files and the git history are kept",
			rawZoneConnectionList(entries)),
	}
}

func rawZoneGrandfatheredCheck(entries []rawZoneAudit) DoctorCheck {
	if len(entries) == 0 {
		return DoctorCheck{Name: "raw-zone-policy", Status: doctorOK, Message: "every published raw zone has an explicit publish policy"}
	}
	return DoctorCheck{
		Name:   "raw-zone-policy",
		Status: doctorWarn,
		Code:   "raw-zone-grandfathered",
		Message: fmt.Sprintf("the raw zone is tracked by git but no publish policy is set (%s): it keeps publishing unchanged (compatibility mode); choose with brain connection publish <id> shared|local-only",
			rawZoneConnectionList(entries)),
	}
}

// rawZoneConnectionList renders "<id> (<n> files)" pairs in tracking order.
func rawZoneConnectionList(entries []rawZoneAudit) string {
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		parts = append(parts, fmt.Sprintf("%s (%d files)", entry.connection, entry.files))
	}
	return strings.Join(parts, ", ")
}

// rawZonePath reports whether path is connectors/<id>/items/... or
// connectors/<id>/checkpoints/... and returns the connection ID and the kind.
func rawZonePath(path string) (string, string, bool) {
	rest, ok := strings.CutPrefix(filepath.ToSlash(path), "connectors/")
	if !ok {
		return "", "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[2] == "" {
		return "", "", false
	}
	if parts[1] != "items" && parts[1] != "checkpoints" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// legacyDumpRoots are the pre-raw-zone connector dump directories. They are
// external, often personal data; git must keep them local-only. Each pattern
// is anchored in .gitignore so connectors/github and connectors/gitlab stay
// shared only when their connection explicitly publishes them.
var legacyDumpRoots = []string{"mail", "github", "gitlab", "merges"}

// doctorLegacyDumpZones reports tracked files under the legacy dump roots and
// the removed connection directory connectors/gitlab. They predate the
// publish policy, which governs connectors/<id> only, so a data repository
// upgraded from an older engine can still publish mail and raw dumps. It
// reports only: files and git history are never changed here.
func (a *App) doctorLegacyDumpZones(ctx context.Context) DoctorCheck {
	if a.cfg.Mode != config.ModeGit || a.deps.Sync == nil {
		return DoctorCheck{Name: "pii-zone", Status: doctorOK, Message: "not used in this mode"}
	}
	lister, ok := a.deps.Sync.(ports.GitPathLister)
	if !ok {
		return DoctorCheck{Name: "pii-zone", Status: doctorOK, Message: "not checked for this sync provider"}
	}
	counts := map[string]int{}
	for _, root := range legacyDumpRoots {
		paths, err := lister.TrackedPaths(ctx, root)
		if err != nil {
			return DoctorCheck{Name: "pii-zone", Status: doctorWarn, Message: "cannot list tracked " + root + " files: " + err.Error()}
		}
		if len(paths) > 0 {
			counts[root] = len(paths)
		}
	}
	// connectors/gitlab was a legacy dump, not a live connection. A live
	// connection with that id keeps its explicit publish policy and is handled
	// by doctorRawZone instead.
	if a.deps.Connections != nil {
		if _, err := a.deps.Connections.Get(ctx, "gitlab"); errors.Is(err, domain.ErrNotFound) {
			paths, listErr := lister.TrackedPaths(ctx, "connectors/gitlab")
			if listErr != nil {
				return DoctorCheck{Name: "pii-zone", Status: doctorWarn, Message: "cannot list tracked connectors/gitlab files: " + listErr.Error()}
			}
			if len(paths) > 0 {
				counts["connectors/gitlab"] = len(paths)
			}
		} else if err != nil {
			return DoctorCheck{Name: "pii-zone", Status: doctorWarn, Message: "cannot read connection gitlab: " + err.Error()}
		}
	}
	if len(counts) == 0 {
		return DoctorCheck{Name: "pii-zone", Status: doctorOK, Message: "no legacy PII or raw dump zones are tracked by git"}
	}
	parts := make([]string, 0, len(counts))
	for _, root := range append(append([]string{}, legacyDumpRoots...), "connectors/gitlab") {
		if count := counts[root]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s (%d files)", root, count))
		}
	}
	return DoctorCheck{
		Name:   "pii-zone",
		Status: doctorWarn,
		Code:   "tracked-pii-zone",
		Message: fmt.Sprintf("legacy PII or raw dump zones are tracked by git (%s): untrack them with git rm --cached -r -- mail github gitlab merges connectors/gitlab; the files stay on disk and git history is not rewritten",
			strings.Join(parts, ", ")),
	}
}

// trackedPIIConnectorRoots are the connectors/ subtrees that hold exported
// provider data or run records: a tracked file there can publish personal data.
var trackedPIIConnectorRoots = []string{"runs", "github", "gitlab"}

const (
	// maxTrackedPIIScanFiles bounds one doctor pass: the legacy dump zones can
	// hold tens of thousands of files and doctor must stay fast. The check
	// reports the files it inspected, never a silent all-clear.
	maxTrackedPIIScanFiles = 500
	// maxTrackedPIIFileBytes bounds the head read of one tracked file.
	maxTrackedPIIFileBytes = 1 << 20
)

// mailboxFieldPattern matches a JSON string field that names a mailbox or a
// login. The value is a likely user name even when the domain was stripped, so
// it counts as personal data in a published file.
var mailboxFieldPattern = regexp.MustCompile(`(?i)"(?:e-?mail|author_email|committer_email|user_email|mail|mailbox|login|username)"\s*:\s*"[^"]+"`)

// doctorTrackedPII reports tracked files under connectors/ that contain e-mail
// addresses or likely mailbox user names: connectors/*.json (the connection
// catalogue), connectors/runs/ and the legacy connectors/github and
// connectors/gitlab dump zones. Such files were published before the raw-zone
// policy existed and stay in the shared tree until the owner untracks them.
// Fixtures (*_test*, testdata/) are ignored. Report only: doctor never
// untracks, rewrites or deletes a file.
func (a *App) doctorTrackedPII(ctx context.Context) DoctorCheck {
	if a.cfg.Mode != config.ModeGit || a.deps.Sync == nil {
		return DoctorCheck{Name: "tracked-pii", Status: doctorOK, Message: "not used in this mode"}
	}
	lister, ok := a.deps.Sync.(ports.GitPathLister)
	if !ok {
		return DoctorCheck{Name: "tracked-pii", Status: doctorOK, Message: "not checked for this sync provider"}
	}
	paths, err := lister.TrackedPaths(ctx, "connectors")
	if err != nil {
		return DoctorCheck{Name: "tracked-pii", Status: doctorWarn, Message: "cannot list tracked connector files: " + err.Error()}
	}
	checked := 0
	offending := 0
	listed := make([]string, 0, maxListedProblems)
	for _, path := range paths {
		if !trackedPIIPath(path) {
			continue
		}
		if checked >= maxTrackedPIIScanFiles {
			break
		}
		checked++
		raw, err := a.readTrackedConnectorFile(path)
		if err != nil {
			continue
		}
		if emailPattern.Match(raw) || mailboxFieldPattern.Match(raw) {
			offending++
			if len(listed) < maxListedProblems {
				listed = append(listed, path)
			}
		}
	}
	if offending == 0 {
		return DoctorCheck{
			Name:    "tracked-pii",
			Status:  doctorOK,
			Message: fmt.Sprintf("%d tracked connector files hold no e-mail address or mailbox user name", checked),
		}
	}
	return DoctorCheck{
		Name:   "tracked-pii",
		Status: doctorWarn,
		Code:   "tracked-pii",
		Message: fmt.Sprintf("%d of %d checked tracked connector files hold e-mail addresses or mailbox user names: %s; untrack each with git rm --cached <path> (the files and the git history are kept) and keep the value in the credential store or a local-only zone; doctor changes nothing (warning until the identity fields move to the credential store)",
			offending, checked, strings.Join(listed, ", ")),
	}
}

// trackedPIIPath reports whether a tracked connector path belongs to the PII
// surface: a direct connectors/*.json file, or any file under connectors/runs,
// connectors/github or connectors/gitlab. Test fixtures are ignored.
func trackedPIIPath(path string) bool {
	rest, ok := strings.CutPrefix(filepath.ToSlash(path), "connectors/")
	if !ok || rest == "" {
		return false
	}
	segments := strings.Split(rest, "/")
	for _, segment := range segments {
		if strings.Contains(segment, "_test") || strings.EqualFold(segment, "testdata") {
			return false
		}
	}
	if len(segments) == 1 {
		return strings.HasSuffix(segments[0], ".json")
	}
	for _, root := range trackedPIIConnectorRoots {
		if segments[0] == root {
			return true
		}
	}
	return false
}

// readTrackedConnectorFile reads the head of a tracked connector file, refusing
// a path that escapes the data directory.
func (a *App) readTrackedConnectorFile(path string) ([]byte, error) {
	dataDir := strings.TrimSpace(a.cfg.DataDir)
	if dataDir == "" {
		return nil, errors.New("config.data_dir is empty")
	}
	root := filepath.Clean(dataDir)
	full := filepath.Join(root, filepath.FromSlash(path))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("tracked path %s escapes the data directory", path)
	}
	file, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, maxTrackedPIIFileBytes))
}

// doctorPendingConflicts counts the conflict artifacts the merge drivers and
// consolidation wrote: state field conflicts, registry claim conflicts and
// pending merge manifests. An applied manifest is the audit of a finished
// consolidation and is not counted. The artifacts are kept in the data
// repository on purpose — they are the audit of what each machine could not
// decide alone — so a non-zero count is a warning to reconcile them, never a
// failure.
func (a *App) doctorPendingConflicts(_ context.Context) DoctorCheck {
	if a.cfg.Mode == config.ModeServer || strings.TrimSpace(a.cfg.DataDir) == "" {
		return DoctorCheck{Name: "conflicts", Status: doctorOK, Message: "not used in this mode"}
	}
	dataDir := a.cfg.DataDir
	stateCount := len(globFiles(filepath.Join(dataDir, "projects", "*", stateConflictDir), ".md")) +
		len(globFiles(filepath.Join(dataDir, "projects", "*", "notes", notesConflictDir), ".md"))
	registryCount := len(globFiles(filepath.Join(dataDir, registryConflictDir), ".md"))
	manifestCount := len(pendingMergeManifests(dataDir))
	total := stateCount + registryCount + manifestCount
	if total == 0 {
		return DoctorCheck{Name: "conflicts", Status: doctorOK, Message: "no pending conflict artifacts"}
	}
	return DoctorCheck{
		Name:   "conflicts",
		Status: doctorWarn,
		Code:   "pending-conflicts",
		Message: fmt.Sprintf("%d conflict artifacts pending: %d state/notes (state-conflicts/), %d registry (registry-conflicts/), %d merge manifest (merges/); inspect them with `brain review` and delete each artifact once both sides are reconciled",
			total, stateCount, registryCount, manifestCount),
	}
}

// doctorDuplicateRepo reports one repository URL registered under several
// project ids. Knowledge written under each id stays split — the lv24-ytk
// variants are the live example — so doctor warns until the owner merges
// them; it never changes the registry itself.
func (a *App) doctorDuplicateRepo(ctx context.Context) DoctorCheck {
	if a.deps.Store == nil || a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "duplicate-repo", Status: doctorOK, Message: "not checked in this mode"}
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return DoctorCheck{Name: "duplicate-repo", Status: doctorFail, Message: err.Error()}
	}
	groups := map[string][]string{}
	for _, project := range projects {
		key := gitinfo.NormalizeRepoURL(project.RepoURL)
		if key == "" {
			continue
		}
		groups[key] = append(groups[key], project.ID)
	}
	var duplicates []string
	for key, ids := range groups {
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		duplicates = append(duplicates, key+" ("+strings.Join(ids, ", ")+")")
	}
	if len(duplicates) == 0 {
		return DoctorCheck{
			Name:    "duplicate-repo",
			Status:  doctorOK,
			Message: fmt.Sprintf("%d repositories are each owned by one project", len(groups)),
		}
	}
	sort.Strings(duplicates)
	return DoctorCheck{
		Name:   "duplicate-repo",
		Status: doctorWarn,
		Code:   "duplicate-repo",
		Message: fmt.Sprintf("%d repositories are registered under more than one project id: %s; pick one id and move the other project's records there (brain record trash/supersede) so later writes do not split",
			len(duplicates), strings.Join(duplicates, "; ")),
	}
}
