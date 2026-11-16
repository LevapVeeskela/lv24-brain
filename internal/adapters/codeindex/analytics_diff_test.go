package codeindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestDiffComparesTwoIndexesOverAGitHistory(t *testing.T) {
	fixture := newFixture(t)
	git(t, fixture.root, "init", "-q", "-b", "main")
	base := gitCommit(t, fixture.root, "base")
	fixture.mustIndex(t)

	ctx := context.Background()
	baseIndex := filepath.Join(t.TempDir(), "base.db")
	if _, err := fixture.index.db.ExecContext(ctx, `VACUUM INTO "`+strings.ReplaceAll(filepath.ToSlash(baseIndex), `"`, `""`)+`"`); err != nil {
		t.Fatalf("snapshot index: %v", err)
	}

	added := filepath.Join(fixture.root, "service", "added.go")
	if err := os.WriteFile(added, []byte(`package service

// AddedOnly exists after the base commit and calls Fetch.
func AddedOnly() string {
	return Fetch(New(), "key")
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	head := gitCommit(t, fixture.root, "add a definition")
	fixture.mustSync(t, "service/added.go")

	view, err := fixture.index.Diff(ctx, "fixture", ports.GraphDiffQuery{
		BaseProject: "base", BaseIndexPath: baseIndex,
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.BaseIndex != baseIndex || view.HeadIndex != fixture.dbPath {
		t.Fatalf("indexes = %+v", view)
	}
	if view.BaseCommit != base || view.HeadCommit != head {
		t.Fatalf("commits = %q..%q, want %q..%q", view.BaseCommit, view.HeadCommit, base, head)
	}
	if view.Files.Added != 1 || view.Files.Removed != 0 {
		t.Fatalf("files = %+v", view.Files)
	}
	if !contains(view.AddedFiles, "service/added.go") {
		t.Fatalf("added files = %v", view.AddedFiles)
	}
	if view.Symbols.Added < 1 || view.Symbols.Removed != 0 {
		t.Fatalf("symbols = %+v", view.Symbols)
	}
	found := false
	for _, symbol := range view.AddedSymbols {
		if symbol.Name == "AddedOnly" {
			found = true
		}
	}
	if !found {
		t.Fatalf("added symbols = %+v", view.AddedSymbols)
	}
	if view.Edges.Added == 0 {
		t.Fatalf("an added definition must bring at least one edge: %+v", view)
	}

	// Repeating the call yields the same counts and samples.
	again, err := fixture.index.Diff(ctx, "fixture", ports.GraphDiffQuery{BaseProject: "base", BaseIndexPath: baseIndex})
	if err != nil {
		t.Fatal(err)
	}
	if again.Files != view.Files || again.Symbols != view.Symbols || again.Edges != view.Edges {
		t.Fatalf("diff is not deterministic: %+v / %+v", view, again)
	}

	// A snapshot of the current store has no delta against itself.
	current := filepath.Join(t.TempDir(), "current.db")
	if _, err := fixture.index.db.ExecContext(ctx, `VACUUM INTO "`+strings.ReplaceAll(filepath.ToSlash(current), `"`, `""`)+`"`); err != nil {
		t.Fatal(err)
	}
	same, err := fixture.index.Diff(ctx, "fixture", ports.GraphDiffQuery{BaseIndexPath: current})
	if err != nil {
		t.Fatal(err)
	}
	if same.Files.Added+same.Files.Removed+same.Symbols.Added+same.Symbols.Removed+same.Edges.Added+same.Edges.Removed != 0 {
		t.Fatalf("a store diffed with a copy of itself is not empty: %+v", same)
	}
	if !notesContain(same.Notes, "same files") {
		t.Fatalf("notes = %v", same.Notes)
	}
}

func TestDiffCapsSamplesAndRefusesBadBase(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	baseIndex := filepath.Join(t.TempDir(), "base.db")
	if _, err := fixture.index.db.ExecContext(ctx, `VACUUM INTO "`+strings.ReplaceAll(filepath.ToSlash(baseIndex), `"`, `""`)+`"`); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		id := "extra_symbol_" + strings.Repeat("x", index+1)
		if _, err := fixture.index.db.ExecContext(ctx,
			`INSERT INTO symbols(id, path, package, kind, name, qualname, start_line, end_line, signature, doc, exported)
			 VALUES(?, 'extra/file.go', 'extra', 'func', ?, ?, 1, 1, '', '', 0)`, id, id, id); err != nil {
			t.Fatal(err)
		}
	}
	view, err := fixture.index.Diff(ctx, "fixture", ports.GraphDiffQuery{BaseIndexPath: baseIndex, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if view.Symbols.Added != 4 || len(view.AddedSymbols) != 2 || !view.Truncated {
		t.Fatalf("symbol cap = %+v", view)
	}
	for _, bad := range []string{"", "relative.db", filepath.Join(t.TempDir(), "missing.db")} {
		if _, err := fixture.index.Diff(ctx, "fixture", ports.GraphDiffQuery{BaseIndexPath: bad}); err == nil {
			t.Fatalf("base %q was accepted", bad)
		}
	}
	if _, err := fixture.index.Diff(ctx, "fixture", ports.GraphDiffQuery{BaseIndexPath: fixture.dbPath}); err == nil {
		t.Fatal("a store was diffed against itself")
	}
}

func TestRebuildCommunitiesSkipsAnUnchangedGraph(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	var before int
	if err := fixture.index.db.QueryRowContext(ctx, `SELECT count(*) FROM communities`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("the fixture produced no community to preserve")
	}
	var digest string
	if err := fixture.index.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaGraphDigest).Scan(&digest); err != nil {
		t.Fatalf("graph digest was not stored: %v", err)
	}
	if digest == "" {
		t.Fatal("graph digest is empty")
	}
	// A sentinel name survives only when the digest fast path skips the
	// rebuild; a recompute would overwrite the row.
	if _, err := fixture.index.db.ExecContext(ctx, `UPDATE communities SET name = 'sentinel'`); err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.index.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildCommunities(ctx, tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var sentinel int
	if err := fixture.index.db.QueryRowContext(ctx, `SELECT count(*) FROM communities WHERE name = 'sentinel'`).Scan(&sentinel); err != nil {
		t.Fatal(err)
	}
	if sentinel == 0 {
		t.Fatal("an unchanged graph recomputed the partition instead of reusing it")
	}

	// A graph change invalidates the digest and rebuilds the names.
	if _, err := fixture.index.db.ExecContext(ctx,
		`INSERT INTO symbols(id, path, package, kind, name, qualname, start_line, end_line, signature, doc, exported)
		 VALUES('sentinel_symbol', 'sentinel/file.go', 'sentinel', 'func', 'Sentinel', 'Sentinel', 1, 1, '', '', 0)`); err != nil {
		t.Fatal(err)
	}
	tx, err = fixture.index.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildCommunities(ctx, tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.index.db.QueryRowContext(ctx, `SELECT count(*) FROM communities WHERE name = 'sentinel'`).Scan(&sentinel); err != nil {
		t.Fatal(err)
	}
	if sentinel != 0 {
		t.Fatal("a changed graph reused the stale partition")
	}
}
