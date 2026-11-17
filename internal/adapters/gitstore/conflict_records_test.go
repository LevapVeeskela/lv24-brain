package gitstore

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/mergedriver"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestStatusReportsConflictRecords(t *testing.T) {
	provider, repo, _ := setup(t)
	ctx := context.Background()
	writeFile(t, repo.dir, "projects/demo/state-conflicts/20261117T060000Z-goal.md", "kept\n")
	writeFile(t, repo.dir, "registry-conflicts/20261117T060000Z-alias.md", "kept\n")

	status, err := provider.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Dirty {
		t.Fatal("status.Dirty = false with untracked conflict records")
	}
	if len(status.Conflicts) != 0 {
		t.Fatalf("unmerged conflicts = %v, want none (records are informational)", status.Conflicts)
	}
	for _, want := range []string{
		"projects/demo/state-conflicts/20261117T060000Z-goal.md",
		"registry-conflicts/20261117T060000Z-alias.md",
	} {
		if !containsString(status.ConflictRecords, want) {
			t.Fatalf("ConflictRecords = %v, want %q", status.ConflictRecords, want)
		}
	}
}

func TestCommitConflictRecordsStagesOnlyRecords(t *testing.T) {
	provider, repo, _ := setup(t)
	ctx := context.Background()
	record := "projects/demo/state-conflicts/20261117T060000Z-goal.md"
	writeFile(t, repo.dir, record, "kept\n")
	writeFile(t, repo.dir, "projects/demo/unrelated.md", "do not stage\n")

	paths, changed, err := provider.CommitConflictRecords(ctx, "brain sync: conflict records")
	if err != nil {
		t.Fatalf("CommitConflictRecords: %v", err)
	}
	if !changed || len(paths) != 1 || paths[0] != record {
		t.Fatalf("paths=%v changed=%v, want [%s] true", paths, changed, record)
	}
	// The record is now tracked and clean; the unrelated file stays dirty.
	if log := repo.run("log", "-1", "--format=%s"); !strings.Contains(log, "conflict records") {
		t.Fatalf("commit subject = %q", log)
	}
	tracked := repo.run("ls-files", "--", record)
	if strings.TrimSpace(tracked) != record {
		t.Fatalf("record not tracked: %q", tracked)
	}
	unrelated := repo.run("ls-files", "--", "projects/demo/unrelated.md")
	if strings.TrimSpace(unrelated) != "" {
		t.Fatalf("unrelated file was staged: %q", unrelated)
	}
	_, changed, err = provider.CommitConflictRecords(ctx, "brain sync: conflict records")
	if err != nil || changed {
		t.Fatalf("second CommitConflictRecords changed=%v err=%v, want no-op", changed, err)
	}
}

func TestDivergentItemSiblingIsCommitted(t *testing.T) {
	provider, repo, _ := setup(t)
	ctx := context.Background()
	sibling := "connectors/work/items/abc-0123456789ab.md"
	writeFile(t, repo.dir, sibling, "incoming item\n")

	status, err := provider.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(status.ConflictRecords, sibling) {
		t.Fatalf("ConflictRecords = %v, want %q", status.ConflictRecords, sibling)
	}
	paths, changed, err := provider.CommitConflictRecords(ctx, "brain sync: item conflict")
	if err != nil || !changed || len(paths) != 1 || paths[0] != sibling {
		t.Fatalf("paths=%v changed=%v err=%v", paths, changed, err)
	}
	if tracked := repo.run("ls-files", "--", sibling); strings.TrimSpace(tracked) != sibling {
		t.Fatalf("sibling not committed: %q", tracked)
	}
}

func TestVerifySyncLayoutRejectsNestedDataDir(t *testing.T) {
	requireGit(t)
	root := filepath.Join(t.TempDir(), "repo")
	repo := initRepo(t, root, false)
	nested := filepath.Join(root, "data")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	repo.run("config", "--local", "core.safecrlf", "false")

	rootProvider := newTestProvider(t, root)
	if err := rootProvider.VerifySyncLayout(context.Background()); err != nil {
		t.Fatalf("root layout: %v", err)
	}
	nestedProvider := newTestProvider(t, nested)
	if err := nestedProvider.VerifySyncLayout(context.Background()); err == nil {
		t.Fatal("nested data directory was accepted")
	}
}

func TestMergeDriverIncludesItems(t *testing.T) {
	var found bool
	for _, driver := range MergeDrivers() {
		if driver.Name == "lv24-items" {
			found = true
		}
	}
	if !found {
		t.Fatal("lv24-items is not a managed merge driver")
	}
	if got := driverCommand("lv24-items"); !strings.HasPrefix(got, "items %O %A %B") {
		t.Fatalf("driverCommand = %q", got)
	}
}

// TestMergeDriverItemsHelper is re-executed by git as the lv24-items driver in
// the two-clone test below. It must not run as an ordinary test: the guard
// returns immediately unless the helper environment is set.
func TestMergeDriverItemsHelper(t *testing.T) {
	if os.Getenv("LV24_ITEMS_DRIVER_HELPER") != "1" {
		return
	}
	args := flag.Args()
	if len(args) < 6 || args[0] != "items" {
		os.Exit(2)
	}
	base, _ := os.ReadFile(args[1])
	ours, _ := os.ReadFile(args[2])
	theirs, _ := os.ReadFile(args[3])
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		os.Exit(1)
	}
	root := strings.TrimSpace(string(out))
	result, err := mergedriver.MergeItems(base, ours, theirs, mergedriver.Options{Path: args[5], OutputDir: root})
	if err != nil {
		os.Exit(1)
	}
	if err := os.WriteFile(args[2], result.Merged, 0o644); err != nil {
		os.Exit(1)
	}
	for _, conflict := range result.Conflicts {
		if _, err := mergedriver.WriteConflict(filepath.Join(root, "merge-conflicts"), conflict, time.Now()); err != nil {
			os.Exit(1)
		}
	}
	os.Exit(0)
}

func registerItemsDriver(t *testing.T, repo *testRepo) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf("%q -test.run=TestMergeDriverItemsHelper -- items %%O %%A %%B %%L %%P", filepath.ToSlash(exe))
	repo.run("config", "--local", "merge.lv24-items.driver", command)
}

func TestSharedRawZoneTwoClones(t *testing.T) {
	provider, repo, other := setup(t)
	t.Setenv("LV24_ITEMS_DRIVER_HELPER", "1")
	ctx := context.Background()

	attributes := "connectors/*/items/*.md merge=lv24-items\n"
	itemPath := "connectors/work/items/abc.md"
	item := func(observed string) string {
		front := "---\nschema_version: 1\nref:\n  connection_id: \"work\"\n  external_id: \"a\"\nhash: \"sha256:abc\"\n"
		if observed != "" {
			front += "observed_at: \"" + observed + "\"\n"
		}
		return front + "---\nshared body\n"
	}

	registerItemsDriver(t, repo)
	registerItemsDriver(t, other)
	// A common baseline, present on both clones before either writes.
	writeFile(t, repo.dir, ".gitattributes", attributes)
	writeFile(t, repo.dir, itemPath, item(""))
	if _, err := provider.Commit(ctx, "baseline: attributes and item"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Push(ctx); err != nil {
		t.Fatal(err)
	}
	other.run("pull", "--quiet", "--rebase")
	if got := readFile(t, other.dir, ".gitattributes"); !strings.Contains(got, "merge=lv24-items") {
		t.Fatalf("B did not receive the attributes:\n%s", got)
	}
	if got := other.run("check-attr", "merge", "--", itemPath); !strings.Contains(got, "lv24-items") {
		t.Fatalf("B does not route the item to the driver: %q", got)
	}

	// Both hosts ingest the same item from the baseline with a host-specific
	// observed_at. Their commits diverge; B's rebase must invoke the driver.
	writeFile(t, repo.dir, itemPath, item("2026-11-17T06:00:00Z"))
	if _, err := provider.Commit(ctx, "A: shared item"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Push(ctx); err != nil {
		t.Fatal(err)
	}
	writeFile(t, other.dir, itemPath, item("2026-11-17T07:00:00Z"))
	other.run("add", "--all")
	other.run("commit", "--quiet", "-m", "B: same item, different host clock")
	other.run("pull", "--quiet", "--rebase")

	merged := readFile(t, other.dir, itemPath)
	if strings.Contains(merged, "observed_at") {
		t.Fatalf("merged item still has a host clock:\n%s", merged)
	}
	if !strings.Contains(merged, "shared body") {
		t.Fatalf("merged item lost the body:\n%s", merged)
	}
	other.run("push", "--quiet", "origin", "main")

	result, err := provider.Pull(ctx, ports.SyncOptions{Autostash: true})
	if err != nil {
		t.Fatalf("Pull after B published the merged item: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("conflicts = %v, want none", result.Conflicts)
	}
	if stashes := repo.run("stash", "list"); strings.TrimSpace(stashes) != "" {
		t.Fatalf("stash left after merge:\n%s", stashes)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
