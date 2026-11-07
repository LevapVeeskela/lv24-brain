package fsstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// withShortLockTimings shortens the lock timeout for one test.
func withShortLockTimings(t *testing.T, timeout time.Duration) {
	t.Helper()
	oldTimeout := fileLockTimeout
	fileLockTimeout = timeout
	t.Cleanup(func() { fileLockTimeout = oldTimeout })
}

func TestFileLockExcludesOtherHolderUntilReleased(t *testing.T) {
	withShortLockTimings(t, 150*time.Millisecond)
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "2026-11-07.md")

	release, err := acquireFileLock(ctx, target)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := os.Stat(target + lockFileSuffix); err != nil {
		t.Fatalf("lock file missing while held: %v", err)
	}
	if _, err := acquireFileLock(ctx, target); !errors.Is(err, errLockTimeout) {
		t.Fatalf("second acquire while held: err = %v, want errLockTimeout", err)
	}
	release()
	if _, err := os.Stat(target + lockFileSuffix); !os.IsNotExist(err) {
		t.Fatalf("lock file after release: err = %v, want not exist", err)
	}
	release2, err := acquireFileLock(ctx, target)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

func TestFileLockBreaksStaleLock(t *testing.T) {
	withShortLockTimings(t, 150*time.Millisecond)
	target := filepath.Join(t.TempDir(), "state.md")
	lockPath := target + lockFileSuffix
	if err := os.WriteFile(lockPath, []byte("pid 999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * fileLockStaleAfter)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}

	release, err := acquireFileLock(context.Background(), target)
	if err != nil {
		t.Fatalf("acquire over stale lock: %v", err)
	}
	release()
}

func TestFileLockKeepsFreshForeignLock(t *testing.T) {
	withShortLockTimings(t, 150*time.Millisecond)
	target := filepath.Join(t.TempDir(), "registry.json")
	lockPath := target + lockFileSuffix
	if err := os.WriteFile(lockPath, []byte("pid 999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := acquireFileLock(context.Background(), target); !errors.Is(err, errLockTimeout) {
		t.Fatalf("acquire over fresh lock: err = %v, want errLockTimeout", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("fresh foreign lock was removed: %v", err)
	}
}

func TestFileLockHonorsContextCancellation(t *testing.T) {
	withShortLockTimings(t, 10*time.Second)
	target := filepath.Join(t.TempDir(), "2026-11-07.md")
	release, err := acquireFileLock(context.Background(), target)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := acquireFileLock(ctx, target); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire with cancelled context: err = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestWithFileLockReleasesWhenFunctionFails(t *testing.T) {
	target := filepath.Join(t.TempDir(), "2026-11-07.md")
	wantErr := errors.New("boom")
	err := withFileLock(context.Background(), target, func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("withFileLock err = %v, want %v", err, wantErr)
	}
	if _, statErr := os.Stat(target + lockFileSuffix); !os.IsNotExist(statErr) {
		t.Fatalf("lock left behind after failure: %v", statErr)
	}
}

func TestAppendNoteConcurrentGoroutinesKeepEveryNote(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	ctx := context.Background()
	const writers = 24
	created := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.AppendNote(ctx, domain.Note{
				ProjectID: project.ID,
				Title:     fmt.Sprintf("goroutine-%02d", i),
				Body:      fmt.Sprintf("body %d", i),
				CreatedAt: created,
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AppendNote: %v", err)
	}
	assertNoteTitles(t, store, project.ID, "goroutine-", writers)
}

const (
	lockHelperEnv    = "FSSTORE_TEST_APPEND_HELPER"
	lockHelperDir    = "FSSTORE_TEST_DIR"
	lockHelperPrefix = "FSSTORE_TEST_PREFIX"
	lockHelperCount  = "FSSTORE_TEST_COUNT"
)

// TestAppendNoteHelperProcess is not a test on its own. It runs in a child
// process started by TestAppendNoteAcrossProcessesKeepEveryNote and appends
// notes to the same daily file as its siblings.
func TestAppendNoteHelperProcess(t *testing.T) {
	if os.Getenv(lockHelperEnv) != "1" {
		t.Skip("helper process for TestAppendNoteAcrossProcessesKeepEveryNote")
	}
	count, err := strconv.Atoi(os.Getenv(lockHelperCount))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	store, err := New(os.Getenv(lockHelperDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	prefix := os.Getenv(lockHelperPrefix)
	created := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		_, err := store.AppendNote(context.Background(), domain.Note{
			ProjectID: "lv24-brain",
			Title:     fmt.Sprintf("%s-%03d", prefix, i),
			Body:      fmt.Sprintf("%s body %d", prefix, i),
			CreatedAt: created,
		})
		if err != nil {
			t.Fatalf("AppendNote %s #%d: %v", prefix, i, err)
		}
	}
}

func TestAppendNoteAcrossProcessesKeepEveryNote(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns helper processes")
	}
	store := newTestStore(t)
	project := seedProject(t, store)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	const processes = 4
	const perProcess = 20
	type child struct {
		cmd *exec.Cmd
		out *bytes.Buffer
	}
	children := make([]child, 0, processes)
	for p := 0; p < processes; p++ {
		out := &bytes.Buffer{}
		cmd := exec.Command(exe, "-test.run=^TestAppendNoteHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			lockHelperEnv+"=1",
			lockHelperDir+"="+store.Root(),
			fmt.Sprintf("%s=p%d", lockHelperPrefix, p),
			fmt.Sprintf("%s=%d", lockHelperCount, perProcess),
		)
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper %d: %v", p, err)
		}
		children = append(children, child{cmd: cmd, out: out})
	}
	for p, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("helper %d failed: %v\n%s", p, err, c.out.String())
		}
	}

	notes, err := store.ListNotes(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	if len(notes) != processes*perProcess {
		t.Fatalf("notes = %d, want %d (lost updates)", len(notes), processes*perProcess)
	}
	for p := 0; p < processes; p++ {
		assertNoteTitles(t, store, project.ID, fmt.Sprintf("p%d-", p), perProcess)
	}
}

// assertNoteTitles checks that exactly want notes with titles starting with
// prefix are listed, all of them distinct.
func assertNoteTitles(t *testing.T, store *Store, projectID, prefix string, want int) {
	t.Helper()
	notes, err := store.ListNotes(context.Background(), projectID)
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	seen := make(map[string]int)
	for _, note := range notes {
		if len(note.Title) >= len(prefix) && note.Title[:len(prefix)] == prefix {
			seen[note.Title]++
		}
	}
	if len(seen) != want {
		t.Fatalf("notes with prefix %q = %d distinct, want %d", prefix, len(seen), want)
	}
	for title, n := range seen {
		if n != 1 {
			t.Fatalf("note %q listed %d times", title, n)
		}
	}
}
