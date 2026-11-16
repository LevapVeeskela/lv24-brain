package exchange

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func newTestStore(t *testing.T, dir string) *fsstore.Store {
	t.Helper()
	s, err := fsstore.New(dir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	return s
}

func seedProject(t *testing.T, ctx context.Context, store *fsstore.Store, id string) {
	t.Helper()
	if err := store.PutProject(ctx, domain.Project{ID: id, Name: id, Kind: domain.ProjectKindApp}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	fixed := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newTestStore(t, srcDir)
	seedProject(t, ctx, src, "p1")
	if _, err := src.PutState(ctx, domain.State{
		ProjectID:     "p1",
		Goal:          "goal",
		Active:        []string{"a"},
		Blocked:       []string{"b"},
		NextAction:    "next",
		OpenQuestions: []string{"q"},
		Provenance:    domain.Provenance{Origin: domain.OriginHuman, Reviewed: true, Trust: domain.TrustVerified},
		UpdatedAt:     fixed,
		FreshUntil:    fixed.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	note := domain.Note{
		ID:         "note-foo",
		ProjectID:  "p1",
		Title:      "title",
		Body:       "body line",
		Tags:       []string{"t"},
		Provenance: domain.Provenance{Origin: domain.OriginAgent, Reviewed: false, Trust: domain.TrustInferred},
		Validity:   domain.Validity{Confidence: 0.5},
		Relations:  domain.RelationList{{Target: "note-bar", Relation: domain.RelationRelatesTo}},
		CreatedAt:  fixed,
		UpdatedAt:  fixed,
	}
	if _, err := src.AppendNote(ctx, note); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	// second note as relation target
	note2 := domain.Note{
		ID:        "note-bar",
		ProjectID: "p1",
		Body:      "bar",
		CreatedAt: fixed,
		UpdatedAt: fixed,
	}
	if _, err := src.AppendNote(ctx, note2); err != nil {
		t.Fatalf("AppendNote2: %v", err)
	}
	if _, err := src.AppendDecision(ctx, domain.Decision{
		ID: "decision-x", ProjectID: "p1", Title: "dec", Status: domain.DecisionAccepted, Context: "ctx",
		Provenance: domain.Provenance{Origin: domain.OriginHuman, Reviewed: true, Trust: domain.TrustVerified},
		CreatedAt:  fixed, UpdatedAt: fixed,
	}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	if _, err := src.AppendAttempt(ctx, domain.Attempt{
		ID: "attempt-y", ProjectID: "p1", Title: "att", Hypothesis: "h", Result: "r", Reason: "why",
		CreatedAt: fixed, UpdatedAt: fixed,
	}); err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	if _, err := src.AppendHandoff(ctx, domain.Handoff{
		ID: "handoff-z", ProjectID: "p1", Summary: "sum", NextAction: "na", GitContext: "gc",
		CreatedAt: fixed, UpdatedAt: fixed,
	}); err != nil {
		t.Fatalf("AppendHandoff: %v", err)
	}

	exportDir := filepath.Join(t.TempDir(), "export")
	manifest, err := Export(ctx, src, ExportOptions{Out: exportDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if manifest.Counts.Records != 6 {
		t.Fatalf("records=%d want 6", manifest.Counts.Records)
	}
	if manifest.Counts.Projects != 1 {
		t.Fatalf("projects=%d want 1", manifest.Counts.Projects)
	}

	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newTestStore(t, dstDir)
	report, err := Import(ctx, dst, ImportOptions{From: exportDir, CreateProjects: true})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Created != 6 || report.Unchanged != 0 || report.Conflicts != 0 || report.Quarantined != 0 {
		t.Fatalf("report=%+v", report)
	}
	if report.Projects != 1 {
		t.Fatalf("projects imported=%d want 1", report.Projects)
	}

	// verify note preserved
	notes, err := dst.ListNotes(ctx, "p1")
	if err != nil || len(notes) != 2 {
		t.Fatalf("notes=%d err=%v", len(notes), err)
	}
	var found *domain.Note
	for i := range notes {
		if notes[i].ID == "note-foo" {
			found = &notes[i]
		}
	}
	if found == nil || found.Body != "body line" || found.Title != "title" {
		t.Fatalf("note not preserved: %+v", found)
	}
	if found.Provenance.Origin != domain.OriginAgent || found.Provenance.Reviewed {
		t.Fatalf("provenance not preserved: %+v", found.Provenance)
	}
	if found.Provenance.Trust != domain.TrustInferred {
		t.Fatalf("trust not preserved: %v", found.Provenance.Trust)
	}
	if len(found.Relations) != 1 || found.Relations[0].Target != "note-bar" {
		t.Fatalf("relations not preserved: %+v", found.Relations)
	}
	if found.Validity.Confidence != 0.5 {
		t.Fatalf("validity not preserved: %+v", found.Validity)
	}

	st, err := dst.GetState(ctx, "p1")
	if err != nil || st.Goal != "goal" || len(st.Active) != 1 {
		t.Fatalf("state not preserved: %+v %v", st, err)
	}

	// idempotency
	report2, err := Import(ctx, dst, ImportOptions{From: exportDir, CreateProjects: true})
	if err != nil {
		t.Fatalf("Import2: %v", err)
	}
	if report2.Created != 0 || report2.Unchanged != 6 || report2.Conflicts != 0 {
		t.Fatalf("idempotency report=%+v", report2)
	}

	// re-export byte-identical content (except manifest createdAt)
	exportDir2 := filepath.Join(t.TempDir(), "export2")
	if _, err := Export(ctx, dst, ExportOptions{Out: exportDir2}); err != nil {
		t.Fatalf("Export2: %v", err)
	}
	for _, f := range []string{"records.jsonl", "projects.jsonl", "tombstones.jsonl"} {
		a, _ := os.ReadFile(filepath.Join(exportDir, f))
		b, _ := os.ReadFile(filepath.Join(exportDir2, f))
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs", f)
		}
	}
	// manifest compare ignoring createdAt
	mb1, _ := os.ReadFile(filepath.Join(exportDir, "manifest.json"))
	mb2, _ := os.ReadFile(filepath.Join(exportDir2, "manifest.json"))
	var m1, m2 Manifest
	json.Unmarshal(mb1, &m1)
	json.Unmarshal(mb2, &m2)
	m1.CreatedAt = ""
	m2.CreatedAt = ""
	if !bytes.Equal(mustJSON(m1), mustJSON(m2)) {
		t.Errorf("manifest differs after normalizing createdAt")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestZipRoundTrip(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newTestStore(t, srcDir)
	seedProject(t, ctx, src, "p1")
	if _, err := src.AppendNote(ctx, domain.Note{ID: "note-a", ProjectID: "p1", Body: "body", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	zipPath := filepath.Join(t.TempDir(), "bundle.zip")
	if _, err := Export(ctx, src, ExportOptions{Out: zipPath}); err != nil {
		t.Fatalf("Export zip: %v", err)
	}
	if !strings.HasSuffix(zipPath, ".zip") {
		t.Fatal("bad")
	}
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newTestStore(t, dstDir)
	report, err := Import(ctx, dst, ImportOptions{From: zipPath, CreateProjects: true})
	if err != nil {
		t.Fatalf("Import zip: %v", err)
	}
	if report.Created != 1 {
		t.Fatalf("created=%d want 1", report.Created)
	}
	notes, _ := dst.ListNotes(ctx, "p1")
	if len(notes) != 1 || notes[0].Body != "body" {
		t.Fatalf("note not imported: %+v", notes)
	}
}

func TestCorruptZip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.zip")
	os.WriteFile(path, []byte("not a zip"), 0644)
	ctx := context.Background()
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newTestStore(t, dstDir)
	_, err := Import(ctx, dst, ImportOptions{From: path})
	if err == nil {
		t.Fatal("expected error for corrupt zip")
	}
}

func TestZipPathTraversal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evil.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	fw, _ := w.Create("../evil.txt")
	fw.Write([]byte("x"))
	w.Close()
	f.Close()

	ctx := context.Background()
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newTestStore(t, dstDir)
	_, err = Import(ctx, dst, ImportOptions{From: path})
	if err == nil {
		t.Fatal("expected path traversal error")
	}
	if !strings.Contains(err.Error(), "traversal") && !strings.Contains(err.Error(), "invalid zip") && !strings.Contains(err.Error(), "..") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestZipOversizedGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.zip")
	// build a valid small zip then tamper uncompressed size in central directory
	f, _ := os.Create(path)
	w := zip.NewWriter(f)
	fw, _ := w.Create("big.txt")
	fw.Write([]byte("hello"))
	w.Close()
	f.Close()
	data, _ := os.ReadFile(path)
	// find central directory signature and bump uncompressed size field
	sig := []byte{0x50, 0x4b, 0x01, 0x02}
	idx := bytes.Index(data, sig)
	if idx < 0 {
		t.Fatal("no central dir")
	}
	// uncompressed size is 4 bytes at offset 24 from signature
	off := idx + 24
	// set to 100MB+1
	val := uint32(100*1024*1024 + 1)
	data[off] = byte(val)
	data[off+1] = byte(val >> 8)
	data[off+2] = byte(val >> 16)
	data[off+3] = byte(val >> 24)
	os.WriteFile(path, data, 0644)

	ctx := context.Background()
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newTestStore(t, dstDir)
	_, err := Import(ctx, dst, ImportOptions{From: path})
	if err == nil {
		t.Fatal("expected oversized error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "too large") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestImportIdempotencyAndDryRun(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newTestStore(t, srcDir)
	seedProject(t, ctx, src, "p1")
	if _, err := src.AppendNote(ctx, domain.Note{ID: "note-a", ProjectID: "p1", Body: "body", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	exportDir := filepath.Join(t.TempDir(), "export")
	Export(ctx, src, ExportOptions{Out: exportDir})

	// dry run writes nothing
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newTestStore(t, dstDir)
	seedProject(t, ctx, dst, "p1")
	r, err := Import(ctx, dst, ImportOptions{From: exportDir, DryRun: true})
	if err != nil {
		t.Fatalf("dryrun err: %v", err)
	}
	if r.Created != 1 {
		t.Fatalf("dryrun created=%d want 1", r.Created)
	}
	notes, _ := dst.ListNotes(ctx, "p1")
	if len(notes) != 0 {
		t.Fatalf("dry run wrote %d notes", len(notes))
	}
}

func TestConflictNoOverwrite(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newTestStore(t, srcDir)
	seedProject(t, ctx, src, "p1")
	if _, err := src.AppendNote(ctx, domain.Note{ID: "note-a", ProjectID: "p1", Body: "original", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	exportDir := filepath.Join(t.TempDir(), "export")
	Export(ctx, src, ExportOptions{Out: exportDir})

	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newTestStore(t, dstDir)
	if _, err := Import(ctx, dst, ImportOptions{From: exportDir, CreateProjects: true}); err != nil {
		t.Fatal(err)
	}
	// mutate bundle body and recompute its content hash
	data, _ := os.ReadFile(filepath.Join(exportDir, "records.jsonl"))
	var rec RecordEntry
	json.Unmarshal(bytes.TrimSpace(data), &rec) // single record line
	rec.Body = "changed"
	rec.ContentHash = contentHashOf(rec)
	out, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(exportDir, "records.jsonl"), append(out, '\n'), 0644)

	report, err := Import(ctx, dst, ImportOptions{From: exportDir, CreateProjects: true})
	if err != nil {
		t.Fatalf("import err: %v", err)
	}
	if report.Conflicts != 1 {
		t.Fatalf("conflicts=%d want 1", report.Conflicts)
	}
	notes, _ := dst.ListNotes(ctx, "p1")
	if len(notes) != 1 || notes[0].Body != "original" {
		t.Fatalf("overwrite happened or body wrong: %+v", notes)
	}
}

func TestMissingProjectCreateFlag(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newTestStore(t, srcDir)
	seedProject(t, ctx, src, "p1")
	if _, err := src.AppendNote(ctx, domain.Note{ID: "note-a", ProjectID: "p1", Body: "body", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	exportDir := filepath.Join(t.TempDir(), "export")
	Export(ctx, src, ExportOptions{Out: exportDir})

	// without create-projects: quarantined
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newTestStore(t, dstDir)
	r, err := Import(ctx, dst, ImportOptions{From: exportDir, CreateProjects: false})
	if err != nil {
		t.Fatal(err)
	}
	if r.Quarantined != 1 || r.Created != 0 {
		t.Fatalf("without create: report=%+v", r)
	}
	notes, _ := dst.ListNotes(ctx, "p1")
	if len(notes) != 0 {
		t.Fatalf("unexpected notes without create")
	}

	// with create-projects: created
	dstDir2 := filepath.Join(t.TempDir(), "dst2")
	dst2 := newTestStore(t, dstDir2)
	r2, err := Import(ctx, dst2, ImportOptions{From: exportDir, CreateProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Created != 1 || r2.Quarantined != 0 {
		t.Fatalf("with create: report=%+v", r2)
	}
	if _, err := dst2.GetProject(ctx, "p1"); err != nil {
		t.Fatalf("project not created: %v", err)
	}
}

func TestProjectsJSONLRoundTrip(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newTestStore(t, srcDir)
	if err := src.PutProject(ctx, domain.Project{ID: "p1", Name: "P1", Kind: domain.ProjectKindApp, Aliases: []string{"a"}, Paths: []string{"/tmp"}, RepoURL: "u", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	exportDir := filepath.Join(t.TempDir(), "export")
	Export(ctx, src, ExportOptions{Out: exportDir})

	// verify projects.jsonl exists and round trips
	data, err := os.ReadFile(filepath.Join(exportDir, "projects.jsonl"))
	if err != nil || len(data) == 0 {
		t.Fatalf("projects.jsonl missing: %v", err)
	}
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newTestStore(t, dstDir)
	r, err := Import(ctx, dst, ImportOptions{From: exportDir, CreateProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Projects != 1 {
		t.Fatalf("projects=%d want 1", r.Projects)
	}
	p, err := dst.GetProject(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "P1" || p.DefaultBranch != "main" || len(p.Aliases) != 1 {
		t.Fatalf("project not round-tripped: %+v", p)
	}
}

func TestManifestChecksums(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newTestStore(t, srcDir)
	seedProject(t, ctx, src, "p1")
	if _, err := src.AppendNote(ctx, domain.Note{ID: "note-a", ProjectID: "p1", Body: "body", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	exportDir := filepath.Join(t.TempDir(), "export")
	Export(ctx, src, ExportOptions{Out: exportDir})

	mb, _ := os.ReadFile(filepath.Join(exportDir, "manifest.json"))
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	if m.Format != FormatName || m.FormatVersion != FormatVersion {
		t.Fatalf("manifest format: %+v", m)
	}
	if m.Counts.Records != 1 || m.Counts.Projects != 1 {
		t.Fatalf("counts: %+v", m.Counts)
	}
	// verify each file checksum
	for _, fe := range m.Files {
		b, err := os.ReadFile(filepath.Join(exportDir, fe.Path))
		if err != nil {
			t.Fatalf("read %s: %v", fe.Path, err)
		}
		h := sha256Hex(b)
		if h != fe.SHA256 {
			t.Errorf("checksum mismatch %s", fe.Path)
		}
		if len(b) != fe.Bytes {
			t.Errorf("bytes mismatch %s", fe.Path)
		}
		if fe.Entries != strings.Count(string(b), "\n") {
			t.Errorf("entries mismatch %s", fe.Path)
		}
	}
	// validate manifest version function
	if err := ValidateManifestVersion(m.FormatVersion); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := ValidateManifestVersion("2.0"); err == nil {
		t.Fatal("expected unsupported version error")
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestStreaming1kRecords(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newTestStore(t, srcDir)
	seedProject(t, ctx, src, "p1")
	now := time.Now().UTC()
	for i := 0; i < 1000; i++ {
		if _, err := src.AppendNote(ctx, domain.Note{
			ID:        fmt.Sprintf("note-%04d", i),
			ProjectID: "p1",
			Body:      fmt.Sprintf("body %d", i),
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	exportDir := filepath.Join(t.TempDir(), "export")
	manifest, err := Export(ctx, src, ExportOptions{Out: exportDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if manifest.Counts.Records != 1000 {
		t.Fatalf("records=%d want 1000", manifest.Counts.Records)
	}
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newTestStore(t, dstDir)
	report, err := Import(ctx, dst, ImportOptions{From: exportDir, CreateProjects: true})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Created != 1000 {
		t.Fatalf("created=%d want 1000", report.Created)
	}
	notes, _ := dst.ListNotes(ctx, "p1")
	if len(notes) != 1000 {
		t.Fatalf("imported notes=%d", len(notes))
	}
}
