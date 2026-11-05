package fsstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestConnectorRunRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	finished := started.Add(90 * time.Second)
	run := domain.ConnectorRun{
		ID: "run-abc", Source: "github", StartedAt: started, FinishedAt: &finished,
		Status: domain.ConnectorPartial, Counts: map[string]int{"repos": 3, "prs": 2},
		Error: "rate limited after 3 repos",
	}
	if err := store.AppendConnectorRun(ctx, run); err != nil {
		t.Fatalf("AppendConnectorRun: %v", err)
	}

	runs, err := store.ListConnectorRuns(ctx, "", 0)
	if err != nil {
		t.Fatalf("ListConnectorRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	got := runs[0]
	if got.ID != run.ID || got.Source != run.Source || got.Status != run.Status || got.Error != run.Error {
		t.Fatalf("run = %+v", got)
	}
	if !got.StartedAt.Equal(started) || got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Fatalf("times = %v -> %v", got.StartedAt, got.FinishedAt)
	}
	if got.Counts["repos"] != 3 || got.Counts["prs"] != 2 {
		t.Fatalf("counts = %+v", got.Counts)
	}

	dir := filepath.Join(store.Root(), connectorsDir, connectorRunsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 || !strings.Contains(entries[0].Name(), "github") {
		t.Fatalf("entries = %v", entries)
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read run file: %v", err)
	}
	if !strings.Contains(string(raw), "kind: connector-run") {
		t.Fatalf("frontmatter missing kind: %s", raw)
	}
	if strings.Contains(string(raw), "\r\n") {
		t.Fatal("run file has CRLF line endings")
	}

	if err := store.AppendConnectorRun(ctx, run); err != nil {
		t.Fatalf("re-append: %v", err)
	}
	entries, err = os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("re-append wrote another file: %v, %v", entries, err)
	}
}

func TestConnectorRunReappendSameIDWithNewStartedAt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	original := domain.ConnectorRun{
		ID: "run-abc", Source: "github", StartedAt: base, Status: domain.ConnectorSuccess,
	}
	if err := store.AppendConnectorRun(ctx, original); err != nil {
		t.Fatalf("AppendConnectorRun: %v", err)
	}
	retry := original
	retry.StartedAt = base.Add(time.Hour)
	retry.Status = domain.ConnectorFailed
	if err := store.AppendConnectorRun(ctx, retry); err != nil {
		t.Fatalf("re-append: %v", err)
	}

	dir := filepath.Join(store.Root(), connectorsDir, connectorRunsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("re-append with new StartedAt wrote %d files", len(entries))
	}
	runs, err := store.ListConnectorRuns(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListConnectorRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != original.ID {
		t.Fatalf("runs = %+v", runs)
	}
	if !runs[0].StartedAt.Equal(retry.StartedAt) || runs[0].Status != domain.ConnectorFailed {
		t.Fatalf("re-appended record = %+v", runs[0])
	}
}

func TestConnectorRunConcurrentSanitizedIDCollisionKeepsBoth(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ids := []string{"run/a", "run@a"}

	start := make(chan struct{})
	errs := make(chan error, len(ids))
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			run := domain.ConnectorRun{ID: id, Source: "github", StartedAt: started, Status: domain.ConnectorSuccess}
			errs <- store.AppendConnectorRun(ctx, run)
		}(id)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AppendConnectorRun: %v", err)
		}
	}

	runs, err := store.ListConnectorRuns(ctx, "github", 10)
	if err != nil {
		t.Fatalf("ListConnectorRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
	seen := map[string]bool{runs[0].ID: true, runs[1].ID: true}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("raw id %q missing from %+v", id, runs)
		}
	}
}

func TestConnectorRunSameIDDifferentSourceKeepsBoth(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	first := domain.ConnectorRun{
		ID: "run-shared", Source: "github", StartedAt: base, Status: domain.ConnectorSuccess,
	}
	second := domain.ConnectorRun{
		ID: "run-shared", Source: "gitlab", StartedAt: base.Add(time.Hour), Status: domain.ConnectorFailed,
	}
	for _, run := range []domain.ConnectorRun{first, second} {
		if err := store.AppendConnectorRun(ctx, run); err != nil {
			t.Fatalf("AppendConnectorRun(%s): %v", run.Source, err)
		}
	}

	dir := filepath.Join(store.Root(), connectorsDir, connectorRunsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("same id across sources wrote %d files: %v", len(entries), entries)
	}
	runs, err := store.ListConnectorRuns(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListConnectorRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
	sources := map[string]domain.ConnectorStatus{runs[0].Source: runs[0].Status, runs[1].Source: runs[1].Status}
	if sources["github"] != domain.ConnectorSuccess || sources["gitlab"] != domain.ConnectorFailed {
		t.Fatalf("records = %+v", runs)
	}
}

func TestConnectorRunSanitizedIDCollisionKeepsBoth(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ids := []string{"run/a", "run@a"}
	for _, id := range ids {
		run := domain.ConnectorRun{ID: id, Source: "github", StartedAt: started, Status: domain.ConnectorSuccess}
		if err := store.AppendConnectorRun(ctx, run); err != nil {
			t.Fatalf("AppendConnectorRun(%q): %v", id, err)
		}
	}

	dir := filepath.Join(store.Root(), connectorsDir, connectorRunsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("colliding ids wrote %d files: %v", len(entries), entries)
	}
	runs, err := store.ListConnectorRuns(ctx, "github", 10)
	if err != nil {
		t.Fatalf("ListConnectorRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
	seen := map[string]bool{runs[0].ID: true, runs[1].ID: true}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("raw id %q missing from %+v", id, runs)
		}
	}
}

func TestConnectorRunListSkipsMalformedFrontmatter(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	dir := filepath.Join(store.Root(), connectorsDir, connectorRunsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	malformed := "---\nid: [unclosed\nkind: connector-run\n---\n\nhuman note\n"
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte(malformed), 0o644); err != nil {
		t.Fatalf("write malformed file: %v", err)
	}
	run := domain.ConnectorRun{
		ID: "run-1", Source: "github", StartedAt: time.Now().UTC(), Status: domain.ConnectorSuccess,
	}
	if err := store.AppendConnectorRun(ctx, run); err != nil {
		t.Fatalf("AppendConnectorRun: %v", err)
	}

	runs, err := store.ListConnectorRuns(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListConnectorRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestConnectorRunListFilterOrderAndLimit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	seed := []domain.ConnectorRun{
		{ID: "run-1", Source: "github", StartedAt: base, Status: domain.ConnectorSuccess},
		{ID: "run-2", Source: "gitlab", StartedAt: base.Add(time.Hour), Status: domain.ConnectorSuccess},
		{ID: "run-3", Source: "github", StartedAt: base.Add(2 * time.Hour), Status: domain.ConnectorFailed},
	}
	for _, run := range seed {
		if err := store.AppendConnectorRun(ctx, run); err != nil {
			t.Fatalf("AppendConnectorRun(%s): %v", run.ID, err)
		}
	}

	runs, err := store.ListConnectorRuns(ctx, "", 2)
	if err != nil {
		t.Fatalf("ListConnectorRuns: %v", err)
	}
	if len(runs) != 2 || runs[0].ID != "run-3" || runs[1].ID != "run-2" {
		t.Fatalf("runs = %+v", runs)
	}

	github, err := store.ListConnectorRuns(ctx, "github", 10)
	if err != nil {
		t.Fatalf("ListConnectorRuns(github): %v", err)
	}
	if len(github) != 2 || github[0].ID != "run-3" {
		t.Fatalf("github runs = %+v", github)
	}

	none, err := store.ListConnectorRuns(ctx, "imap", 10)
	if err != nil || len(none) != 0 {
		t.Fatalf("imap runs = %+v, %v", none, err)
	}
}

func TestConnectorRunSkipsUnrelatedFiles(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	dir := filepath.Join(store.Root(), connectorsDir, connectorRunsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("plain text without frontmatter\n"), 0o644); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}
	run := domain.ConnectorRun{ID: "run-1", Source: "github", StartedAt: time.Now().UTC(), Status: domain.ConnectorSuccess}
	if err := store.AppendConnectorRun(ctx, run); err != nil {
		t.Fatalf("AppendConnectorRun: %v", err)
	}
	runs, err := store.ListConnectorRuns(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListConnectorRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestConnectorRunSanitizesPathParts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	run := domain.ConnectorRun{
		ID: "../../escape", Source: "../../github/../evil",
		StartedAt: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC),
		Status:    domain.ConnectorSuccess,
	}
	if err := store.AppendConnectorRun(ctx, run); err != nil {
		t.Fatalf("AppendConnectorRun: %v", err)
	}
	dir := filepath.Join(store.Root(), connectorsDir, connectorRunsDir)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	name := entries[0].Name()
	if strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		t.Fatalf("unsafe file name %q", name)
	}
}

func TestConnectorRunRejectsInvalidRun(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	err := store.AppendConnectorRun(ctx, domain.ConnectorRun{ID: "run-1", Status: domain.ConnectorSuccess})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty source error = %v", err)
	}
}
