package connectionlock

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestTryLockContendsAndReleaseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	first, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := first.TryLock(context.Background(), "gh-work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.TryLock(context.Background(), "gh-work"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("contending lock error = %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatalf("second release = %v", err)
	}
	next, err := second.TryLock(context.Background(), "gh-work")
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := next(); err != nil {
		t.Fatal(err)
	}
}

func TestNewWritesLockIgnoreFile(t *testing.T) {
	root := t.TempDir()
	if _, err := New(root); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "connectors", ".locks", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != lockIgnoreContent {
		t.Fatalf(".locks/.gitignore = %q", raw)
	}
}

func TestLockArtifactsAreGitIgnored(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	if out, err := exec.Command(git, "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	locker, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := locker.TryLock(context.Background(), "gh-work")
	if err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(git, "-C", root, "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, out)
	}
	if status := strings.TrimSpace(string(out)); status != "" {
		t.Fatalf("lock artifacts are untracked: %s", status)
	}
}

func TestTryLockValidatesConnectionAndHonorsCanceledContext(t *testing.T) {
	locker, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locker.TryLock(context.Background(), "../escape"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid connection error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := locker.TryLock(ctx, "gh-work"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v", err)
	}
}
