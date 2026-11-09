package fsstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestCheckpointPutGetReplace(t *testing.T) {
	// Arrange
	ctx := context.Background()
	store := newTestStore(t)
	checkpoints := store.Checkpoints()
	first := domain.Checkpoint{Stream: "issues", Version: 1, Data: []byte{0, 1, 2, 0xff}}
	second := domain.Checkpoint{Stream: "issues", Version: 2, Data: []byte(`{"after":"2026-11-09"}`)}

	// Act
	putErr := checkpoints.Put(ctx, "gh-acme", first)
	replaceErr := checkpoints.Put(ctx, "gh-acme", second)
	got, getErr := checkpoints.Get(ctx, "gh-acme", "issues")

	// Assert
	if putErr != nil || replaceErr != nil || getErr != nil {
		t.Fatalf("put %v replace %v get %v", putErr, replaceErr, getErr)
	}
	if !got.Equal(second) {
		t.Fatalf("Get = %+v, want %+v", got, second)
	}
	dir := filepath.Join(store.root, connectorsDir, "gh-acme", checkpointsDir)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || strings.HasPrefix(entries[0].Name(), tempFilePrefix) {
		t.Fatalf("checkpoint dir = %v, %v", entries, err)
	}
}

func TestCheckpointStreamsDifferingOnlyInCaseNeverCollide(t *testing.T) {
	// Arrange
	ctx := context.Background()
	checkpoints := newTestStore(t).Checkpoints()
	upper := domain.Checkpoint{Stream: "INBOX", Version: 1, Data: []byte("upper")}
	lower := domain.Checkpoint{Stream: "inbox", Version: 1, Data: []byte("lower")}

	// Act
	if err := checkpoints.Put(ctx, "mail-corp", upper); err != nil {
		t.Fatal(err)
	}
	if err := checkpoints.Put(ctx, "mail-corp", lower); err != nil {
		t.Fatal(err)
	}
	gotUpper, upperErr := checkpoints.Get(ctx, "mail-corp", "INBOX")
	gotLower, lowerErr := checkpoints.Get(ctx, "mail-corp", "inbox")
	listed, listErr := checkpoints.List(ctx, "mail-corp")

	// Assert
	if upperErr != nil || lowerErr != nil || listErr != nil {
		t.Fatalf("errors: %v %v %v", upperErr, lowerErr, listErr)
	}
	if !bytes.Equal(gotUpper.Data, []byte("upper")) || !bytes.Equal(gotLower.Data, []byte("lower")) {
		t.Fatalf("collision: INBOX=%q inbox=%q", gotUpper.Data, gotLower.Data)
	}
	if len(listed) != 2 || listed[0].Stream != "INBOX" || listed[1].Stream != "inbox" {
		t.Fatalf("List = %+v", listed)
	}
	if checkpointFileName("INBOX") == checkpointFileName("inbox") ||
		!strings.EqualFold(checkpointFileName("INBOX")[:6], checkpointFileName("inbox")[:6]) ||
		checkpointFileName("INBOX") != strings.ToLower(checkpointFileName("INBOX")) {
		t.Fatalf("file names %q %q", checkpointFileName("INBOX"), checkpointFileName("inbox"))
	}
}

func TestCheckpointDeleteOneAndAll(t *testing.T) {
	// Arrange
	ctx := context.Background()
	checkpoints := newTestStore(t).Checkpoints()
	for _, stream := range []string{"issues", "pulls", "commits"} {
		if err := checkpoints.Put(ctx, "gh-acme", domain.Checkpoint{Stream: stream, Version: 1}); err != nil {
			t.Fatal(err)
		}
	}

	// Act
	oneErr := checkpoints.Delete(ctx, "gh-acme", "pulls")
	missingErr := checkpoints.Delete(ctx, "gh-acme", "pulls")
	afterOne, _ := checkpoints.List(ctx, "gh-acme")
	allErr := checkpoints.Delete(ctx, "gh-acme", "")
	afterAll, listErr := checkpoints.List(ctx, "gh-acme")

	// Assert
	if oneErr != nil || missingErr != nil || allErr != nil || listErr != nil {
		t.Fatalf("errors: %v %v %v %v", oneErr, missingErr, allErr, listErr)
	}
	if len(afterOne) != 2 || afterOne[0].Stream != "commits" || afterOne[1].Stream != "issues" {
		t.Fatalf("after one = %+v", afterOne)
	}
	if len(afterAll) != 0 {
		t.Fatalf("after all = %+v", afterAll)
	}
	if _, err := checkpoints.Get(ctx, "gh-acme", "issues"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete err = %v", err)
	}
}

func TestCheckpointValidation(t *testing.T) {
	ctx := context.Background()
	checkpoints := newTestStore(t).Checkpoints()
	cases := []struct {
		connectionID string
		checkpoint   domain.Checkpoint
	}{
		{"gh-acme", domain.Checkpoint{Stream: "../x", Version: 1}},
		{"gh-acme", domain.Checkpoint{Stream: "issues", Version: 0}},
		{"gh-acme", domain.Checkpoint{Stream: "issues", Version: 1, Data: make([]byte, domain.MaxCheckpointBytes+1)}},
		{"../escape", domain.Checkpoint{Stream: "issues", Version: 1}},
	}
	for _, tc := range cases {
		if err := checkpoints.Put(ctx, tc.connectionID, tc.checkpoint); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Put(%q, %q) err = %v", tc.connectionID, tc.checkpoint.Stream, err)
		}
	}
}

func TestCheckpointFailedWriteKeepsOldValue(t *testing.T) {
	// Arrange: a rejected Put never touches the stored checkpoint.
	ctx := context.Background()
	checkpoints := newTestStore(t).Checkpoints()
	old := domain.Checkpoint{Stream: "issues", Version: 1, Data: []byte("old")}
	if err := checkpoints.Put(ctx, "gh-acme", old); err != nil {
		t.Fatal(err)
	}

	// Act
	err := checkpoints.Put(ctx, "gh-acme", domain.Checkpoint{Stream: "issues", Version: 1, Data: make([]byte, domain.MaxCheckpointBytes+1)})
	got, getErr := checkpoints.Get(ctx, "gh-acme", "issues")

	// Assert
	if err == nil || getErr != nil || !got.Equal(old) {
		t.Fatalf("Put err %v, Get %+v %v", err, got, getErr)
	}
}
