package exchange

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Import reads a bundle and imports curated records.
func Import(ctx context.Context, dst Source, opts ImportOptions) (ImportReport, error) {
	return importBundle(ctx, dst, opts)
}
func ImportBundle(ctx context.Context, dst Source, opts ImportOptions) (ImportReport, error) {
	return importBundle(ctx, dst, opts)
}
func RunImport(ctx context.Context, dst Source, opts ImportOptions) (ImportReport, error) {
	return importBundle(ctx, dst, opts)
}
func ImportFromDir(ctx context.Context, dst Source, opts ImportOptions) (ImportReport, error) {
	return importBundle(ctx, dst, opts)
}

type importAction struct {
	kind   string // project, record
	status string // create, unchanged, conflict, quarantined, skipped
	record RecordEntry
	proj   domain.Project
}

type importManifest struct {
	Version   int          `json:"version"`
	From      string       `json:"from"`
	StartedAt time.Time    `json:"started_at"`
	Status    string       `json:"status"`
	Report    ImportReport `json:"report"`
	Error     string       `json:"error,omitempty"`
}

func importBundle(ctx context.Context, dst Source, opts ImportOptions) (ImportReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	from := strings.TrimSpace(opts.From)
	if from == "" {
		return ImportReport{}, fmt.Errorf("exchange: --from is required")
	}
	release, err := lockImport(ctx, dst)
	if err != nil {
		return ImportReport{}, err
	}
	defer release()

	dir := from
	tempDir := ""
	if strings.HasSuffix(strings.ToLower(from), ".zip") {
		td, err := os.MkdirTemp("", "lv24-exchange-*")
		if err != nil {
			return ImportReport{}, err
		}
		tempDir = td
		defer os.RemoveAll(tempDir)
		if err := unzipTo(from, tempDir); err != nil {
			return ImportReport{}, err
		}
		dir = tempDir
	}
	report := ImportReport{}

	manifestPath := filepath.Join(dir, "manifest.json")
	manifestBytes, _ := os.ReadFile(manifestPath)
	if len(manifestBytes) > 0 {
		var m Manifest
		if err := json.Unmarshal(manifestBytes, &m); err != nil {
			return report, fmt.Errorf("invalid manifest: %w", err)
		}
		if err := ValidateManifestVersion(m.FormatVersion); err != nil {
			if m.Version != "" {
				if err2 := ValidateManifestVersion(m.Version); err2 != nil {
					return report, err
				}
			} else {
				return report, err
			}
		}
		if err := verifyManifestFiles(dir, m.Files); err != nil {
			return report, err
		}
	}

	projectEntries := readProjectEntries(filepath.Join(dir, "projects.jsonl"))
	projectMap := map[string]ProjectEntry{}
	for _, pe := range projectEntries {
		projectMap[pe.ID] = pe
	}

	existingProjects, err := dst.ListProjects(ctx)
	if err != nil {
		return report, err
	}
	existingProjectSet := map[string]bool{}
	for _, p := range existingProjects {
		existingProjectSet[p.ID] = true
	}

	actions, err := planImport(ctx, dst, opts, dir, projectEntries, projectMap, existingProjectSet)
	if err != nil {
		return report, err
	}

	if opts.DryRun {
		// A dry run writes nothing, including no run manifest under backups/.
		return summarizePlan(actions), nil
	}

	runManifestPath, _, manifestErr := writeImportManifest(dst, from, "in_progress", report, "")
	if manifestErr != nil {
		return report, manifestErr
	}

	createdProjects := map[string]bool{}
	for _, action := range actions {
		if action.kind != "project" || action.status != "create" {
			continue
		}
		if existingProjectSet[action.proj.ID] || createdProjects[action.proj.ID] {
			continue
		}
		if err := dst.PutProject(ctx, action.proj); err != nil {
			report.Skipped++
			if action.proj.ID != "" {
				report.SkippedIDs = append(report.SkippedIDs, action.proj.ID)
			}
			continue
		}
		existingProjectSet[action.proj.ID] = true
		createdProjects[action.proj.ID] = true
		report.Projects++
	}

	for _, action := range actions {
		if action.kind != "record" {
			continue
		}
		switch action.status {
		case "unchanged":
			report.Unchanged++
		case "conflict":
			report.Conflicts++
			report.ConflictIDs = append(report.ConflictIDs, action.record.ID)
		case "quarantined":
			report.Quarantined++
		case "skipped":
			report.Skipped++
			if action.record.ID != "" {
				report.SkippedIDs = append(report.SkippedIDs, action.record.ID)
			}
		case "create":
			if !existingProjectSet[action.record.ProjectID] {
				report.Quarantined++
				continue
			}
			if err := createRecord(ctx, dst, action.record); err != nil {
				_ = finalizeImportManifest(runManifestPath, "failed", report, err.Error())
				return report, err
			}
			report.Created++
		default:
			report.Skipped++
			if action.record.ID != "" {
				report.SkippedIDs = append(report.SkippedIDs, action.record.ID)
			}
		}
	}
	_ = finalizeImportManifest(runManifestPath, "applied", report, "")
	return report, nil
}

func lockImport(ctx context.Context, dst Source) (func(), error) {
	type syncLocker interface {
		LockSync(context.Context) (func(), error)
	}
	if locker, ok := any(dst).(syncLocker); ok {
		return locker.LockSync(ctx)
	}
	return func() {}, nil
}

func planImport(ctx context.Context, dst Source, opts ImportOptions, stagedDir string, projectEntries []ProjectEntry, projectMap map[string]ProjectEntry, existingProjects map[string]bool) ([]importAction, error) {
	actions := make([]importAction, 0, len(projectEntries))
	knownProjects := make(map[string]bool, len(existingProjects))
	for id := range existingProjects {
		knownProjects[id] = true
	}
	for _, pe := range projectEntries {
		if knownProjects[pe.ID] || !opts.CreateProjects {
			continue
		}
		proj := domain.Project{
			ID:             pe.ID,
			Name:           pe.Name,
			Paths:          pe.Paths,
			Aliases:        pe.Aliases,
			Kind:           pe.Kind,
			RepoURL:        pe.RepoURL,
			DefaultBranch:  pe.DefaultBranch,
			PathProvenance: pe.PathProvenance,
			CreatedAt:      pe.CreatedAt,
			UpdatedAt:      pe.UpdatedAt,
		}
		if proj.Name == "" {
			proj.Name = proj.ID
		}
		if proj.Kind == "" {
			proj.Kind = domain.ProjectKindOther
		}
		actions = append(actions, importAction{kind: "project", status: "create", proj: proj})
		knownProjects[pe.ID] = true
	}

	records := readRecordEntries(filepath.Join(stagedDir, "records.jsonl"))
	sort.Slice(records, func(i, j int) bool {
		if records[i].ProjectID != records[j].ProjectID {
			return records[i].ProjectID < records[j].ProjectID
		}
		if records[i].Kind != records[j].Kind {
			return records[i].Kind < records[j].Kind
		}
		return records[i].ID < records[j].ID
	})

	for _, re := range records {
		if !knownProjects[re.ProjectID] {
			if opts.CreateProjects {
				pe, ok := projectMap[re.ProjectID]
				name := re.ProjectID
				kind := domain.ProjectKindOther
				if ok {
					if pe.Name != "" {
						name = pe.Name
					}
					if pe.Kind != "" {
						kind = pe.Kind
					}
				}
				proj := domain.Project{ID: re.ProjectID, Name: name, Kind: kind, PathProvenance: pe.PathProvenance}
				actions = append(actions, importAction{kind: "project", status: "create", proj: proj})
				knownProjects[re.ProjectID] = true
			}
		}
		if !knownProjects[re.ProjectID] {
			actions = append(actions, importAction{kind: "record", status: "quarantined", record: re})
			continue
		}
		if !existingProjects[re.ProjectID] {
			actions = append(actions, importAction{kind: "record", status: "create", record: re})
			continue
		}
		existing, err := findExistingRecord(ctx, dst, re.ProjectID, re.Kind, re.ID)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if contentHashAt(*existing, entryVersion(re.V)) == re.ContentHash {
				actions = append(actions, importAction{kind: "record", status: "unchanged", record: re})
			} else {
				actions = append(actions, importAction{kind: "record", status: "conflict", record: re})
			}
			continue
		}
		actions = append(actions, importAction{kind: "record", status: "create", record: re})
	}
	return actions, nil
}

func summarizePlan(actions []importAction) ImportReport {
	var report ImportReport
	for _, action := range actions {
		switch action.kind {
		case "project":
			if action.status == "create" {
				report.Projects++
			}
		case "record":
			switch action.status {
			case "create":
				report.Created++
			case "unchanged":
				report.Unchanged++
			case "conflict":
				report.Conflicts++
				report.ConflictIDs = append(report.ConflictIDs, action.record.ID)
			case "quarantined":
				report.Quarantined++
			case "skipped":
				report.Skipped++
				if action.record.ID != "" {
					report.SkippedIDs = append(report.SkippedIDs, action.record.ID)
				}
			}
		}
	}
	return report
}

func writeImportManifest(dst Source, from, status string, report ImportReport, errText string) (string, func(), error) {
	type rooter interface{ Root() string }
	root := ""
	if r, ok := any(dst).(rooter); ok {
		root = r.Root()
	}
	if root == "" {
		return "", func() {}, nil
	}
	dir := filepath.Join(root, "backups", "import-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", func() {}, err
	}
	path := filepath.Join(dir, "manifest.json")
	manifest := importManifest{Version: 1, From: from, StartedAt: time.Now().UTC(), Status: status, Report: report, Error: errText}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", func() {}, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", func() {}, err
	}
	return path, func() {}, nil
}

func finalizeImportManifest(path, status string, report ImportReport, errText string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var manifest importManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	manifest.Status = status
	manifest.Report = report
	manifest.Error = errText
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}

// verifyManifestFiles checks every bundled file against the SHA256 the manifest
// recorded for it. A bundle that was edited, truncated or half-copied after
// export is rejected before a single record is planned.
func verifyManifestFiles(dir string, files []FileEntry) error {
	for _, file := range files {
		rel, err := bundleRel(file.Path)
		if err != nil {
			return err
		}
		if strings.TrimSpace(file.SHA256) == "" {
			return fmt.Errorf("exchange: manifest has no sha256 for %s", file.Path)
		}
		raw, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return fmt.Errorf("exchange: manifest lists %s but it cannot be read: %w", file.Path, err)
		}
		if file.Bytes != 0 && len(raw) != file.Bytes {
			return fmt.Errorf("exchange: %s is %d bytes, manifest says %d", file.Path, len(raw), file.Bytes)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, file.SHA256) {
			return fmt.Errorf("exchange: manifest SHA256 mismatch for %s: got %s, want %s", file.Path, got, file.SHA256)
		}
	}
	return nil
}

// bundleRel validates a manifest file path and returns it relative to the
// bundle directory.
func bundleRel(name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || strings.ContainsAny(name, `\:`) {
		return "", fmt.Errorf("exchange: unsafe bundle path %q", name)
	}
	clean := path.Clean(name)
	if clean != name || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("exchange: unsafe bundle path %q", name)
	}
	return filepath.FromSlash(clean), nil
}

func readProjectEntries(path string) []ProjectEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []ProjectEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var pe ProjectEntry
		if err := json.Unmarshal([]byte(line), &pe); err != nil {
			continue
		}
		out = append(out, pe)
	}
	return out
}

func readRecordEntries(path string) []RecordEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []RecordEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var re RecordEntry
		if err := json.Unmarshal([]byte(line), &re); err != nil {
			continue
		}
		out = append(out, re)
	}
	return out
}

// findExistingRecord renders one stored record the same way export does, so the
// import can compare content without trusting the bundle's own hash.
func findExistingRecord(ctx context.Context, dst Source, projectID, kind, id string) (*RecordEntry, error) {
	switch kind {
	case "note":
		notes, err := dst.ListNotes(ctx, projectID)
		if err != nil {
			return nil, err
		}
		for _, n := range notes {
			if n.ID == id {
				re := noteToEntry(n)
				return &re, nil
			}
		}
	case "decision":
		decs, err := dst.ListDecisions(ctx, projectID)
		if err != nil {
			return nil, err
		}
		for _, d := range decs {
			if d.ID == id {
				re := decisionToEntry(d)
				return &re, nil
			}
		}
	case "attempt":
		atts, err := dst.ListAttempts(ctx, projectID)
		if err != nil {
			return nil, err
		}
		for _, a := range atts {
			if a.ID == id {
				re := attemptToEntry(a)
				return &re, nil
			}
		}
	case "handoff":
		hs, err := dst.ListHandoffs(ctx, projectID)
		if err != nil {
			return nil, err
		}
		for _, h := range hs {
			if h.ID == id {
				re := handoffToEntry(h)
				return &re, nil
			}
		}
	case "state":
		st, err := dst.GetState(ctx, projectID)
		if err != nil {
			return nil, nil
		}
		re := stateToEntry(st)
		return &re, nil
	}
	return nil, nil
}

func createRecord(ctx context.Context, dst Source, re RecordEntry) error {
	switch re.Kind {
	case "note":
		note := entryToNote(re)
		_, err := dst.AppendNote(ctx, note)
		return err
	case "decision":
		dec := entryToDecision(re)
		_, err := dst.AppendDecision(ctx, dec)
		return err
	case "attempt":
		att := entryToAttempt(re)
		_, err := dst.AppendAttempt(ctx, att)
		return err
	case "handoff":
		h := entryToHandoff(re)
		_, err := dst.AppendHandoff(ctx, h)
		return err
	case "state":
		st := entryToState(re)
		_, err := dst.PutState(ctx, st)
		return err
	}
	return fmt.Errorf("unknown kind %s", re.Kind)
}

func noteToEntry(n domain.Note) RecordEntry {
	re := RecordEntry{
		V:              EntryVersion,
		ProjectID:      n.ProjectID,
		Kind:           "note",
		ID:             n.ID,
		Title:          normalizeLF(n.Title),
		Body:           normalizeLF(n.Body),
		Tags:           n.Tags,
		Permalink:      n.Permalink,
		Source:         n.Source,
		Provenance:     n.Provenance.Normalized(),
		Validity:       n.Validity,
		MergeInfo:      n.MergeInfo,
		Taxonomy:       n.Taxonomy,
		TopicHistory:   n.TopicHistory,
		Relations:      n.Relations,
		ReviewSchedule: n.ReviewSchedule,
		Pinned:         n.Pinned,
		CreatedAt:      n.CreatedAt.UTC(),
		UpdatedAt:      n.UpdatedAt.UTC(),
	}
	re.Key = recordKey(re.ProjectID, re.Kind, re.ID)
	re.ContentHash = contentHashOf(re)
	return re
}

func decisionToEntry(d domain.Decision) RecordEntry {
	re := RecordEntry{
		V:              EntryVersion,
		ProjectID:      d.ProjectID,
		Kind:           "decision",
		ID:             d.ID,
		Title:          normalizeLF(d.Title),
		Status:         d.Status,
		Context:        normalizeLF(d.Context),
		Alternatives:   d.Alternatives,
		Consequences:   d.Consequences,
		Evidence:       d.Evidence,
		Paths:          d.Paths,
		Permalink:      d.Permalink,
		Source:         d.Source,
		Provenance:     d.Provenance.Normalized(),
		Validity:       d.Validity,
		MergeInfo:      d.MergeInfo,
		Taxonomy:       d.Taxonomy,
		TopicHistory:   d.TopicHistory,
		Relations:      d.Relations,
		ReviewSchedule: d.ReviewSchedule,
		Pinned:         d.Pinned,
		CreatedAt:      d.CreatedAt.UTC(),
		UpdatedAt:      d.UpdatedAt.UTC(),
	}
	re.Key = recordKey(re.ProjectID, re.Kind, re.ID)
	re.ContentHash = contentHashOf(re)
	return re
}

func attemptToEntry(a domain.Attempt) RecordEntry {
	re := RecordEntry{
		V:              EntryVersion,
		ProjectID:      a.ProjectID,
		Kind:           "attempt",
		ID:             a.ID,
		Title:          normalizeLF(a.Title),
		Hypothesis:     normalizeLF(a.Hypothesis),
		Result:         normalizeLF(a.Result),
		Reason:         normalizeLF(a.Reason),
		Permalink:      a.Permalink,
		Source:         a.Source,
		Provenance:     a.Provenance.Normalized(),
		Validity:       a.Validity,
		MergeInfo:      a.MergeInfo,
		Taxonomy:       a.Taxonomy,
		TopicHistory:   a.TopicHistory,
		Relations:      a.Relations,
		ReviewSchedule: a.ReviewSchedule,
		Pinned:         a.Pinned,
		CreatedAt:      a.CreatedAt.UTC(),
		UpdatedAt:      a.UpdatedAt.UTC(),
	}
	re.Key = recordKey(re.ProjectID, re.Kind, re.ID)
	re.ContentHash = contentHashOf(re)
	return re
}

func handoffToEntry(h domain.Handoff) RecordEntry {
	re := RecordEntry{
		V:              EntryVersion,
		ProjectID:      h.ProjectID,
		Kind:           "handoff",
		ID:             h.ID,
		Summary:        normalizeLF(h.Summary),
		GitContext:     normalizeLF(h.GitContext),
		NextAction:     normalizeLF(h.NextAction),
		Permalink:      h.Permalink,
		Source:         h.Source,
		Provenance:     h.Provenance.Normalized(),
		Validity:       h.Validity,
		MergeInfo:      h.MergeInfo,
		Taxonomy:       h.Taxonomy,
		TopicHistory:   h.TopicHistory,
		Relations:      h.Relations,
		ReviewSchedule: h.ReviewSchedule,
		Pinned:         h.Pinned,
		CreatedAt:      h.CreatedAt.UTC(),
		UpdatedAt:      h.UpdatedAt.UTC(),
	}
	re.Key = recordKey(re.ProjectID, re.Kind, re.ID)
	re.ContentHash = contentHashOf(re)
	return re
}

func entryToNote(re RecordEntry) domain.Note {
	return domain.Note{
		ID:             re.ID,
		ProjectID:      re.ProjectID,
		Title:          re.Title,
		Body:           re.Body,
		Tags:           re.Tags,
		Permalink:      re.Permalink,
		Source:         re.Source,
		Provenance:     re.Provenance.Normalized(),
		Validity:       re.Validity,
		MergeInfo:      re.MergeInfo,
		Taxonomy:       re.Taxonomy,
		TopicHistory:   re.TopicHistory,
		Relations:      re.Relations,
		ReviewSchedule: re.ReviewSchedule,
		Pinned:         re.Pinned,
		CreatedAt:      re.CreatedAt,
		UpdatedAt:      re.UpdatedAt,
	}
}

func entryToDecision(re RecordEntry) domain.Decision {
	return domain.Decision{
		ID:             re.ID,
		ProjectID:      re.ProjectID,
		Title:          re.Title,
		Status:         re.Status,
		Context:        re.Context,
		Alternatives:   re.Alternatives,
		Consequences:   re.Consequences,
		Evidence:       re.Evidence,
		Paths:          re.Paths,
		Permalink:      re.Permalink,
		Source:         re.Source,
		Provenance:     re.Provenance.Normalized(),
		Validity:       re.Validity,
		MergeInfo:      re.MergeInfo,
		Taxonomy:       re.Taxonomy,
		TopicHistory:   re.TopicHistory,
		Relations:      re.Relations,
		ReviewSchedule: re.ReviewSchedule,
		Pinned:         re.Pinned,
		CreatedAt:      re.CreatedAt,
		UpdatedAt:      re.UpdatedAt,
	}
}

func entryToAttempt(re RecordEntry) domain.Attempt {
	return domain.Attempt{
		ID:             re.ID,
		ProjectID:      re.ProjectID,
		Title:          re.Title,
		Hypothesis:     re.Hypothesis,
		Result:         re.Result,
		Reason:         re.Reason,
		Permalink:      re.Permalink,
		Source:         re.Source,
		Provenance:     re.Provenance.Normalized(),
		Validity:       re.Validity,
		MergeInfo:      re.MergeInfo,
		Taxonomy:       re.Taxonomy,
		TopicHistory:   re.TopicHistory,
		Relations:      re.Relations,
		ReviewSchedule: re.ReviewSchedule,
		Pinned:         re.Pinned,
		CreatedAt:      re.CreatedAt,
		UpdatedAt:      re.UpdatedAt,
	}
}

func entryToHandoff(re RecordEntry) domain.Handoff {
	return domain.Handoff{
		ID:             re.ID,
		ProjectID:      re.ProjectID,
		Summary:        re.Summary,
		GitContext:     re.GitContext,
		NextAction:     re.NextAction,
		Permalink:      re.Permalink,
		Source:         re.Source,
		Provenance:     re.Provenance.Normalized(),
		Validity:       re.Validity,
		MergeInfo:      re.MergeInfo,
		Taxonomy:       re.Taxonomy,
		TopicHistory:   re.TopicHistory,
		Relations:      re.Relations,
		ReviewSchedule: re.ReviewSchedule,
		CreatedAt:      re.CreatedAt,
		UpdatedAt:      re.UpdatedAt,
	}
}

func stateToEntry(st domain.State) RecordEntry {
	re := RecordEntry{
		V:             EntryVersion,
		ProjectID:     st.ProjectID,
		Kind:          "state",
		ID:            st.ProjectID + "-state",
		Goal:          normalizeLF(st.Goal),
		Active:        st.Active,
		Blocked:       st.Blocked,
		NextAction:    normalizeLF(st.NextAction),
		OpenQuestions: st.OpenQuestions,
		Provenance:    st.Provenance.Normalized(),
		UpdatedAt:     st.UpdatedAt.UTC(),
		FreshUntil:    st.FreshUntil.UTC(),
	}
	re.Key = recordKey(re.ProjectID, re.Kind, re.ID)
	re.ContentHash = contentHashOf(re)
	return re
}

func entryToState(re RecordEntry) domain.State {
	return domain.State{
		ProjectID:     re.ProjectID,
		Goal:          re.Goal,
		Active:        re.Active,
		Blocked:       re.Blocked,
		NextAction:    re.NextAction,
		OpenQuestions: re.OpenQuestions,
		Provenance:    re.Provenance.Normalized(),
		UpdatedAt:     re.UpdatedAt,
		FreshUntil:    re.FreshUntil,
	}
}

func unzipTo(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		name := f.Name
		if strings.Contains(name, "..") {
			return fmt.Errorf("invalid zip entry path %s", name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dst)+string(os.PathSeparator)) && filepath.Clean(target) != filepath.Clean(dst) {
			return fmt.Errorf("zip path traversal: %s", name)
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		if f.UncompressedSize64 > 100*1024*1024 {
			rc.Close()
			out.Close()
			return fmt.Errorf("zip entry too large: %s", name)
		}
		limited := io.LimitReader(rc, 100*1024*1024+1)
		n, err := io.Copy(out, limited)
		if err != nil {
			rc.Close()
			out.Close()
			return err
		}
		if n > 100*1024*1024 {
			rc.Close()
			out.Close()
			return fmt.Errorf("zip entry too large: %s", name)
		}
		rc.Close()
		out.Close()
	}
	return nil
}
