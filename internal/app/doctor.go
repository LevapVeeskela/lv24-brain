package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/version"
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
	if a.cfg.Mode == config.ModeServer {
		checks = append(checks, a.doctorServer(ctx))
	}
	checks = append(checks, a.doctorDataDir())
	checks = append(checks, a.doctorLocks(ctx))
	checks = append(checks, a.doctorSchema())
	checks = append(checks, a.doctorIndex(ctx))
	checks = append(checks, a.doctorRecords(ctx))
	checks = append(checks, a.doctorJournal(ctx))
	checks = append(checks, a.doctorIDs(ctx))
	checks = append(checks, a.doctorQuarantine(ctx))
	checks = append(checks, a.doctorValidity(ctx))
	checks = append(checks, a.doctorDuplicates(ctx))
	checks = append(checks, a.doctorGit(ctx))
	checks = append(checks, a.doctorMergeDrivers(ctx, opts.Fix))
	checks = append(checks, a.doctorRawZone(ctx)...)
	checks = append(checks, a.doctorRetiredPaths(ctx))
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

// doctorRecords reports record files that listing skips (damaged, empty or
// quarantined), which would otherwise be invisible to every command.
func (a *App) doctorRecords(ctx context.Context) DoctorCheck {
	reporter, ok := a.deps.Store.(ports.ProblemReporter)
	if !ok {
		return DoctorCheck{Name: "records", Status: doctorOK, Message: "not checked for this store"}
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return DoctorCheck{Name: "records", Status: doctorFail, Message: err.Error()}
	}
	var problems []ports.RecordProblem
	for _, project := range projects {
		found, err := reporter.Problems(ctx, project.ID)
		if err != nil {
			return DoctorCheck{Name: "records", Status: doctorFail, Message: project.ID + ": " + err.Error()}
		}
		problems = append(problems, found...)
	}
	if len(problems) == 0 {
		return DoctorCheck{Name: "records", Status: doctorOK, Message: fmt.Sprintf("all record files readable (%d projects)", len(projects))}
	}
	listed := make([]string, 0, maxListedProblems)
	for _, problem := range problems[:min(len(problems), maxListedProblems)] {
		listed = append(listed, problem.Path+" ("+problem.Reason+")")
	}
	return DoctorCheck{
		Name:    "records",
		Status:  doctorWarn,
		Message: fmt.Sprintf("%d record files are skipped by listing: %s", len(problems), strings.Join(listed, "; ")),
	}
}

func (a *App) doctorIndex(ctx context.Context) DoctorCheck {
	if a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "index", Status: doctorOK, Message: "server mode uses the remote database"}
	}
	if a.deps.Index == nil {
		return DoctorCheck{Name: "index", Status: doctorFail, Message: "local search index is not available"}
	}
	if _, err := a.deps.Index.Search(ctx, ports.Query{Text: "brain-doctor", Limit: 1}); err != nil {
		return DoctorCheck{Name: "index", Status: doctorFail, Message: err.Error()}
	}
	if a.indexDirty() {
		return DoctorCheck{Name: "index", Status: doctorWarn, Message: "the index may miss records (a write or sync could not update it); run brain index"}
	}
	return DoctorCheck{Name: "index", Status: doctorOK, Message: "SQLite FTS5 is usable"}
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
		return DoctorCheck{Name: "merge-drivers", Status: doctorOK, Message: "state, notes, and registry drivers are registered"}
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
