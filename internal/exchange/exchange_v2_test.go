package exchange

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// hundredRecordStore seeds one project with one state and 99 curated records on
// distinct days, so each record has its own file and its own updated_at.
func hundredRecordStore(t *testing.T) *fsstore.Store {
	t.Helper()
	ctx := context.Background()
	store := newTestStore(t, filepath.Join(t.TempDir(), "src"))
	if err := store.PutProject(ctx, domain.Project{
		ID: "p1", Name: "P one", Kind: domain.ProjectKindApp,
		Paths: []string{"/work/p1"}, Aliases: []string{"one"},
		RepoURL: "https://example.invalid/p1", DefaultBranch: "main",
		PathProvenance: map[string]string{"/work/p1": "MACHINE-A"},
	}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	if _, err := store.PutState(ctx, domain.State{
		ProjectID: "p1", Goal: "100 records", Active: []string{"a"}, NextAction: "ship",
		UpdatedAt: base, FreshUntil: base.Add(48 * time.Hour),
	}); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	for i := 0; i < 33; i++ {
		at := base.Add(time.Duration(i) * 24 * time.Hour)
		if _, err := store.AppendNote(ctx, domain.Note{
			ID: fmt.Sprintf("note-%02d", i), ProjectID: "p1",
			Title: fmt.Sprintf("Note %02d", i), Body: fmt.Sprintf("note body %02d", i),
			Tags: []string{"bulk"}, Pinned: i%3 == 0,
			CreatedAt: at, UpdatedAt: at.Add(time.Hour),
		}); err != nil {
			t.Fatalf("AppendNote %d: %v", i, err)
		}
		if _, err := store.AppendDecision(ctx, domain.Decision{
			ID: fmt.Sprintf("decision-%02d", i), ProjectID: "p1",
			Title: fmt.Sprintf("Decision %02d", i), Status: domain.DecisionAccepted,
			Context: fmt.Sprintf("context %02d", i), Pinned: i%4 == 0,
			CreatedAt: at.Add(2 * time.Hour), UpdatedAt: at.Add(2 * time.Hour),
		}); err != nil {
			t.Fatalf("AppendDecision %d: %v", i, err)
		}
		if _, err := store.AppendAttempt(ctx, domain.Attempt{
			ID: fmt.Sprintf("attempt-%02d", i), ProjectID: "p1",
			Title: fmt.Sprintf("Attempt %02d", i), Hypothesis: "h", Result: "r", Reason: "why",
			CreatedAt: at.Add(3 * time.Hour), UpdatedAt: at.Add(3 * time.Hour),
		}); err != nil {
			t.Fatalf("AppendAttempt %d: %v", i, err)
		}
	}
	return store
}

func readBundleFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range []string{"records.jsonl", "projects.jsonl", "tombstones.jsonl"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		files[name] = raw
	}
	return files
}

func TestExportImportExportIsByteIdenticalFor100Records(t *testing.T) {
	ctx := context.Background()
	src := hundredRecordStore(t)
	first := filepath.Join(t.TempDir(), "export1")
	manifest, err := Export(ctx, src, ExportOptions{Out: first})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if manifest.Counts.Records != 100 {
		t.Fatalf("records=%d want 100", manifest.Counts.Records)
	}
	if manifest.FormatVersion != "2.0" || manifest.Version != "2.0" {
		t.Fatalf("manifest version = %q/%q, want 2.0", manifest.FormatVersion, manifest.Version)
	}
	projectsRaw, err := os.ReadFile(filepath.Join(first, "projects.jsonl"))
	if err != nil {
		t.Fatalf("read projects.jsonl: %v", err)
	}
	var project ProjectEntry
	if err := json.Unmarshal(bytes.TrimSpace(projectsRaw), &project); err != nil {
		t.Fatalf("parse project line: %v", err)
	}
	if project.PathProvenance["/work/p1"] != "MACHINE-A" {
		t.Fatalf("path_provenance not exported: %+v", project.PathProvenance)
	}
	if !strings.Contains(string(projectsRaw), `"pinned":false`) {
		t.Fatalf("projects.jsonl does not carry pinned: %s", projectsRaw)
	}

	dst := newTestStore(t, filepath.Join(t.TempDir(), "dst"))
	report, err := Import(ctx, dst, ImportOptions{From: first, CreateProjects: true})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Created != 100 || report.Conflicts != 0 || report.Projects != 1 {
		t.Fatalf("import report = %+v", report)
	}
	imported, err := dst.GetProject(ctx, "p1")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if imported.PathProvenance["/work/p1"] != "MACHINE-A" {
		t.Fatalf("path_provenance not imported: %+v", imported.PathProvenance)
	}

	second := filepath.Join(t.TempDir(), "export2")
	if _, err := Export(ctx, dst, ExportOptions{Out: second}); err != nil {
		t.Fatalf("Export2: %v", err)
	}
	before := readBundleFiles(t, first)
	after := readBundleFiles(t, second)
	for name := range before {
		if !bytes.Equal(before[name], after[name]) {
			t.Fatalf("%s is not byte-identical after export -> import -> export", name)
		}
	}

	// Stable ids: the same records carry the same ids on both sides.
	notes, err := dst.ListNotes(ctx, "p1")
	if err != nil || len(notes) != 33 {
		t.Fatalf("notes=%d err=%v", len(notes), err)
	}
	ids := map[string]bool{}
	for _, note := range notes {
		ids[note.ID] = true
	}
	if !ids["note-00"] || !ids["note-32"] {
		t.Fatalf("note ids changed: %+v", ids)
	}

	// A rerun of the same v2 bundle is unchanged, pin state included.
	if rerun, err := Import(ctx, dst, ImportOptions{From: first, CreateProjects: true}); err != nil || rerun.Unchanged != 100 || rerun.Conflicts != 0 {
		t.Fatalf("rerun report = %+v err=%v", rerun, err)
	}
}

func TestV1BundleStillImportsAndStaysIdempotent(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t, filepath.Join(t.TempDir(), "src"))
	seedProject(t, ctx, src, "p1")
	at := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	if _, err := src.AppendNote(ctx, domain.Note{ID: "note-a", ProjectID: "p1", Title: "A", Body: "a", CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.AppendDecision(ctx, domain.Decision{ID: "decision-a", ProjectID: "p1", Title: "D", Context: "c", CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "v1")
	if _, err := Export(ctx, src, ExportOptions{Out: dir}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := downgradeBundleToV1(t, dir); err != nil {
		t.Fatalf("downgrade: %v", err)
	}

	dst := newTestStore(t, filepath.Join(t.TempDir(), "dst"))
	first, err := Import(ctx, dst, ImportOptions{From: dir, CreateProjects: true})
	if err != nil {
		t.Fatalf("v1 import: %v", err)
	}
	if first.Created != 2 || first.Conflicts != 0 {
		t.Fatalf("v1 import report = %+v", first)
	}
	second, err := Import(ctx, dst, ImportOptions{From: dir, CreateProjects: true})
	if err != nil {
		t.Fatalf("v1 re-import: %v", err)
	}
	if second.Created != 0 || second.Unchanged != 2 || second.Conflicts != 0 {
		t.Fatalf("v1 re-import report = %+v (pin state must stay out of a v1 hash)", second)
	}
}

// downgradeBundleToV1 rewrites an exported bundle into the v1 shape: entry
// version 1, no pinned key, and content hashes recomputed the v1 way.
func downgradeBundleToV1(t *testing.T, dir string) error {
	t.Helper()
	recordsPath := filepath.Join(dir, "records.jsonl")
	raw, err := os.ReadFile(recordsPath)
	if err != nil {
		return err
	}
	lines := []string{}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry RecordEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return err
		}
		entry.V = 1
		entry.Pinned = false
		entry.ContentHash = contentHashAt(entry, 1)
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		lines = append(lines, string(encoded))
	}
	out := []byte(strings.Join(lines, "\n") + "\n")
	if err := os.WriteFile(recordsPath, out, 0o644); err != nil {
		return err
	}
	projectsPath := filepath.Join(dir, "projects.jsonl")
	projectRaw, err := os.ReadFile(projectsPath)
	if err != nil {
		return err
	}
	var project ProjectEntry
	if err := json.Unmarshal(bytes.TrimSpace(projectRaw), &project); err != nil {
		return err
	}
	project.V = 1
	project.Pinned = false
	project.PathProvenance = nil
	encoded, err := json.Marshal(project)
	if err != nil {
		return err
	}
	if err := os.WriteFile(projectsPath, append(encoded, '\n'), 0o644); err != nil {
		return err
	}

	manifestPath := filepath.Join(dir, "manifest.json")
	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return err
	}
	manifest.FormatVersion = "1.0"
	manifest.Version = "1.0"
	for i := range manifest.Files {
		fileRaw, err := os.ReadFile(filepath.Join(dir, manifest.Files[i].Path))
		if err != nil {
			return err
		}
		manifest.Files[i].Bytes = len(fileRaw)
		manifest.Files[i].SHA256 = sha256Hex(fileRaw)
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, append(manifestBytes, '\n'), 0o644)
}

func TestImportRejectsManifestChecksumMismatch(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t, filepath.Join(t.TempDir(), "src"))
	seedProject(t, ctx, src, "p1")
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	if _, err := src.AppendNote(ctx, domain.Note{ID: "note-a", ProjectID: "p1", Body: "body", CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(ctx, src, ExportOptions{Out: dir}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	recordsPath := filepath.Join(dir, "records.jsonl")
	raw, err := os.ReadFile(recordsPath)
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	raw[len(raw)-2] ^= 0x01 // same length, different bytes
	if err := os.WriteFile(recordsPath, raw, 0o644); err != nil {
		t.Fatalf("tamper records: %v", err)
	}

	dst := newTestStore(t, filepath.Join(t.TempDir(), "dst"))
	_, err = Import(ctx, dst, ImportOptions{From: dir, CreateProjects: true})
	if err == nil || !strings.Contains(err.Error(), "manifest SHA256 mismatch for records.jsonl") {
		t.Fatalf("import error = %v, want a manifest SHA256 mismatch", err)
	}
	notes, _ := dst.ListNotes(ctx, "p1")
	if len(notes) != 0 {
		t.Fatalf("a rejected bundle imported %d notes", len(notes))
	}
}
