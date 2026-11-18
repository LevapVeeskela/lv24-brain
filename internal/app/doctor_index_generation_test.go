package app

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// The index-generation check is report-only: one commit behind is normal, a
// larger gap or a dirty data directory warns, and the message names the
// command that repairs it.
func TestDoctorIndexGenerationWarnsBeyondOneCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	gitTestRun(t, dataDir, "init", "--quiet", "-b", "main")
	gitTestWrite(t, dataDir, "README.md", "data\n")
	gitTestWrite(t, dataDir, ".gitignore", "index/\n")
	gitTestRun(t, dataDir, "add", ".")
	gitTestRun(t, dataDir, "commit", "--quiet", "-m", "initial")

	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutProject(ctx, domain.Project{ID: "p", Name: "P"}); err != nil {
		t.Fatal(err)
	}
	// Commit the registry so the reindex below records a clean tree.
	gitTestRun(t, dataDir, "add", ".")
	gitTestRun(t, dataDir, "commit", "--quiet", "-m", "registry")

	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	application := New(
		config.BrainConfig{Mode: config.ModeLocal, DataDir: dataDir},
		Dependencies{Store: store, Index: index},
	)
	// A loaded machine can exceed gitinfo.Timeout while the reindex records its
	// marker, which would make every later assertion test the timeout instead
	// of the check. Retry until the marker exists.
	reindex := func() {
		for attempt := 0; ; attempt++ {
			if _, err := application.Reindex(ctx, IndexOptions{}); err != nil {
				t.Fatalf("Reindex: %v", err)
			}
			status, err := application.IndexStatus(ctx)
			if err == nil && status.IndexedFrom != "" {
				return
			}
			if attempt == 9 {
				t.Fatalf("git HEAD stayed unreadable during reindex: %+v, %v", status, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	reindex()

	// generationCheck retries the read-only git probe: on a loaded machine it
	// can exceed its timeout, which is not the staleness under test.
	generationCheck := func() DoctorCheck {
		for attempt := 0; ; attempt++ {
			report, err := application.Doctor(ctx, DoctorOptions{})
			if err != nil {
				t.Fatal(err)
			}
			check := DoctorCheck{}
			for _, candidate := range report.Checks {
				if candidate.Name == "index-generation" {
					check = candidate
					break
				}
			}
			if check.Name == "" {
				t.Fatal("doctor has no index-generation check")
			}
			if !strings.Contains(check.Message, "cannot compare") || attempt == 9 {
				return check
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	if got := generationCheck(); got.Status != doctorOK {
		t.Fatalf("fresh index = %+v, want ok", got)
	}

	gitTestWrite(t, dataDir, "README.md", "data v2\n")
	gitTestRun(t, dataDir, "add", ".")
	gitTestRun(t, dataDir, "commit", "--quiet", "-m", "one commit behind")
	if got := generationCheck(); got.Status != doctorOK || !strings.Contains(got.Message, "one commit") {
		t.Fatalf("one commit behind = %+v, want ok", got)
	}

	gitTestWrite(t, dataDir, "README.md", "data v3\n")
	gitTestRun(t, dataDir, "add", ".")
	gitTestRun(t, dataDir, "commit", "--quiet", "-m", "two commits behind")
	got := generationCheck()
	if got.Status != doctorWarn || !strings.Contains(got.Message, "stale at") || !strings.Contains(got.Message, "brain index") {
		t.Fatalf("two commits behind = %+v, want a warn naming brain index", got)
	}

	reindex()
	if got := generationCheck(); got.Status != doctorOK {
		t.Fatalf("reindexed index = %+v, want ok", got)
	}
	gitTestWrite(t, dataDir, "uncommitted.txt", "dirty\n")
	if got := generationCheck(); got.Status != doctorWarn || !strings.Contains(got.Message, "dirty") {
		t.Fatalf("dirty data dir = %+v, want a warn naming the dirty tree", got)
	}
}
