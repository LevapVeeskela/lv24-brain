package exchange

import (
	"archive/zip"
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
)

// Export writes a bundle to opts.Out (directory or .zip).
func Export(ctx context.Context, src Source, opts ExportOptions) (Manifest, error) {
	return exportBundle(ctx, src, opts)
}

// Aliases
func ExportBundle(ctx context.Context, src Source, opts ExportOptions) (Manifest, error) {
	return exportBundle(ctx, src, opts)
}
func RunExport(ctx context.Context, src Source, opts ExportOptions) (Manifest, error) {
	return exportBundle(ctx, src, opts)
}
func ExportToDir(ctx context.Context, src Source, opts ExportOptions) (Manifest, error) {
	return exportBundle(ctx, src, opts)
}

func exportBundle(ctx context.Context, src Source, opts ExportOptions) (Manifest, error) {
	out := strings.TrimSpace(opts.Out)
	if out == "" {
		return Manifest{}, fmt.Errorf("exchange: --out is required")
	}
	projects, err := src.ListProjects(ctx)
	if err != nil {
		return Manifest{}, err
	}
	// filter project
	if opts.Project != "" {
		filtered := []domain.Project{}
		for _, p := range projects {
			if p.ID == opts.Project {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}
	// sort projects by id
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })

	var records []RecordEntry
	for _, p := range projects {
		// state
		st, err := src.GetState(ctx, p.ID)
		if err == nil {
			// only include if state has any content or updated
			re := RecordEntry{
				V:             EntryVersion,
				ProjectID:     p.ID,
				Kind:          "state",
				ID:            p.ID + "-state",
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
			if !opts.Since.IsZero() && re.UpdatedAt.Before(opts.Since) {
				// skip
			} else {
				records = append(records, re)
			}
		}
		// notes
		notes, _ := src.ListNotes(ctx, p.ID)
		for _, n := range notes {
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
				Provenance:     n.Provenance,
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
			if opts.Since.IsZero() || !re.UpdatedAt.Before(opts.Since) {
				records = append(records, re)
			}
		}
		decisions, _ := src.ListDecisions(ctx, p.ID)
		for _, d := range decisions {
			re := RecordEntry{
				V:              EntryVersion,
				ProjectID:      d.ProjectID,
				Kind:           "decision",
				ID:             d.ID,
				Title:          normalizeLF(d.Title),
				Body:           "",
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
			if opts.Since.IsZero() || !re.UpdatedAt.Before(opts.Since) {
				records = append(records, re)
			}
		}
		attempts, _ := src.ListAttempts(ctx, p.ID)
		for _, a := range attempts {
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
			if opts.Since.IsZero() || !re.UpdatedAt.Before(opts.Since) {
				records = append(records, re)
			}
		}
		handoffs, _ := src.ListHandoffs(ctx, p.ID)
		for _, h := range handoffs {
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
			if opts.Since.IsZero() || !re.UpdatedAt.Before(opts.Since) {
				records = append(records, re)
			}
		}
	}
	// sort records by project, kind, id
	sort.Slice(records, func(i, j int) bool {
		if records[i].ProjectID != records[j].ProjectID {
			return records[i].ProjectID < records[j].ProjectID
		}
		if records[i].Kind != records[j].Kind {
			return records[i].Kind < records[j].Kind
		}
		return records[i].ID < records[j].ID
	})

	// projects entries
	projectEntries := make([]ProjectEntry, 0, len(projects))
	for _, p := range projects {
		pe := ProjectEntry{
			V:              EntryVersion,
			ID:             p.ID,
			Name:           p.Name,
			Paths:          p.Paths,
			Aliases:        p.Aliases,
			Kind:           p.Kind,
			RepoURL:        p.RepoURL,
			DefaultBranch:  p.DefaultBranch,
			PathProvenance: p.PathProvenance,
			CreatedAt:      p.CreatedAt.UTC(),
			UpdatedAt:      p.UpdatedAt.UTC(),
		}
		pe.Key = projectKey(pe.ID)
		projectEntries = append(projectEntries, pe)
	}

	// write bundle
	zipMode := strings.HasSuffix(strings.ToLower(out), ".zip")
	var dir string
	if zipMode {
		dir = filepath.Join(os.TempDir(), "lv24-exchange-"+fmt.Sprint(time.Now().UnixNano()))
		if err := os.MkdirAll(dir, 0755); err != nil {
			return Manifest{}, err
		}
		defer os.RemoveAll(dir)
	} else {
		dir = out
		if err := os.MkdirAll(dir, 0755); err != nil {
			return Manifest{}, err
		}
	}

	// write records.jsonl
	if err := writeJSONL(filepath.Join(dir, "records.jsonl"), records); err != nil {
		return Manifest{}, err
	}
	if err := writeJSONL(filepath.Join(dir, "projects.jsonl"), projectEntries); err != nil {
		return Manifest{}, err
	}
	// tombstones empty
	if err := writeFile(filepath.Join(dir, "tombstones.jsonl"), []byte{}); err != nil {
		return Manifest{}, err
	}

	// manifest
	manifest := Manifest{}
	manifest.Format = FormatName
	manifest.FormatVersion = FormatVersion
	manifest.Version = FormatVersion
	manifest.Generator.Name = "lv24-brain"
	manifest.Generator.Version = "0.26.0"
	manifest.Generator.Profile = "engine"
	manifest.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	manifest.Source.System = "lv24-brain"
	manifest.Scope.Projects = []string{"*"}
	if opts.Project != "" {
		manifest.Scope.Projects = []string{opts.Project}
	}
	manifest.Scope.Kinds = []string{"state", "note", "decision", "attempt", "handoff"}
	manifest.Layout.Compression = "deflate"
	manifest.Counts.Projects = len(projectEntries)
	manifest.Counts.Records = len(records)

	// files list
	files := []string{"records.jsonl", "projects.jsonl", "tombstones.jsonl"}
	for _, f := range files {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(dir, f))
		h := sha256.Sum256(b)
		entries := 0
		// count lines
		if len(b) > 0 {
			entries = strings.Count(string(b), "\n")
		}
		manifest.Files = append(manifest.Files, FileEntry{
			Path:    f,
			Bytes:   int(info.Size()),
			SHA256:  hex.EncodeToString(h[:]),
			Entries: entries,
		})
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestBytes, 0644); err != nil {
		return Manifest{}, err
	}

	if zipMode {
		if err := zipDir(dir, out); err != nil {
			return Manifest{}, err
		}
	}
	return manifest, nil
}

func writeJSONL(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	switch val := v.(type) {
	case []RecordEntry:
		for _, e := range val {
			b, err := json.Marshal(e)
			if err != nil {
				return err
			}
			f.Write(b)
			f.Write([]byte("\n"))
		}
	case []ProjectEntry:
		for _, e := range val {
			b, err := json.Marshal(e)
			if err != nil {
				return err
			}
			f.Write(b)
			f.Write([]byte("\n"))
		}
	}
	return nil
}

func writeFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0644)
}

func zipDir(src, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	w := zip.NewWriter(f)
	defer w.Close()
	err = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		rel = filepath.ToSlash(rel)
		fw, err := w.Create(rel)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = fw.Write(b)
		return err
	})
	return err
}
