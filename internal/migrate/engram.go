package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/redact"
)

// EngramOptions configures the engram source adapter. The source is opened
// read-only; only the target directory is ever written, and only when DryRun is
// false.
type EngramOptions struct {
	// SourcePath is an engram.db file or an `engram export` JSON file.
	SourcePath string
	// TargetDir is the LV24 Brain data directory to import into.
	TargetDir string
	// DryRun plans and reports without creating or changing a file.
	DryRun bool
	// IncludePrompts imports user prompts into the local-only prompt zone.
	IncludePrompts bool
	// PromptsEnabled must be true when IncludePrompts is set: the prompt zone
	// is an explicit opt-in and a migration may not turn it on silently.
	PromptsEnabled bool
	// ProjectMap overrides the default project remapping rules.
	ProjectMap ProjectMap
	// SkipConflicts imports the files that do not collide with the target and
	// reports the rest. Without it a plan with any conflicting target aborts
	// before a single file is written.
	SkipConflicts bool
	// RepairTimestamps rewrites the note files of an earlier import so every
	// note keeps its own updated_at. Legacy daily files that the new
	// per-timestamp files replace are kept aside with a .replaced suffix.
	RepairTimestamps bool
	// Now supplies the record clock for rows with unusable timestamps. It
	// defaults to time.Now; a deterministic value makes a test reproducible.
	Now func() time.Time
}

type engramRecord struct {
	kind         string
	id           string
	syncID       string
	externalID   string
	project      string
	scope        string
	subtype      string
	topicKey     string
	reviewAfter  time.Time
	createdAt    time.Time
	updatedAt    time.Time
	title        string
	body         string
	sourceBody   string
	context      string
	alternatives []string
	consequences []string
	evidence     []string
	session      string
	quarantine   bool
	occurrences  int
	history      []domain.HistoryEntry
	relations    domain.RelationList
}

type engramRowPlan struct {
	observation engramObservation
	rule        projectRule
	collapsed   bool
	failed      bool
	record      *engramRecord
	survivor    *engramRowPlan
	occurrences int
	history     []domain.HistoryEntry
}

type engramPromptEntry struct {
	project string
	at      time.Time
	session string
	text    string
}

type engramPlannedFile struct {
	rel     string
	content []byte
	kind    string
	project string
	source  string
}

type engramPlan struct {
	records []*engramRecord
	files   []engramPlannedFile
	prompts []engramPromptEntry
}

// RunEngram imports an engram store. A nil context is context.Background.
func RunEngram(ctx context.Context, opts EngramOptions) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sourcePath, target, err := resolveEngramDirs(opts)
	if err != nil {
		return Result{}, err
	}
	if opts.IncludePrompts && !opts.PromptsEnabled {
		return Result{}, fmt.Errorf("migrate: prompts are enabled with --include-prompts but prompts.enabled is false in brain.yaml; enable the prompt zone first")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	result := Result{
		SourceDir:      sourcePath,
		TargetDir:      target,
		DryRun:         opts.DryRun,
		SourceSystem:   EngramSourceSystem,
		Projects:       []string{},
		Files:          []FileResult{},
		QuarantineRows: []QuarantineRecord{},
		SkippedRows:    []QuarantineRecord{},
		Duplicates:     []DuplicateRecord{},
		Collapse:       []CollapseGroup{},
		ProjectMapping: []ProjectMapping{},
		Warnings:       []string{},
	}
	if result.TreeHashBefore, err = hashTargetTree(target); err != nil {
		return result, err
	}
	source, err := loadEngram(ctx, sourcePath)
	if err != nil {
		return result, err
	}
	result.Scanned = len(source.Observations)
	result.Sessions = len(source.Sessions)
	result.PromptRows = len(source.Prompts)
	result.PromptsOptIn = opts.IncludePrompts
	superseded := 0
	for _, observation := range source.Observations {
		if strings.TrimSpace(observation.SupersededBy) != "" {
			superseded++
		}
	}
	if superseded > 0 {
		result.warn(fmt.Sprintf("%d source rows carry superseded_by_sync_id; supersession is kept as a relation, not as validity", superseded))
	}
	projectMap := mergeProjectMap(opts.ProjectMap)
	if err := projectMap.Validate(); err != nil {
		return result, err
	}
	plan, err := planEngram(ctx, source, projectMap.normalized(), now, opts.IncludePrompts, &result)
	if err != nil {
		return result, err
	}
	result.Imported = countImported(plan)
	result.Projects = plannedProjects(plan)
	if result.Failed > 0 {
		return result, &IncompleteError{Conflicts: result.Conflicts, Failed: result.Failed}
	}
	if err := renderEngramFiles(ctx, plan, &result); err != nil {
		return result, err
	}
	// A daily file from an earlier import already holds notes this plan would
	// write again. Reporting it as a conflict keeps the target free of duplicate
	// notes; --repair-timestamps is the explicit way to replace that layout.
	legacyBlocked, err := legacyLayoutDays(target, plan)
	if err != nil {
		return result, err
	}
	if opts.RepairTimestamps {
		if opts.DryRun {
			if err := repairEngramNoteFiles(target, plan, true, &result); err != nil {
				return result, err
			}
		}
	} else if len(legacyBlocked) > 0 {
		conflicts := legacyConflictResults(legacyBlocked)
		if opts.DryRun || opts.SkipConflicts {
			dropBlockedDays(plan, legacyBlocked, &result)
			result.Conflicts += len(conflicts)
			result.Files = append(result.Files, conflicts...)
		} else {
			result.Conflicts = len(conflicts)
			result.Files = conflicts
			result.TreeHashAfter = result.TreeHashBefore
			return result, &AbortedError{Conflicts: len(conflicts)}
		}
	}
	// The plan is complete now. A conflicting target means the run cannot
	// apply the plan as a whole, so refuse before touching any file.
	if !opts.DryRun && !opts.SkipConflicts {
		if conflicts := plannedConflicts(target, plan.files); len(conflicts) > 0 {
			result.Conflicts = len(conflicts)
			result.Files = conflicts
			result.TreeHashAfter = result.TreeHashBefore
			return result, &AbortedError{Conflicts: len(conflicts)}
		}
	}
	if opts.RepairTimestamps && !opts.DryRun {
		if err := repairEngramNoteFiles(target, plan, false, &result); err != nil {
			return result, err
		}
	}
	for _, file := range plan.files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		storeFile(target, file.rel, file.content, file.project, file.kind, file.source, opts.DryRun, &result)
	}
	if result.TreeHashAfter, err = hashTargetTree(target); err != nil {
		return result, err
	}
	if opts.DryRun && result.TreeHashBefore != result.TreeHashAfter {
		return result, fmt.Errorf("migrate: dry run changed the target tree (%s -> %s); nothing should have been written",
			result.TreeHashBefore, result.TreeHashAfter)
	}
	if result.Conflicts > 0 || result.Failed > 0 {
		return result, &IncompleteError{Conflicts: result.Conflicts, Failed: result.Failed}
	}
	return result, nil
}

// plannedConflicts reports the planned files whose target already exists with
// different content. It never writes: the same comparison storeFile performs is
// run against a throwaway result.
func plannedConflicts(target string, files []engramPlannedFile) []FileResult {
	preview := Result{Files: []FileResult{}}
	for _, file := range files {
		storeFile(target, file.rel, file.content, file.project, file.kind, file.source, true, &preview)
	}
	conflicts := make([]FileResult, 0, preview.Conflicts)
	for _, file := range preview.Files {
		if file.Status == StatusConflict {
			conflicts = append(conflicts, file)
		}
	}
	return conflicts
}

func resolveEngramDirs(opts EngramOptions) (string, string, error) {
	sourceValue := strings.TrimSpace(opts.SourcePath)
	if sourceValue == "" {
		return "", "", fmt.Errorf("migrate: engram --db path is required")
	}
	targetValue := strings.TrimSpace(opts.TargetDir)
	if targetValue == "" {
		return "", "", fmt.Errorf("migrate: target dir is required")
	}
	source, err := filepath.Abs(sourceValue)
	if err != nil {
		return "", "", fmt.Errorf("migrate: resolve engram source: %w", err)
	}
	target, err := filepath.Abs(targetValue)
	if err != nil {
		return "", "", fmt.Errorf("migrate: resolve target dir: %w", err)
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", "", fmt.Errorf("migrate: engram source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("migrate: engram source %s is not a regular file", source)
	}
	if samePath(source, target) {
		return "", "", fmt.Errorf("migrate: engram source and target must be different")
	}
	if isWithin(target, source) {
		return "", "", fmt.Errorf("migrate: engram source must not be inside the target data directory")
	}
	if targetInfo, err := os.Stat(target); err == nil {
		if !targetInfo.IsDir() {
			return "", "", fmt.Errorf("migrate: target %s is not a directory", target)
		}
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("migrate: target dir: %w", err)
	}
	return source, target, nil
}

func countImported(plan *engramPlan) int {
	count := 0
	for _, record := range plan.records {
		if !record.quarantine {
			count++
		}
	}
	return count
}

func plannedProjects(plan *engramPlan) []string {
	seen := map[string]struct{}{}
	for _, record := range plan.records {
		seen[record.project] = struct{}{}
	}
	for _, prompt := range plan.prompts {
		seen[prompt.project] = struct{}{}
	}
	projects := make([]string, 0, len(seen))
	for project := range seen {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	return projects
}

func planEngram(ctx context.Context, source engramSource, projectMap normalizedProjectMap, now func() time.Time, includePrompts bool, result *Result) (*engramPlan, error) {
	sessions := make(map[string]engramSession, len(source.Sessions))
	for _, session := range source.Sessions {
		sessions[session.ID] = session
	}
	mapping := newProjectMappingReport()
	rows := make([]*engramRowPlan, 0, len(source.Observations))
	for _, observation := range source.Observations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := planEngramRow(observation, projectMap, sessions, result)
		mapping.add(observation.Project, sessionDirectory(sessions, observation.SessionID), row.rule)
		rows = append(rows, row)
	}
	collapseDuplicateSyncIDs(rows, result)
	collapseSessionSummaries(rows, result)
	result.ProjectMapping = mapping.list()
	plan := &engramPlan{}
	byObservationID := map[int64]*engramRecord{}
	byExternalID := map[string]*engramRecord{}
	for _, row := range rows {
		if row.collapsed || row.failed || row.rule.Action == projectActionSkip {
			continue
		}
		record := buildEngramRecord(row, now(), result)
		if record == nil {
			row.failed = true
			result.fail(row.observation.Project, row.observation.Type, engramSourcePath(row.observation),
				"", "record could not be built")
			continue
		}
		row.record = record
		plan.records = append(plan.records, record)
		byObservationID[row.observation.ID] = record
		byExternalID[record.externalID] = record
		if record.quarantine {
			result.Quarantined++
			result.QuarantineRows = append(result.QuarantineRows, QuarantineRecord{
				SourceID: record.externalID, Project: row.observation.Project,
				Type: row.observation.Type, Title: record.title, Target: record.project,
				Reason: row.rule.Reason,
			})
		}
	}
	// A collapsed row still points at the survivor that represents it, so
	// relations and versions of an older session summary are preserved on the
	// collapsed note instead of being dropped as dangling.
	for _, row := range rows {
		if row.record == nil && row.survivor != nil && row.survivor.record != nil {
			byObservationID[row.observation.ID] = row.survivor.record
			byExternalID[engramExternalID(row.observation)] = row.survivor.record
		}
	}
	assignEngramRecordIDs(plan.records)
	applyEngramVersions(source.Versions, byObservationID, result)
	applyEngramRelations(source.Relations, byExternalID, result)
	findCrossSourceDuplicates(result, plan.records)
	findInternalDuplicates(result, plan.records)
	if includePrompts {
		known := map[string]bool{}
		for _, record := range plan.records {
			known[record.project] = true
		}
		for _, project := range registryProjectIDs(result.TargetDir) {
			known[project] = true
		}
		planPrompts(plan, source.Prompts, projectMap, sessions, known, result)
	}
	return plan, nil
}

func planEngramRow(observation engramObservation, projectMap normalizedProjectMap, sessions map[string]engramSession, result *Result) *engramRowPlan {
	row := &engramRowPlan{observation: observation}
	directory := sessionDirectory(sessions, observation.SessionID)
	row.rule = projectMap.resolve(observation.Project, directory)
	if row.rule.Action == projectActionQuarantine {
		result.QuarantinedRows++
	}
	if row.rule.Action == projectActionSkip {
		result.Skipped++
		result.SkippedRows = append(result.SkippedRows, QuarantineRecord{
			SourceID: engramExternalID(observation), Project: observation.Project,
			Type: observation.Type, Title: observation.Title, Reason: row.rule.Reason,
		})
		return row
	}
	if strings.TrimSpace(observation.Content) == "" {
		result.Skipped++
		result.SkippedRows = append(result.SkippedRows, QuarantineRecord{
			SourceID: engramExternalID(observation), Project: observation.Project,
			Type: observation.Type, Title: observation.Title, Reason: "empty-body",
		})
		row.rule.Action = projectActionSkip
		return row
	}
	if parseEngramTime(observation.CreatedAt).IsZero() && parseEngramTime(observation.UpdatedAt).IsZero() {
		row.failed = true
		result.fail(observation.Project, observation.Type, engramSourcePath(observation), "",
			fmt.Sprintf("row %d has no usable created_at/updated_at", observation.ID))
	}
	if strings.TrimSpace(observation.SyncID) == "" {
		result.warn(fmt.Sprintf("observation %d has no sync_id; identity falls back to the local row id", observation.ID))
	}
	return row
}

func sessionDirectory(sessions map[string]engramSession, sessionID string) string {
	if session, ok := sessions[sessionID]; ok {
		return session.Directory
	}
	return ""
}

// registryProjectIDs reads the project ids already registered in the target.
// A missing or malformed registry is not an error: prompt import then only
// trusts projects named by imported observations.
func registryProjectIDs(target string) []string {
	raw, err := os.ReadFile(filepath.Join(target, registryName))
	if err != nil {
		return nil
	}
	var registry struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		return nil
	}
	ids := make([]string, 0, len(registry.Projects))
	for _, project := range registry.Projects {
		if strings.TrimSpace(project.ID) != "" {
			ids = append(ids, project.ID)
		}
	}
	return ids
}

func collapseDuplicateSyncIDs(rows []*engramRowPlan, result *Result) {
	groups := map[string][]*engramRowPlan{}
	order := []string{}
	for _, row := range rows {
		if row.rule.Action == projectActionSkip || row.failed {
			continue
		}
		key := engramExternalID(row.observation)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], row)
	}
	for _, key := range order {
		group := groups[key]
		if len(group) < 2 {
			continue
		}
		survivor := group[0]
		for _, candidate := range group[1:] {
			if betterEngramRow(candidate, survivor) {
				survivor = candidate
			}
		}
		for _, row := range group {
			if row == survivor {
				continue
			}
			row.collapsed = true
			row.survivor = survivor
			result.Collapsed++
		}
		result.Collapse = append(result.Collapse, CollapseGroup{
			Project: survivor.rule.Project, Title: survivor.observation.Title,
			Reason: "duplicate-sync-id", Rows: len(group), Kept: key,
		})
	}
}

func betterEngramRow(candidate, current *engramRowPlan) bool {
	candidateUpdated := parseEngramTime(candidate.observation.UpdatedAt)
	currentUpdated := parseEngramTime(current.observation.UpdatedAt)
	if !candidateUpdated.Equal(currentUpdated) {
		return candidateUpdated.After(currentUpdated)
	}
	return candidate.observation.ID > current.observation.ID
}

func collapseSessionSummaries(rows []*engramRowPlan, result *Result) {
	groups := map[string][]*engramRowPlan{}
	order := []string{}
	for _, row := range rows {
		if row.collapsed || row.failed || row.rule.Action == projectActionSkip {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(row.observation.Type), "session_summary") {
			continue
		}
		key := row.rule.Project + "\x00" + normalizeEngramTitle(row.observation.Title)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], row)
	}
	for _, key := range order {
		group := groups[key]
		survivor := group[0]
		for _, candidate := range group[1:] {
			if betterSummaryRow(candidate, survivor) {
				survivor = candidate
			}
		}
		older := make([]*engramRowPlan, 0, len(group)-1)
		for _, row := range group {
			if row == survivor {
				continue
			}
			row.collapsed = true
			row.survivor = survivor
			result.Collapsed++
			older = append(older, row)
		}
		appendSummaryHistory(survivor, older)
		result.Collapse = append(result.Collapse, CollapseGroup{
			Project: survivor.rule.Project, Title: survivor.observation.Title,
			Reason: "session-summary", Rows: len(group), Kept: engramExternalID(survivor.observation),
		})
	}
}

func betterSummaryRow(candidate, current *engramRowPlan) bool {
	candidateCreated := parseEngramTime(candidate.observation.CreatedAt)
	currentCreated := parseEngramTime(current.observation.CreatedAt)
	if !candidateCreated.Equal(currentCreated) {
		return candidateCreated.After(currentCreated)
	}
	return candidate.observation.ID > current.observation.ID
}

// summaryCollapse is stashed on the surviving row because records are built
// after the collapse pass.
func appendSummaryHistory(survivor *engramRowPlan, older []*engramRowPlan) {
	sort.SliceStable(older, func(i, j int) bool {
		left := parseEngramTime(older[i].observation.UpdatedAt)
		right := parseEngramTime(older[j].observation.UpdatedAt)
		if !left.Equal(right) {
			return left.After(right)
		}
		return older[i].observation.ID > older[j].observation.ID
	})
	history := make([]domain.HistoryEntry, 0, len(older))
	for _, row := range older {
		at := parseEngramTime(row.observation.UpdatedAt)
		if at.IsZero() {
			at = parseEngramTime(row.observation.CreatedAt)
		}
		body, _ := redact.Apply(strings.TrimSpace(row.observation.Content))
		entry := domain.HistoryEntry{Body: body}
		if !at.IsZero() {
			entry.At = at.UTC().Format(time.RFC3339)
		}
		history = append(history, entry)
	}
	survivor.occurrences = len(older) + 1
	survivor.history = domain.BoundedHistory(history)
}
