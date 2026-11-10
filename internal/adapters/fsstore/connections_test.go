package fsstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func testConnection(id, account string) domain.Connection {
	return domain.Connection{ID: id, Provider: "github", Account: account, Settings: map[string]string{"repos": "owner/repo"}}
}

func TestConnectionsCRUD(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := NewConnectionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.List(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty list = %v, %v", got, err)
	}
	for _, conn := range []domain.Connection{testConnection("z", "z"), testConnection("a", "a")} {
		if err := store.Create(ctx, conn); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	got.Settings["repos"] = "owner/other"
	got.Schedule = "@daily"
	if err := store.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewConnectionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	list, err := reopened.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "a" || list[1].ID != "z" || list[0].Settings["repos"] != "owner/other" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := store.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("get deleted = %v", err)
	}
	if err := store.Delete(ctx, "a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete missing = %v", err)
	}
	if err := store.Update(ctx, got); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("update missing = %v", err)
	}
}

func TestConnectionPatchMutatesUnderLockAndValidates(t *testing.T) {
	ctx := context.Background()
	store, err := NewConnectionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := testConnection("one", "alice")
	original.Schedule = "@daily"
	if err := store.Create(ctx, original); err != nil {
		t.Fatal(err)
	}
	patched, err := store.Patch(ctx, "one", func(connection *domain.Connection) error {
		connection.Schedule = "@hourly"
		return nil
	})
	if err != nil || patched.Schedule != "@hourly" {
		t.Fatalf("patch = %+v, %v", patched, err)
	}
	stop := errors.New("stop")
	if _, err := store.Patch(ctx, "one", func(connection *domain.Connection) error {
		connection.Schedule = "@every 5m"
		return stop
	}); !errors.Is(err, stop) {
		t.Fatalf("callback error = %v", err)
	}
	stored, err := store.Get(ctx, "one")
	if err != nil || stored.Schedule != "@hourly" {
		t.Fatalf("aborted patch changed the file: %+v, %v", stored, err)
	}
	if _, err := store.Patch(ctx, "missing", func(*domain.Connection) error { return nil }); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing patch = %v", err)
	}
	if _, err := store.Patch(ctx, "one", func(connection *domain.Connection) error {
		connection.ID = "two"
		return nil
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("id change = %v", err)
	}
	if _, err := store.Patch(ctx, "one", func(connection *domain.Connection) error {
		connection.Provider = "gitlab"
		return nil
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("identity change = %v", err)
	}
	if _, err := store.Patch(ctx, "one", nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil mutate = %v", err)
	}
}

func TestConnectionsConcurrentPatchesPreserveDisjointFields(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	seed, err := NewConnectionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Create(ctx, testConnection("one", "alice")); err != nil {
		t.Fatal(err)
	}
	first, err := NewConnectionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewConnectionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := first.Patch(ctx, "one", func(connection *domain.Connection) error {
			connection.Settings = map[string]string{"repos": "owner/other"}
			return nil
		})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := second.Patch(ctx, "one", func(connection *domain.Connection) error {
			connection.Schedule = "@hourly"
			return nil
		})
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	stored, err := seed.Get(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Settings["repos"] != "owner/other" || stored.Schedule != "@hourly" {
		t.Fatalf("lost update: %+v", stored)
	}
}

func TestConnectionsConflictsAndImmutableIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := NewConnectionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := testConnection("one", "Alice")
	original.BaseURL = "https://github.example:443/"
	if err := store.Create(ctx, original); err != nil {
		t.Fatal(err)
	}
	duplicateID := testConnection("one", "Bob")
	if err := store.Create(ctx, duplicateID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("ID conflict = %v", err)
	}
	duplicate := original
	duplicate.ID, duplicate.Account, duplicate.BaseURL = "two", "alice", "https://GITHUB.example"
	if err := store.Create(ctx, duplicate); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("identity conflict = %v", err)
	}
	for _, mutate := range []func(*domain.Connection){
		func(c *domain.Connection) { c.Provider = "gitlab" },
		func(c *domain.Connection) { c.Account = "Bob" },
		func(c *domain.Connection) { c.BaseURL = "https://another.example" },
	} {
		changed := original
		mutate(&changed)
		if err := store.Update(ctx, changed); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("identity update = %v", err)
		}
	}
	original.BaseURL = duplicate.BaseURL
	if err := store.Update(ctx, original); err != nil {
		t.Fatalf("canonical identity update = %v", err)
	}
}

func TestConnectionsFileIsVersionedSortedAndComplete(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := NewConnectionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"z", "a", "m"} {
		if err := store.Create(ctx, testConnection(id, id)); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(root, "connectors", "connections.json"))
		if err != nil {
			t.Fatal(err)
		}
		var entries []struct {
			SchemaVersion int `json:"schemaVersion"`
			domain.Connection
		}
		if err := json.Unmarshal(data, &entries); err != nil {
			t.Fatalf("incomplete JSON: %v", err)
		}
		for i, entry := range entries {
			if entry.SchemaVersion != 1 || entry.Validate() != nil {
				t.Fatalf("invalid entry: %+v", entry)
			}
			if i > 0 && entries[i-1].ID >= entry.ID {
				t.Fatal("not sorted")
			}
		}
	}
	files, err := os.ReadDir(filepath.Join(root, "connectors"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != "connections.json" {
		t.Fatalf("temporary files left: %v", files)
	}
	before, err := os.ReadFile(filepath.Join(root, "connectors", "connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	bad := testConnection("Bad_ID", "new")
	if err := store.Create(ctx, bad); err == nil {
		t.Fatal("accepted invalid connection")
	}
	after, err := os.ReadFile(filepath.Join(root, "connectors", "connections.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("failed write changed file")
	}
}

func TestConnectionsRejectCorruptionWithoutReset(t *testing.T) {
	for _, data := range []string{
		`{broken`, `null`, `{}`, `[{"schemaVersion":2,"id":"a","provider":"github"}]`,
		`[{"schemaVersion":1,"id":"Bad_ID","provider":"github"}]`,
		`[{"schemaVersion":1,"id":"a","provider":"github"},{"schemaVersion":1,"id":"b","provider":"github"}]`,
		`[{"schemaVersion":1,"id":"a","provider":"github","account":"a"},{"schemaVersion":1,"id":"a","provider":"github","account":"b"}]`,
	} {
		t.Run(data, func(t *testing.T) {
			root := t.TempDir()
			store, err := NewConnectionStore(root)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "connectors", "connections.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := store.List(context.Background()); err == nil || !strings.Contains(err.Error(), "connections.json") {
				t.Fatalf("corruption error = %v", err)
			}
			if err := store.Create(context.Background(), testConnection("new", "new")); err == nil {
				t.Fatal("overwrote corruption")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data {
				t.Fatal("corruption was reset")
			}
		})
	}
}

func TestConnectionsConcurrentWritersAndCancellation(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, err := NewConnectionStore(root)
			if err == nil {
				err = store.Create(context.Background(), testConnection(fmt.Sprintf("c-%d", i), fmt.Sprint(i)))
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewConnectionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.List(context.Background())
	if err != nil || len(got) != 8 {
		t.Fatalf("lost write: %d, %v", len(got), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Create(ctx, testConnection("cancelled", "cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
}

func TestConnectionStoreRejectsReservedIDs(t *testing.T) {
	for _, id := range []string{"runs", "con", "nul", "com1"} {
		t.Run(id, func(t *testing.T) {
			// Arrange
			store, err := NewConnectionStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			connection := domain.Connection{ID: id, Provider: "github"}
			// Act
			err = store.Create(context.Background(), connection)
			// Assert
			if err == nil {
				t.Fatalf("Create(%q) = nil, want a reserved-name error", id)
			}
		})
	}
}
