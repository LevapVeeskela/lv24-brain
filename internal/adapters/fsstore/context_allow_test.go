package fsstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func contextAllowStore(t *testing.T, dataDir string) *ContextAllowStore {
	t.Helper()
	store, err := NewContextAllowStore(dataDir)
	if err != nil {
		t.Fatalf("NewContextAllowStore(%q): %v", dataDir, err)
	}
	return store
}

func contextAllowWrite(t *testing.T, dataDir, body string) string {
	t.Helper()
	path := filepath.Join(dataDir, ContextAllowPath)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", ContextAllowPath, err)
	}
	return path
}

func TestContextAllowStoreMissingFile(t *testing.T) {
	dir := t.TempDir()
	grants, err := contextAllowStore(t, dir).ContextGrants(context.Background())
	if err != nil {
		t.Fatalf("ContextGrants: %v", err)
	}
	if grants.SchemaVersion != domain.ContextAllowSchemaVersion || len(grants.Grants) != 0 {
		t.Fatalf("grants = %+v, want schema %d with no grants", grants, domain.ContextAllowSchemaVersion)
	}
	if _, err := os.Stat(filepath.Join(dir, ContextAllowPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file was created: stat error = %v", err)
	}
}

func TestContextAllowStoreLoadsAndNormalizes(t *testing.T) {
	dir := t.TempDir()
	body := `{
  "schemaVersion": 1,
  "grants": [
    {
      "project": " lv24 ",
      "connection": "github-main",
      "kinds": ["Issue", " issue ", "PULL-REQUEST"],
      "maxAgeDays": 30,
      "maxExcerpts": 2
    },
    {
      "project": "brain",
      "connection": "mail-main",
      "kinds": []
    }
  ]
}`
	path := contextAllowWrite(t, dir, body)
	grants, err := contextAllowStore(t, dir).ContextGrants(context.Background())
	if err != nil {
		t.Fatalf("ContextGrants: %v", err)
	}
	if grants.SchemaVersion != domain.ContextAllowSchemaVersion {
		t.Fatalf("schemaVersion = %d, want %d", grants.SchemaVersion, domain.ContextAllowSchemaVersion)
	}
	if len(grants.Grants) != 2 {
		t.Fatalf("grants = %+v, want 2", grants.Grants)
	}
	first := grants.Grants[0]
	wantKinds := []domain.ItemKind{domain.ItemIssue, domain.ItemPullRequest}
	if first.Project != "lv24" || first.Connection != "github-main" ||
		!slices.Equal(first.Kinds, wantKinds) || first.MaxAgeDays != 30 || first.MaxExcerpts != 2 {
		t.Fatalf("first grant = %+v, want project lv24, connection github-main, kinds %v, maxAgeDays 30, maxExcerpts 2", first, wantKinds)
	}
	second := grants.Grants[1]
	if second.MaxExcerpts != domain.MaxContextExcerpts {
		t.Fatalf("second maxExcerpts = %d, want %d", second.MaxExcerpts, domain.MaxContextExcerpts)
	}
	if len(second.Kinds) != 0 || second.MaxAgeDays != 0 {
		t.Fatalf("second grant = %+v, want no kinds and no age filter", second)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back %s: %v", ContextAllowPath, err)
	}
	if string(after) != body {
		t.Fatalf("%s was rewritten by a read", ContextAllowPath)
	}
}

func TestContextAllowStoreRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantValidation bool
	}{
		{
			name: "unknown field",
			body: `{"schemaVersion":1,"grants":[{"project":"lv24","connection":"github-main","unknown":true}]}`,
		},
		{
			name: "trailing document",
			body: `{"schemaVersion":1}{"schemaVersion":1}`,
		},
		{
			name:           "missing schema version",
			body:           `{"grants":[]}`,
			wantValidation: true,
		},
		{
			name:           "wrong schema version",
			body:           `{"schemaVersion":2}`,
			wantValidation: true,
		},
		{
			name: "duplicate project and connection",
			body: `{"schemaVersion":1,"grants":[
				{"project":"lv24","connection":"github-main"},
				{"project":"lv24","connection":"github-main"}
			]}`,
			wantValidation: true,
		},
		{
			name:           "invalid connection id",
			body:           `{"schemaVersion":1,"grants":[{"project":"lv24","connection":"GitHub Main"}]}`,
			wantValidation: true,
		},
		{
			name:           "invalid item kind",
			body:           `{"schemaVersion":1,"grants":[{"project":"lv24","connection":"github-main","kinds":["ticket"]}]}`,
			wantValidation: true,
		},
		{
			name:           "zero max excerpts",
			body:           `{"schemaVersion":1,"grants":[{"project":"lv24","connection":"github-main","maxExcerpts":0}]}`,
			wantValidation: true,
		},
		{
			name:           "too many max excerpts",
			body:           `{"schemaVersion":1,"grants":[{"project":"lv24","connection":"github-main","maxExcerpts":6}]}`,
			wantValidation: true,
		},
		{
			name:           "negative max age days",
			body:           `{"schemaVersion":1,"grants":[{"project":"lv24","connection":"github-main","maxAgeDays":-1}]}`,
			wantValidation: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			contextAllowWrite(t, dir, test.body)
			grants, err := contextAllowStore(t, dir).ContextGrants(context.Background())
			if err == nil {
				t.Fatalf("ContextGrants = %+v, want an error", grants)
			}
			if !strings.Contains(err.Error(), ContextAllowPath) {
				t.Fatalf("error = %v, want mention of %s", err, ContextAllowPath)
			}
			if test.wantValidation && !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("error = %v, want domain.ErrValidation", err)
			}
		})
	}
}

func TestContextAllowStoreRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	body := `{"schemaVersion":1}` + strings.Repeat(" ", maxContextAllowBytes)
	contextAllowWrite(t, dir, body)
	grants, err := contextAllowStore(t, dir).ContextGrants(context.Background())
	if err == nil {
		t.Fatalf("ContextGrants = %+v, want an error", grants)
	}
	if !strings.Contains(err.Error(), ContextAllowPath) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want a size error mentioning %s", err, ContextAllowPath)
	}
}

// contextAllowCancelAfter reports context.Canceled from its second Err call:
// entry is allowed, the check after the file read is not.
type contextAllowCancelAfter struct {
	context.Context
	errCalls int
}

func (c *contextAllowCancelAfter) Err() error {
	c.errCalls++
	if c.errCalls > 1 {
		return context.Canceled
	}
	return nil
}

func TestContextAllowStoreHonorsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	contextAllowWrite(t, dir, `{"schemaVersion":1}`)
	store := contextAllowStore(t, dir)

	t.Run("before read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		grants, err := store.ContextGrants(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ContextGrants = %+v, %v, want context.Canceled", grants, err)
		}
	})

	t.Run("after read", func(t *testing.T) {
		grants, err := store.ContextGrants(&contextAllowCancelAfter{Context: context.Background()})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ContextGrants = %+v, %v, want context.Canceled", grants, err)
		}
	})
}

func TestContextAllowStoreRejectsEmptyDataDir(t *testing.T) {
	for _, dataDir := range []string{"", "   "} {
		store, err := NewContextAllowStore(dataDir)
		if store != nil || err == nil {
			t.Fatalf("NewContextAllowStore(%q) = %v, %v; want nil store and error", dataDir, store, err)
		}
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("NewContextAllowStore(%q) error = %v, want domain.ErrValidation", dataDir, err)
		}
	}
}
