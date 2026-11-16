package exchange

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

func importBundle(ctx context.Context, dst Source, opts ImportOptions) (ImportReport, error) {
	from := strings.TrimSpace(opts.From)
	if from == "" {
		return ImportReport{}, fmt.Errorf("exchange: --from is required")
	}
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

	// read manifest
	manifestPath := filepath.Join(dir, "manifest.json")
	manifestBytes, _ := os.ReadFile(manifestPath)
	if len(manifestBytes) > 0 {
		var m Manifest
		if err := json.Unmarshal(manifestBytes, &m); err != nil {
			return report, fmt.Errorf("invalid manifest: %w", err)
		}
		if err := ValidateManifestVersion(m.FormatVersion); err != nil {
			// maybe version field
			if m.Version != "" {
				if err2 := ValidateManifestVersion(m.Version); err2 != nil {
					return report, err
				}
			} else {
				return report, err
			}
		}
	}

	// projects
	projectEntries := readProjectEntries(filepath.Join(dir, "projects.jsonl"))
	// map by id
	projectMap := map[string]ProjectEntry{}
	for _, pe := range projectEntries {
		projectMap[pe.ID] = pe
	}

	// ensure projects
	existingProjects, _ := dst.ListProjects(ctx)
	existingProjectSet := map[string]bool{}
	for _, p := range existingProjects {
		existingProjectSet[p.ID] = true
	}

	// import projects
	for _, pe := range projectEntries {
		if existingProjectSet[pe.ID] {
			continue
		}
		if !opts.CreateProjects {
			continue
		}
		proj := domain.Project{
			ID:            pe.ID,
			Name:          pe.Name,
			Paths:         pe.Paths,
			Aliases:       pe.Aliases,
			Kind:          pe.Kind,
			RepoURL:       pe.RepoURL,
			DefaultBranch: pe.DefaultBranch,
			CreatedAt:     pe.CreatedAt,
			UpdatedAt:     pe.UpdatedAt,
		}
		if proj.Name == "" {
			proj.Name = proj.ID
		}
		if proj.Kind == "" {
			proj.Kind = domain.ProjectKindOther
		}
		if err := dst.PutProject(ctx, proj); err != nil {
			continue
		}
		existingProjectSet[pe.ID] = true
		report.Projects++
	}

	// records
	records := readRecordEntries(filepath.Join(dir, "records.jsonl"))
	// sort for determinism
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
		// project existence
		if !existingProjectSet[re.ProjectID] {
			if opts.CreateProjects {
				// create minimal project
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
				if err := dst.PutProject(ctx, domain.Project{ID: re.ProjectID, Name: name, Kind: kind}); err == nil {
					existingProjectSet[re.ProjectID] = true
					report.Projects++
				}
			}
		}
		if !existingProjectSet[re.ProjectID] {
			report.Quarantined++
			continue
		}
		if opts.DryRun {
			// still classify? We'll count as created if not exists
			existing, err := findExistingRecord(ctx, dst, re.ProjectID, re.Kind, re.ID)
			if err != nil || existing == nil {
				report.Created++
			} else {
				if existing.ContentHash == re.ContentHash {
					report.Unchanged++
				} else {
					report.Conflicts++
					report.ConflictIDs = append(report.ConflictIDs, re.ID)
				}
			}
			continue
		}

		// find existing
		existing, err := findExistingRecord(ctx, dst, re.ProjectID, re.Kind, re.ID)
		if err == nil && existing != nil {
			if existing.ContentHash == re.ContentHash {
				report.Unchanged++
				continue
			}
			report.Conflicts++
			report.ConflictIDs = append(report.ConflictIDs, re.ID)
			continue
		}
		// create
		if err := createRecord(ctx, dst, re); err != nil {
			report.Quarantined++
			continue
		}
		report.Created++
	}
	return report, nil
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

type existingRecord struct {
	ContentHash string
}

func findExistingRecord(ctx context.Context, dst Source, projectID, kind, id string) (*existingRecord, error) {
	// list records of kind and find id
	switch kind {
	case "note":
		notes, err := dst.ListNotes(ctx, projectID)
		if err != nil {
			return nil, err
		}
		for _, n := range notes {
			if n.ID == id {
				re := noteToEntry(n)
				return &existingRecord{ContentHash: re.ContentHash}, nil
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
				return &existingRecord{ContentHash: re.ContentHash}, nil
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
				return &existingRecord{ContentHash: re.ContentHash}, nil
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
				return &existingRecord{ContentHash: re.ContentHash}, nil
			}
		}
	case "state":
		st, err := dst.GetState(ctx, projectID)
		if err != nil {
			return nil, nil
		}
		rex := stateToEntry(st)
		return &existingRecord{ContentHash: rex.ContentHash}, nil
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

// conversion helpers
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
		// guard path traversal
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
		// zip bomb guard: limit size
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
