package fsstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var _ ports.RecordMerger = (*Store)(nil)

// mergeDoc is one live decision, attempt or handoff file.
type mergeDoc struct {
	kind      string
	id        string
	title     string
	path      string
	rel       string
	raw       []byte
	body      string
	relations domain.RelationList
	validity  domain.Validity
	merge     domain.MergeInfo
}

// mergeNoteEntry is one marker entry of a daily notes file.
type mergeNoteEntry struct {
	id    string
	title string
	path  string
	rel   string
	start int
	end   int
	note  domain.Note
}

// mergeNoteFile is one parsed daily notes file.
type mergeNoteFile struct {
	path        string
	rel         string
	frontmatter noteFileFrontmatter
	lines       []string
	raw         []byte
}

// mergeTrashRecord is one record already in the project's trash.
type mergeTrashRecord struct {
	kind       string
	id         string
	title      string
	mergedInto string
}

// mergeUniverse is every live record and tombstone of one project, loaded once
// for a plan.
type mergeUniverse struct {
	store     *Store
	projectID string
	project   string
	liveDocs  map[string]*mergeDoc
	liveNotes map[string]*mergeNoteEntry
	noteFiles map[string]*mergeNoteFile
	trashed   map[string]*mergeTrashRecord
}

// mergeRecordOp is the metadata change of one record in a merge.
type mergeRecordOp struct {
	// mergedFrom is written to the survivor; nil leaves it unchanged.
	mergedFrom []string
	// tombstone is the survivor an absorbed record names; empty leaves it.
	tombstone string
	// validTo ends an absorbed record when it is still current.
	validTo time.Time
}

// mergeFileWork is one file the merge rewrites.
type mergeFileWork struct {
	relPath       string
	absPath       string
	change        string
	before        []byte
	after         []byte
	beforeMissing bool
	// movedTo and moveToAbs name the trash location of an absorbed record.
	movedTo   string
	moveToAbs string
}

// mergeWork is a complete plan with the byte-level changes it will apply.
type mergeWork struct {
	plan  ports.MergePlan
	files []*mergeFileWork
}

// PlanMerge computes a merge plan without writing anything. The plan names the
// survivor, the records it absorbs, the references that would be rewritten and
// every reason the merge cannot proceed.
func (s *Store) PlanMerge(ctx context.Context, request ports.MergeRequest) (ports.MergePlan, error) {
	if err := contextError(ctx); err != nil {
		return ports.MergePlan{}, err
	}
	work, err := s.planMerge(ctx, request)
	if err != nil {
		return ports.MergePlan{}, err
	}
	return work.plan, nil
}

// planMerge loads the project and builds the plan and its byte changes.
func (s *Store) planMerge(ctx context.Context, request ports.MergeRequest) (*mergeWork, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	projectID := strings.TrimSpace(request.ProjectID)
	if err := domain.ValidateRequired("project.id", projectID); err != nil {
		return nil, err
	}
	if err := s.requireProject(projectID); err != nil {
		return nil, err
	}
	survivorID := strings.TrimSpace(request.Survivor)
	if err := domain.ValidateRecordReference("survivor", survivorID); err != nil {
		return nil, err
	}
	universe, err := s.loadMergeUniverse(ctx, projectID)
	if err != nil {
		return nil, err
	}
	work := &mergeWork{plan: ports.MergePlan{ProjectID: projectID}}
	survivorKind, survivorTitle, survivorMerge, ok := universe.liveRecord(survivorID)
	if !ok {
		if _, trashed := universe.trashed[survivorID]; trashed {
			work.plan.Conflicts = append(work.plan.Conflicts,
				fmt.Sprintf("survivor %s is in the trash; restore it first", survivorID))
		} else {
			work.plan.Conflicts = append(work.plan.Conflicts, fmt.Sprintf("survivor %s does not exist", survivorID))
		}
		return work, nil
	}
	work.plan.Kind = survivorKind
	work.plan.Survivor = ports.MergeRecord{Kind: survivorKind, ID: survivorID, Title: survivorTitle, State: ports.MergeLive}
	if conflict := universe.currentConflict(survivorID, survivorKind); conflict != "" {
		work.plan.Conflicts = append(work.plan.Conflicts, conflict)
	}
	if survivorMerge.MergedInto != "" {
		work.plan.Conflicts = append(work.plan.Conflicts,
			fmt.Sprintf("survivor %s is itself merged into %s", survivorID, survivorMerge.MergedInto))
	}

	var duplicates []ports.MergeRecord
	seen := map[string]struct{}{}
	for _, raw := range request.Duplicates {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if err := domain.ValidateRecordReference("duplicate", id); err != nil {
			return nil, err
		}
		if id == survivorID {
			work.plan.Conflicts = append(work.plan.Conflicts, "the survivor cannot also be a duplicate")
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if kind, title, mergeInfo, live := universe.liveRecord(id); live {
			duplicates = append(duplicates, ports.MergeRecord{Kind: kind, ID: id, Title: title, State: ports.MergeLive})
			if kind != survivorKind {
				work.plan.Conflicts = append(work.plan.Conflicts,
					fmt.Sprintf("%s is a %s but the survivor is a %s; merge keeps one kind", id, kind, survivorKind))
			}
			if mergeInfo.MergedInto != "" {
				work.plan.Conflicts = append(work.plan.Conflicts,
					fmt.Sprintf("%s is already merged into %s", id, mergeInfo.MergedInto))
			}
			for _, absorbed := range mergeInfo.MergedFrom {
				if absorbed == survivorID {
					work.plan.Conflicts = append(work.plan.Conflicts,
						fmt.Sprintf("merging %s into %s would close a merge cycle", id, survivorID))
				}
			}
			if conflict := universe.currentConflict(id, kind); conflict != "" {
				work.plan.Conflicts = append(work.plan.Conflicts, conflict)
			}
			continue
		}
		if trashed, ok := universe.trashed[id]; ok {
			record := ports.MergeRecord{Kind: trashed.kind, ID: id, Title: trashed.title, State: ports.MergeTombstone}
			switch {
			case trashed.kind != survivorKind:
				work.plan.Conflicts = append(work.plan.Conflicts,
					fmt.Sprintf("%s is a %s but the survivor is a %s; merge keeps one kind", id, trashed.kind, survivorKind))
			case trashed.mergedInto == survivorID:
				work.plan.AlreadyMerged = append(work.plan.AlreadyMerged, record)
			case trashed.mergedInto != "":
				work.plan.Conflicts = append(work.plan.Conflicts,
					fmt.Sprintf("%s is already merged into %s", id, trashed.mergedInto))
			default:
				work.plan.Conflicts = append(work.plan.Conflicts,
					fmt.Sprintf("%s is in the trash; restore it before merging", id))
			}
			continue
		}
		work.plan.Conflicts = append(work.plan.Conflicts, fmt.Sprintf("duplicate %s does not exist", id))
	}
	work.plan.Duplicates = duplicates
	work.plan.AlreadyMerged = dedupeMergeRecords(work.plan.AlreadyMerged)
	if len(work.plan.Conflicts) > 0 {
		return work, nil
	}

	mergedFrom := append([]string(nil), survivorMerge.MergedFrom...)
	replacements := make(map[string]string, len(duplicates))
	for _, duplicate := range duplicates {
		mergedFrom = appendUnique(mergedFrom, duplicate.ID)
		replacements[duplicate.ID] = survivorID
	}
	for _, already := range work.plan.AlreadyMerged {
		mergedFrom = appendUnique(mergedFrom, already.ID)
	}
	work.plan.MergedFrom = mergedFrom
	if err := universe.buildMergeWork(work, survivorID, duplicates, mergedFrom, replacements); err != nil {
		return nil, err
	}
	work.plan.Fingerprint = mergeFingerprint(work)
	work.plan.Changed = len(work.files) > 0
	return work, nil
}

// buildMergeWork turns the plan into the per-file byte changes.
func (u *mergeUniverse) buildMergeWork(work *mergeWork, survivorID string, duplicates []ports.MergeRecord, mergedFrom []string, replacements map[string]string) error {
	now := u.store.nowUTC()
	ops := map[string]mergeRecordOp{}
	absorbed := make(map[string]struct{}, len(duplicates))
	for _, duplicate := range duplicates {
		absorbed[duplicate.ID] = struct{}{}
		ops[duplicate.ID] = mergeRecordOp{tombstone: survivorID, validTo: now}
	}
	ops[survivorID] = mergeRecordOp{mergedFrom: mergedFrom}

	files := map[string]*mergeFileWork{}
	addFile := func(rel, abs, change string, before, after []byte) {
		if string(before) == string(after) {
			return
		}
		if existing, ok := files[rel]; ok {
			existing.after = after
			existing.change = preferMergeChange(existing.change, change)
			return
		}
		files[rel] = &mergeFileWork{relPath: rel, absPath: abs, change: change, before: before, after: after}
	}

	// Live documents (decisions, attempts, handoffs).
	for _, doc := range u.sortedDocs() {
		after, rewrites, err := rewriteMergeDoc(doc, ops[doc.id], replacements, u.projectID)
		if err != nil {
			return err
		}
		work.plan.Links = append(work.plan.Links, rewrites...)
		change := ports.MergeChangeLink
		switch {
		case doc.id == survivorID:
			change = ports.MergeChangeSurvivor
		case hasID(absorbed, doc.id):
			change = ports.MergeChangeTombstone
		}
		addFile(doc.rel, doc.path, change, doc.raw, after)
	}

	// Daily notes files. A file may hold the survivor, duplicates and plain
	// notes at once, so it is rewritten entry by entry.
	removedByFile := map[string][]domain.Note{}
	for _, path := range u.sortedNoteFiles() {
		file := u.noteFiles[path]
		after, rewrites, removed, err := u.rewriteNoteFile(file, ops, replacements, survivorID)
		if err != nil {
			return err
		}
		work.plan.Links = append(work.plan.Links, rewrites...)
		change := ports.MergeChangeLink
		switch {
		case hasNoteEntry(file, survivorID):
			change = ports.MergeChangeSurvivor
		case len(removed) > 0:
			change = ports.MergeChangeTombstone
		}
		addFile(file.rel, file.path, change, file.raw, after)
		if len(removed) > 0 {
			removedByFile[file.path] = removed
		}
	}

	// Absorbed notes are appended to the trash file of their own daily date.
	for _, path := range u.sortedNoteFiles() {
		file := u.noteFiles[path]
		removed := removedByFile[path]
		if len(removed) == 0 {
			continue
		}
		trashAbs := filepath.Join(u.project, trashDir, notesDir, filepath.Base(file.path))
		trashRel, err := u.rel(trashAbs)
		if err != nil {
			return err
		}
		before, err := os.ReadFile(trashAbs)
		beforeMissing := false
		if err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("fsstore: read trash notes: %w", err)
			}
			beforeMissing = true
		}
		content := before
		if beforeMissing {
			content = nil
		}
		for _, note := range removed {
			content, err = noteFileWithEntryContent(content, note, noteDateStem(file.path), note.CreatedAt, now)
			if err != nil {
				return err
			}
		}
		if existing, ok := files[trashRel]; ok {
			existing.after = content
			existing.change = ports.MergeChangeMove
			continue
		}
		files[trashRel] = &mergeFileWork{
			relPath: trashRel, absPath: trashAbs, change: ports.MergeChangeMove,
			before: before, after: content, beforeMissing: beforeMissing,
		}
	}

	// Absorbed non-note files move to the trash after their tombstone bytes are
	// written.
	for _, doc := range u.sortedDocs() {
		if !hasID(absorbed, doc.id) {
			continue
		}
		trashAbs, err := u.freeTrashPath(doc.kind, filepath.Base(doc.path))
		if err != nil {
			return err
		}
		trashRel, err := u.rel(trashAbs)
		if err != nil {
			return err
		}
		workFile := files[doc.rel]
		if workFile == nil {
			continue
		}
		workFile.movedTo = trashRel
		workFile.moveToAbs = trashAbs
	}

	work.files = sortedMergeFiles(files)
	return nil
}

// rewriteMergeDoc rewrites one decision, attempt or handoff file. The body is
// rewritten textually, so hand-written sections survive, while the typed
// frontmatter carries the merge metadata and relation targets.
func rewriteMergeDoc(doc *mergeDoc, op mergeRecordOp, replacements map[string]string, projectID string) ([]byte, []ports.MergeLinkRewrite, error) {
	rewrites := mergeLinkRewrites(doc.kind, doc.id, "body", doc.body, projectID, replacements)
	relations, relationRewrites := rewriteMergeRelations(doc.kind, doc.id, doc.relations, replacements)
	rewrites = append(rewrites, relationRewrites...)
	newBody, _ := domain.RewriteRecordLinks(doc.body, projectID, replacements)
	switch doc.kind {
	case ports.RecordDecision:
		var frontmatter decisionFrontmatter
		_, err := markdown.Parse(doc.raw, &frontmatter)
		if err != nil {
			return nil, nil, fmt.Errorf("fsstore: parse decision %s: %w", doc.path, err)
		}
		if err := validateRecordFrontmatter(doc.path, frontmatter.Frontmatter, doc.kind); err != nil {
			return nil, nil, err
		}
		frontmatter.relationsFrontmatter = relationsToFrontmatter(relations)
		if err := applyMergeOp(&frontmatter.validityFrontmatter, &frontmatter.mergeFrontmatter, op, frontmatter.CreatedAt); err != nil {
			return nil, nil, err
		}
		rendered, err := markdown.Render(frontmatter, newBody)
		if err != nil {
			return nil, nil, fmt.Errorf("fsstore: render decision %s: %w", doc.path, err)
		}
		return rendered, rewrites, nil
	case ports.RecordAttempt:
		var frontmatter attemptFrontmatter
		_, err := markdown.Parse(doc.raw, &frontmatter)
		if err != nil {
			return nil, nil, fmt.Errorf("fsstore: parse attempt %s: %w", doc.path, err)
		}
		if err := validateRecordFrontmatter(doc.path, frontmatter.Frontmatter, doc.kind); err != nil {
			return nil, nil, err
		}
		frontmatter.relationsFrontmatter = relationsToFrontmatter(relations)
		if err := applyMergeOp(&frontmatter.validityFrontmatter, &frontmatter.mergeFrontmatter, op, frontmatter.CreatedAt); err != nil {
			return nil, nil, err
		}
		rendered, err := markdown.Render(frontmatter, newBody)
		if err != nil {
			return nil, nil, fmt.Errorf("fsstore: render attempt %s: %w", doc.path, err)
		}
		return rendered, rewrites, nil
	case ports.RecordHandoff:
		var frontmatter handoffFrontmatter
		_, err := markdown.Parse(doc.raw, &frontmatter)
		if err != nil {
			return nil, nil, fmt.Errorf("fsstore: parse handoff %s: %w", doc.path, err)
		}
		if err := validateRecordFrontmatter(doc.path, frontmatter.Frontmatter, doc.kind); err != nil {
			return nil, nil, err
		}
		frontmatter.relationsFrontmatter = relationsToFrontmatter(relations)
		if err := applyMergeOp(&frontmatter.validityFrontmatter, &frontmatter.mergeFrontmatter, op, frontmatter.CreatedAt); err != nil {
			return nil, nil, err
		}
		rendered, err := markdown.Render(frontmatter, newBody)
		if err != nil {
			return nil, nil, fmt.Errorf("fsstore: render handoff %s: %w", doc.path, err)
		}
		return rendered, rewrites, nil
	default:
		return nil, nil, domain.NewValidationError("kind", fmt.Sprintf("record kind %q cannot be merged", doc.kind))
	}
}

// applyMergeOp writes the merge fields into the frontmatter.
func applyMergeOp(validity *validityFrontmatter, merge *mergeFrontmatter, op mergeRecordOp, createdAt time.Time) error {
	if op.mergedFrom != nil {
		merge.MergedFrom = op.mergedFrom
		merge.MergedInto = ""
	}
	if op.tombstone == "" {
		return nil
	}
	merge.MergedInto = op.tombstone
	merge.MergedFrom = nil
	if op.validTo.IsZero() {
		return nil
	}
	current, err := validityFromFrontmatter(*validity, createdAt)
	if err != nil {
		return err
	}
	if !current.ValidTo.IsZero() {
		return nil
	}
	*validity = validityToFrontmatter(domain.Validity{
		ValidFrom: current.ValidFrom, ValidTo: op.validTo, Supersedes: current.Supersedes,
		SupersededBy: current.SupersededBy, Confidence: current.Confidence,
	}, createdAt)
	return nil
}

// rewriteNoteFile rewrites one daily notes file: links in every section, the
// survivor's merged_from, and relation targets. Absorbed entries are removed
// and returned for the trash file.
func (u *mergeUniverse) rewriteNoteFile(file *mergeNoteFile, ops map[string]mergeRecordOp, replacements map[string]string, survivorID string) ([]byte, []ports.MergeLinkRewrite, []domain.Note, error) {
	markerIndex := map[int]struct{}{}
	for i, line := range file.lines {
		if _, isMarker, err := parseNoteMarker(line); err != nil {
			return nil, nil, nil, fmt.Errorf("fsstore: parse notes %s: %w", file.path, err)
		} else if isMarker {
			markerIndex[i] = struct{}{}
		}
	}
	plain := make([]string, len(file.lines))
	for i, line := range file.lines {
		if _, marker := markerIndex[i]; marker {
			plain[i] = ""
			continue
		}
		plain[i] = line
	}
	rewritten, _ := domain.RewriteRecordLinks(strings.Join(plain, "\n"), u.projectID, replacements)
	lines := strings.Split(rewritten, "\n")
	for i := range markerIndex {
		lines[i] = file.lines[i]
	}

	entries, err := u.noteEntries(file, lines)
	if err != nil {
		return nil, nil, nil, err
	}
	var (
		rewrites []ports.MergeLinkRewrite
		removed  []domain.Note
		out      []string
	)
	cursor := 0
	for _, entry := range entries {
		out = append(out, lines[cursor:entry.start]...)
		metadata, _, err := parseNoteMarker(lines[entry.start])
		if err != nil {
			return nil, nil, nil, fmt.Errorf("fsstore: parse note %q: %w", entry.id, err)
		}
		note := entry.note
		note.ProjectID = u.projectID
		rewrites = append(rewrites, mergeLinkRewrites("note", entry.id, "body",
			strings.Join(file.lines[entry.start:entry.end], "\n"), u.projectID, replacements)...)
		relations, relationRewrites := rewriteMergeRelations("note", entry.id, note.Relations, replacements)
		rewrites = append(rewrites, relationRewrites...)
		metadata.relationsFrontmatter = relationsToFrontmatter(relations)
		note.Relations = relations
		op := ops[entry.id]
		if err := applyMergeOp(&metadata.validityFrontmatter, &metadata.mergeFrontmatter, op, note.CreatedAt); err != nil {
			return nil, nil, nil, err
		}
		note.Validity = mergeValidity(note.Validity, note.CreatedAt, op)
		note.MergeInfo = mergeInfoOf(metadata.mergeFrontmatter).Normalized()
		cursor = entry.end
		if entry.id != survivorID && op.tombstone != "" {
			removed = append(removed, note)
			continue
		}
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("fsstore: encode note %q: %w", entry.id, err)
		}
		out = append(out, noteMarkerPrefix+string(encoded)+noteMarkerSuffix)
		out = append(out, lines[entry.start+1:entry.end]...)
	}
	out = append(out, lines[cursor:]...)
	body := strings.Join(out, "\n")
	rendered, err := markdown.Render(file.frontmatter, body)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fsstore: render notes %s: %w", file.path, err)
	}
	return rendered, rewrites, removed, nil
}

// mergeValidity applies the merge end to a note's validity.
func mergeValidity(validity domain.Validity, createdAt time.Time, op mergeRecordOp) domain.Validity {
	if op.tombstone == "" || op.validTo.IsZero() || !validity.ValidTo.IsZero() {
		return validity
	}
	validity.ValidTo = op.validTo
	if validity.ValidFrom.IsZero() {
		validity.ValidFrom = createdAt
	}
	return validity
}

func mergeInfoOf(merge mergeFrontmatter) domain.MergeInfo {
	return domain.MergeInfo{MergedFrom: merge.MergedFrom, MergedInto: merge.MergedInto}
}

// mergeLinkRewrites lists the [[id]] references in text that a merge rewrites.
func mergeLinkRewrites(sourceKind, sourceID, field, text, projectID string, replacements map[string]string) []ports.MergeLinkRewrite {
	if len(replacements) == 0 || !strings.Contains(text, "[[") {
		return nil
	}
	var out []ports.MergeLinkRewrite
	seen := map[string]struct{}{}
	for _, link := range domain.ParseRecordLinks(text) {
		if link.ProjectID != "" && link.ProjectID != projectID {
			continue
		}
		replacement, ok := replacements[link.ID]
		if !ok || replacement == "" || replacement == link.ID {
			continue
		}
		ref := link.ID
		if link.ProjectID != "" {
			ref = link.ProjectID + "/" + link.ID
		}
		if _, duplicate := seen[ref]; duplicate {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ports.MergeLinkRewrite{
			SourceKind: sourceKind, SourceID: sourceID, Field: field,
			Ref: ref, OldID: link.ID, NewID: replacement,
		})
	}
	return out
}

// rewriteMergeRelations points every relation at the survivor.
func rewriteMergeRelations(sourceKind, sourceID string, relations domain.RelationList, replacements map[string]string) (domain.RelationList, []ports.MergeLinkRewrite) {
	if len(relations) == 0 || len(replacements) == 0 {
		return relations, nil
	}
	out := make(domain.RelationList, 0, len(relations))
	var rewrites []ports.MergeLinkRewrite
	for _, relation := range relations {
		normalized := relation.Normalize()
		if replacement, ok := replacements[normalized.Target]; ok && replacement != normalized.Target {
			rewrites = append(rewrites, ports.MergeLinkRewrite{
				SourceKind: sourceKind, SourceID: sourceID, Field: "relation",
				Ref: normalized.Target, OldID: normalized.Target, NewID: replacement,
			})
			normalized.Target = replacement
		}
		out = append(out, normalized)
	}
	return out.Normalize(), rewrites
}

// freeTrashPath returns a free path under trash/<kindDir> for name.
func (u *mergeUniverse) freeTrashPath(kind, name string) (string, error) {
	dir := filepath.Join(u.project, trashDir, recordKindDirs[kind])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("fsstore: create trash %s: %w", dir, err)
	}
	return freeTrashPath(dir, name)
}

// rel renders a path relative to the data directory with forward slashes.
func (u *mergeUniverse) rel(path string) (string, error) {
	rel, err := filepath.Rel(u.store.root, path)
	if err != nil {
		return "", domain.NewValidationError("path", "must stay inside the data directory")
	}
	return filepath.ToSlash(rel), nil
}

// loadMergeUniverse reads every live record file and every trash record of a
// project. Files that cannot be read are skipped, like listing skips them; a
// merge never edits a file it cannot parse.
func (s *Store) loadMergeUniverse(ctx context.Context, projectID string) (*mergeUniverse, error) {
	project, err := s.projectDir(projectID)
	if err != nil {
		return nil, err
	}
	universe := &mergeUniverse{
		store: s, projectID: projectID, project: project,
		liveDocs: map[string]*mergeDoc{}, liveNotes: map[string]*mergeNoteEntry{},
		noteFiles: map[string]*mergeNoteFile{}, trashed: map[string]*mergeTrashRecord{},
	}
	for _, kind := range recordKinds {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		dir := filepath.Join(project, recordKindDirs[kind])
		paths, err := recordPaths(dir)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil || len(raw) == 0 {
				continue
			}
			rel, err := universe.rel(path)
			if err != nil {
				return nil, err
			}
			if kind == ports.RecordNote {
				var frontmatter noteFileFrontmatter
				body, err := markdown.Parse(raw, &frontmatter)
				if err != nil {
					continue
				}
				file := &mergeNoteFile{path: path, rel: rel, frontmatter: frontmatter, lines: strings.Split(body, "\n"), raw: raw}
				universe.noteFiles[path] = file
				entries, err := universe.noteEntries(file, file.lines)
				if err != nil {
					continue
				}
				for _, entry := range entries {
					if _, exists := universe.liveNotes[entry.id]; !exists {
						copied := entry
						universe.liveNotes[entry.id] = &copied
					}
				}
				continue
			}
			doc, err := readMergeDoc(kind, path, rel, raw)
			if err != nil {
				continue
			}
			if _, exists := universe.liveDocs[doc.id]; !exists {
				universe.liveDocs[doc.id] = doc
			}
		}
	}
	for _, kind := range recordKinds {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		dir := filepath.Join(project, trashDir, recordKindDirs[kind])
		paths, err := recordPaths(dir)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil || len(raw) == 0 {
				continue
			}
			if kind == ports.RecordNote {
				var frontmatter noteFileFrontmatter
				body, err := markdown.Parse(raw, &frontmatter)
				if err != nil {
					continue
				}
				file := &mergeNoteFile{path: path, frontmatter: frontmatter, lines: strings.Split(body, "\n")}
				entries, err := universe.noteEntries(file, file.lines)
				if err != nil {
					continue
				}
				for _, entry := range entries {
					universe.trashed[entry.id] = &mergeTrashRecord{
						kind: kind, id: entry.id, title: entry.title, mergedInto: strings.TrimSpace(entry.note.MergeInfo.MergedInto),
					}
				}
				continue
			}
			doc, err := readMergeDoc(kind, path, "", raw)
			if err != nil {
				continue
			}
			universe.trashed[doc.id] = &mergeTrashRecord{
				kind: kind, id: doc.id, title: doc.title, mergedInto: strings.TrimSpace(doc.merge.MergedInto),
			}
		}
	}
	return universe, nil
}

// readMergeDoc parses one decision, attempt or handoff file into the merge view.
func readMergeDoc(kind, path, rel string, raw []byte) (*mergeDoc, error) {
	doc := &mergeDoc{kind: kind, path: path, rel: rel, raw: raw}
	switch kind {
	case ports.RecordDecision:
		var frontmatter decisionFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return nil, err
		}
		createdAt := frontmatter.CreatedAt.UTC()
		doc.id, doc.title = frontmatter.ID, parseDecisionTitle(body)
		doc.body = body
		doc.validity, err = validityFromFrontmatter(frontmatter.validityFrontmatter, createdAt)
		if err != nil {
			return nil, err
		}
		doc.merge, err = mergeFromFrontmatter(frontmatter.mergeFrontmatter, "decision.merge", doc.id)
		if err != nil {
			return nil, err
		}
		doc.relations, err = relationsFromFrontmatter(frontmatter.relationsFrontmatter, "decision.relations", doc.id)
		if err != nil {
			return nil, err
		}
	case ports.RecordAttempt:
		var frontmatter attemptFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return nil, err
		}
		createdAt := frontmatter.CreatedAt.UTC()
		doc.id, doc.title = frontmatter.ID, parseAttemptTitle(body)
		doc.body = body
		doc.validity, err = validityFromFrontmatter(frontmatter.validityFrontmatter, createdAt)
		if err != nil {
			return nil, err
		}
		doc.merge, err = mergeFromFrontmatter(frontmatter.mergeFrontmatter, "attempt.merge", doc.id)
		if err != nil {
			return nil, err
		}
		doc.relations, err = relationsFromFrontmatter(frontmatter.relationsFrontmatter, "attempt.relations", doc.id)
		if err != nil {
			return nil, err
		}
	case ports.RecordHandoff:
		var frontmatter handoffFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return nil, err
		}
		doc.id = frontmatter.ID
		doc.title = "Handoff " + frontmatter.CreatedAt.UTC().Format(time.RFC3339)
		doc.body = body
		doc.validity, err = validityFromFrontmatter(frontmatter.validityFrontmatter, frontmatter.CreatedAt.UTC())
		if err != nil {
			return nil, err
		}
		doc.merge, err = mergeFromFrontmatter(frontmatter.mergeFrontmatter, "handoff.merge", doc.id)
		if err != nil {
			return nil, err
		}
		doc.relations, err = relationsFromFrontmatter(frontmatter.relationsFrontmatter, "handoff.relations", doc.id)
		if err != nil {
			return nil, err
		}
	}
	if doc.id == "" {
		return nil, domain.NewValidationError("record.id", fmt.Sprintf("%s has no id", path))
	}
	return doc, nil
}

func parseDecisionTitle(body string) string {
	title, _, _, _, _, _, _ := parseDecisionBody(body)
	return title
}

func parseAttemptTitle(body string) string {
	title, _, _, _ := parseAttemptBody(body)
	return title
}

// noteEntries returns the marker entries of one daily notes file.
func (u *mergeUniverse) noteEntries(file *mergeNoteFile, lines []string) ([]mergeNoteEntry, error) {
	var markers []int
	for i, line := range lines {
		if _, isMarker, err := parseNoteMarker(line); err != nil {
			return nil, err
		} else if isMarker {
			markers = append(markers, i)
		}
	}
	entries := make([]mergeNoteEntry, 0, len(markers))
	for k, start := range markers {
		end := len(lines)
		if k+1 < len(markers) {
			end = markers[k+1]
		}
		notes, err := parseNotesBody(strings.Join(lines[start:end], "\n"), file.frontmatter.UpdatedAt)
		if err != nil {
			return nil, err
		}
		if len(notes) != 1 {
			continue
		}
		note := notes[0]
		note.ProjectID = u.projectID
		note.UpdatedAt = file.frontmatter.UpdatedAt
		entries = append(entries, mergeNoteEntry{
			id: note.ID, title: note.Title, path: file.path, rel: file.rel,
			start: start, end: end, note: note,
		})
	}
	return entries, nil
}

// liveRecord resolves one live record of any kind.
func (u *mergeUniverse) liveRecord(id string) (kind, title string, merge domain.MergeInfo, ok bool) {
	if doc, found := u.liveDocs[id]; found {
		return doc.kind, doc.title, doc.merge, true
	}
	if entry, found := u.liveNotes[id]; found {
		return ports.RecordNote, entry.title, entry.note.MergeInfo, true
	}
	return "", "", domain.MergeInfo{}, false
}

// currentConflict reports why a live record cannot take part in a merge.
func (u *mergeUniverse) currentConflict(id, kind string) string {
	var validity domain.Validity
	if doc, found := u.liveDocs[id]; found {
		validity = doc.validity
	} else if entry, found := u.liveNotes[id]; found {
		validity = entry.note.Validity
	} else {
		return ""
	}
	if validity.ValidTo.IsZero() {
		return ""
	}
	return fmt.Sprintf("%s %s is not current (valid_to %s); merge its replacement instead",
		kind, id, validity.ValidTo.UTC().Format(time.RFC3339))
}

func (u *mergeUniverse) sortedDocs() []*mergeDoc {
	out := make([]*mergeDoc, 0, len(u.liveDocs))
	for _, doc := range u.liveDocs {
		out = append(out, doc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

func (u *mergeUniverse) sortedNoteFiles() []string {
	out := make([]string, 0, len(u.noteFiles))
	for path := range u.noteFiles {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// containsNoteEntry reports whether a daily file holds the note id.
func hasNoteEntry(file *mergeNoteFile, id string) bool {
	for _, line := range file.lines {
		metadata, isMarker, err := parseNoteMarker(line)
		if err != nil || !isMarker {
			continue
		}
		if metadata.ID == id {
			return true
		}
	}
	return false
}

func hasID(set map[string]struct{}, id string) bool {
	_, ok := set[id]
	return ok
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func dedupeMergeRecords(records []ports.MergeRecord) []ports.MergeRecord {
	seen := map[string]struct{}{}
	out := make([]ports.MergeRecord, 0, len(records))
	for _, record := range records {
		if _, duplicate := seen[record.ID]; duplicate {
			continue
		}
		seen[record.ID] = struct{}{}
		out = append(out, record)
	}
	return out
}

func preferMergeChange(current, next string) string {
	rank := map[string]int{
		ports.MergeChangeRelation: 0, ports.MergeChangeLink: 1,
		ports.MergeChangeMove: 2, ports.MergeChangeTombstone: 3, ports.MergeChangeSurvivor: 4,
	}
	if rank[next] > rank[current] {
		return next
	}
	return current
}

func sortedMergeFiles(files map[string]*mergeFileWork) []*mergeFileWork {
	out := make([]*mergeFileWork, 0, len(files))
	for _, file := range files {
		out = append(out, file)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].relPath < out[j].relPath })
	return out
}

// mergeFingerprint covers the request and every pre-merge file hash, so the
// apply can refuse a plan whose inputs moved.
func mergeFingerprint(work *mergeWork) string {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x1f%s\x1f%s\x1f%s\x1f", work.plan.ProjectID, work.plan.Kind,
		work.plan.Survivor.ID, strings.Join(work.plan.MergedFrom, ","))
	for _, file := range work.files {
		fmt.Fprintf(hash, "%s\x1f%s\x1f%s\x1f", file.relPath, file.change, hashBytes(file.before))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func hashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
